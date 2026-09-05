// One websocket connects the frontend to the Go ui.App: Tea renders
// come down, events go up, paired pushes fan out. Reconnects with
// backoff (webview reloads, dev server restarts, HMR) and re-announces
// the active page on every connect so the server always re-renders.

/** GantryCallError rejects a failed callGo with the gerr code the Go
 * side attached ("panic.call", or the code of the returned error), so
 * callers can switch on it. */
export class GantryCallError extends Error {
  code?: string;
  constructor(message: string, code?: string) {
    super(message);
    this.name = "GantryCallError";
    this.code = code;
  }
}

/** The (single) consumer of server {"t":"error"} frames; errors.ts
 * subscribes. Registered lazily to keep this module UI-free. */
let errorListener: ((p: unknown) => void) | null = null;
export function onServerError(fn: (p: unknown) => void): void {
  errorListener = fn;
}

export type WireNode = {
  type: string;
  key?: string;
  props?: Record<string, unknown>;
  handlers?: Record<string, string>;
  children?: WireNode[];
};

type RenderListener = (tree: WireNode) => void;
type PushListener = (event: string, payload: unknown) => void;
type StateListener = () => void;

let ws: WebSocket | null = null;
let url = "";
let activePage = "";
// The active page's captured route params, re-sent on every (re)connect
// alongside the page key so the Go side can read them. Normalized to
// arrays on the wire: a [id] value is a single-element array, a [...slug]
// catch-all keeps all its segments.
let activeParams: Record<string, string[]> = {};
let backoff = 300;
let renderListener: RenderListener | null = null;
const pushListeners = new Map<string, Set<PushListener>>();
const sendQueue: string[] = [];

// Awaited calls: id -> resolver, with a timeout so a dead server
// cannot leak promises forever.
let nextCallId = 0;
const pending = new Map<string, { resolve: (v: unknown) => void; reject: (e: Error) => void; timer: number }>();

// Shared Go state (useGoState): the latest value per key + subscribers.
const stateValues = new Map<string, unknown>();
const stateListeners = new Map<string, Set<StateListener>>();

function defaultURL(): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return proto + "//" + location.host + "/gantry/ws";
}

/** connect opens (or reuses) the app socket. createApp calls this. */
export function connect(customURL?: string): void {
  if (ws) return;
  url = customURL ?? url ?? "";
  if (!url) url = defaultURL();
  open();
}

function open(): void {
  const sock = new WebSocket(url);
  ws = sock;
  sock.onopen = () => {
    backoff = 300;
    if (activePage) sock.send(JSON.stringify({ t: "ready", page: activePage, params: activeParams }));
    while (sendQueue.length > 0) sock.send(sendQueue.shift() as string);
  };
  sock.onmessage = (e) => {
    let msg: {
      t: string;
      tree?: WireNode;
      key?: string;
      name?: string;
      p?: unknown;
      id?: string;
      ok?: boolean;
      err?: string;
      code?: string;
    };
    try {
      msg = JSON.parse(e.data as string);
    } catch {
      return;
    }
    if (msg.t === "render" && msg.tree) {
      renderListener?.(msg.tree);
    } else if (msg.t === "push" && msg.key && msg.name) {
      pushListeners.get(msg.key)?.forEach((fn) => fn(msg.name as string, msg.p));
    } else if (msg.t === "reply" && msg.id) {
      const p = pending.get(msg.id);
      if (p) {
        pending.delete(msg.id);
        clearTimeout(p.timer);
        if (msg.ok) p.resolve(msg.p);
        else p.reject(new GantryCallError(msg.err ?? "call failed", msg.code));
      }
    } else if (msg.t === "state" && msg.key) {
      stateValues.set(msg.key, msg.p);
      stateListeners.get(msg.key)?.forEach((fn) => fn());
    } else if (msg.t === "error") {
      errorListener?.(msg.p);
    }
  };
  sock.onclose = () => {
    if (ws !== sock) return; // superseded
    ws = null;
    setTimeout(() => {
      if (!ws) open();
    }, backoff);
    backoff = Math.min(backoff * 2, 5000);
  };
  sock.onerror = () => sock.close();
}

function send(obj: unknown): void {
  const data = JSON.stringify(obj);
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(data);
  } else {
    sendQueue.push(data);
    connect();
  }
}

/** ready announces the mounted page (and its captured route params); the
 * server activates its Model and records the params for the Go half. */
export function ready(pageKey: string, params?: Record<string, string | string[]>): void {
  activePage = pageKey;
  activeParams = normalizeParams(params);
  send({ t: "ready", page: pageKey, params: activeParams });
}

// normalizeParams puts every value in array form so the Go side sees one
// stable shape (map[string][]string): a [id] becomes ["7"], a [...slug]
// keeps its segments.
function normalizeParams(params?: Record<string, string | string[]>): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const [k, v] of Object.entries(params ?? {})) {
    out[k] = Array.isArray(v) ? v : [v];
  }
  return out;
}

/** sendTeaEvent fires a Tea handler by its render-generation id. */
export function sendTeaEvent(handlerId: string, payload?: unknown): void {
  send({ t: "event", h: handlerId, p: payload });
}

/** sendPaired fires a named event on a page/component's Go half. */
export function sendPaired(key: string, name: string, payload?: unknown): void {
  send({ t: "event", key, name, p: payload });
}

/** onRender subscribes the (single) Tea tree consumer. */
export function onRender(fn: RenderListener | null): void {
  renderListener = fn;
}

/** onPush subscribes to pushes for one paired key; returns unsubscribe. */
export function onPush(key: string, fn: PushListener): () => void {
  let set = pushListeners.get(key);
  if (!set) {
    set = new Set();
    pushListeners.set(key, set);
  }
  set.add(fn);
  return () => {
    set.delete(fn);
    if (set.size === 0) pushListeners.delete(key);
  };
}

/** callGo awaits a Go function: a pair's Call entry or a service's. */
export function callGo(key: string, name: string, payload?: unknown, timeoutMs = 30_000): Promise<unknown> {
  const id = "c" + ++nextCallId;
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(() => {
      pending.delete(id);
      reject(new Error(`call ${key}.${name} timed out`));
    }, timeoutMs);
    pending.set(id, { resolve, reject, timer });
    send({ t: "call", key, name, id, p: payload });
  });
}

/** getGoState reads the latest synced value for a state key. */
export function getGoState(key: string): unknown {
  return stateValues.get(key);
}

/** hasGoState reports whether the server has sent this key yet. */
export function hasGoState(key: string): boolean {
  return stateValues.has(key);
}

/** setGoState applies a local write and sends it to Go (no echo). */
export function setGoState(key: string, value: unknown): void {
  stateValues.set(key, value);
  stateListeners.get(key)?.forEach((fn) => fn());
  send({ t: "setstate", key, p: value });
}

/** onGoState subscribes to changes of one state key. */
export function onGoState(key: string, fn: StateListener): () => void {
  let set = stateListeners.get(key);
  if (!set) {
    set = new Set();
    stateListeners.set(key, set);
  }
  set.add(fn);
  return () => {
    set.delete(fn);
    if (set.size === 0) stateListeners.delete(key);
  };
}
