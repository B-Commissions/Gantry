/// <reference path="../types/index.d.ts" />
import { Component, StrictMode, useEffect, type ErrorInfo as ReactErrorInfo, type FC, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { TitleBar, type TitleBarProps } from "./TitleBar";
import { ResizeFrame } from "./ResizeFrame";
import { installZoomGuard } from "./zoom";
import { connect, ready, callGo } from "./socket";
import { useRoute, redirect, ParamsContext, type RouteParams } from "./router";
import { setRegistry, type ComponentRegistry, type TeaComponentProps } from "./tea/Runtime";
import { installErrorHandlers, reportError, useGantryErrors, dismissNotice, clearFatal, setDevMode, type ErrorHandlingOptions, type GantryErrorInfo } from "./errors";
import { ErrorScreen, type ErrorScreenProps } from "./ErrorScreen";
import { fetchEnv, useMode } from "./env";

/** The shape of a page module (a pages/<name>/<name>.tsx file). */
export interface GantryPageModule {
  default: FC;
  /** export const chrome = false to hide the TitleBar on this page. */
  chrome?: boolean;
  /**
   * Which layouts/ wrap this page: a name ("compact"), several nested
   * outermost-first (["main", "compact"]), false for none, true to
   * force "main" even on a chromeless page. Default: "main" if it
   * exists (chromeless pages default to none).
   */
  layout?: boolean | string | string[];
  /** export const route = "/custom" to override the derived route. */
  route?: string;
}

export interface GantryPage {
  key: string;
  route: string;
  mod: GantryPageModule;
}

/**
 * The generated virtual:gantry-app module: the page/component/layout
 * registry the Vite plugin derives from the app's folders. The
 * synthesized .gantry/main.tsx imports it and hands it to createApp.
 * gantry-web itself must never import the virtual module: esbuild
 * cannot resolve virtual ids, so that import would force the package
 * out of dep prebundling - and serving it as raw node_modules source
 * lets Vite double-instantiate modules (?v=hash vs bare URLs), which
 * split the router's subscriber set and broke navigation.
 */
export interface GantryAppModule {
  pages: GantryPage[];
  components: Record<string, { default: FC }>;
  /** Layouts by short name: layouts/main/main.tsx -> "main". */
  layouts: Record<string, { default: FC<{ children?: ReactNode }> }>;
  /** The root app.tsx module (default export: CreateAppOptions), or null. */
  appConfig: { default: CreateAppOptions } | null;
  singlePage?: boolean;
}

// The registry handed to createApp; module-level so match/layoutsFor
// read it without threading props.
let reg: GantryAppModule;

export interface CreateAppOptions {
  /** Title shown in the TitleBar (default: none). */
  title?: ReactNode;
  /**
   * TitleBar customization: title placement (titleAlign), your own
   * controls in the left/right slots (back/forward buttons, menus),
   * heights and reserves. The usual home for this is the optional
   * app.tsx at the app root, default-exporting CreateAppOptions.
   */
  titleBar?: Partial<TitleBarProps>;
  /** Extra Tea components beyond the paired components/ folders. */
  components?: ComponentRegistry;
  /** Binding prefix when the Go side changed it from "gantry". */
  prefix?: string;
  /** Websocket URL override (default ws://<host>/gantry/ws). */
  socketURL?: string;
  /** Hide the TitleBar on every page (pages can also export chrome = false). */
  chrome?: boolean;
  /**
   * Crash detection and interception - on by default. A React render
   * crash shows a full-screen error (full detail in development, a
   * friendly card in production) instead of a white screen; Go panics
   * and unhandled JS errors show dismissible notices in development.
   * Replace the UI (screen), intercept (onError), or turn it off.
   */
  errors?: ErrorHandlingOptions & { screen?: FC<ErrorScreenProps> };
}

function keyClass(key: string): string {
  return "gantry-" + key.replace(/[^a-zA-Z0-9]+/g, "-");
}

function routeOf(p: GantryPage): string {
  return p.mod.route ?? p.route;
}

// A route compiles to typed segments: a literal must match exactly, a
// [param] captures one segment, a [...rest] catch-all captures one or
// more trailing segments.
type Seg =
  | { kind: "lit"; value: string }
  | { kind: "param"; name: string }
  | { kind: "catchall"; name: string };

function routeIsDynamic(route: string): boolean {
  return route.includes("[");
}

function cleanPath(path: string): string {
  return path !== "/" && path.endsWith("/") ? path.slice(0, -1) : path;
}

function splitPath(path: string): string[] {
  return path === "/" ? [] : path.replace(/^\//, "").split("/");
}

function compileRoute(route: string): Seg[] {
  return splitPath(route).map((s): Seg => {
    const cm = s.match(/^\[\.\.\.(.+)\]$/);
    if (cm) return { kind: "catchall", name: cm[1] };
    const pm = s.match(/^\[(.+)\]$/);
    if (pm) return { kind: "param", name: pm[1] };
    return { kind: "lit", value: s };
  });
}

// specificity ranks candidate dynamic routes so the most concrete wins:
// literals beat params beat catch-alls, segment by segment.
function specificity(segs: Seg[]): number {
  let score = 0;
  for (const s of segs) {
    score += s.kind === "lit" ? 100 : s.kind === "param" ? 10 : 1;
  }
  return score;
}

function matchSegs(segs: Seg[], parts: string[]): RouteParams | null {
  const params: RouteParams = {};
  for (let i = 0; i < segs.length; i++) {
    const s = segs[i];
    if (s.kind === "catchall") {
      const rest = parts.slice(i);
      if (rest.length === 0) return null; // [...x] needs at least one segment
      params[s.name] = rest;
      return params;
    }
    if (i >= parts.length || parts[i] === "") return null;
    if (s.kind === "param") {
      params[s.name] = parts[i];
    } else if (parts[i] !== s.value) {
      return null;
    }
  }
  return parts.length === segs.length ? params : null;
}

export interface RouteMatch {
  page: GantryPage;
  params: RouteParams;
}

/**
 * match resolves a pathname to a page. An exact static route wins; then
 * dynamic routes are tried most-specific first ([id] and [...slug]).
 * Returns null when nothing matches - the caller falls back to the index.
 */
function match(path: string): RouteMatch | null {
  const clean = cleanPath(path);
  const exact = reg.pages.find((p) => !routeIsDynamic(routeOf(p)) && routeOf(p) === clean);
  if (exact) return { page: exact, params: {} };

  const parts = splitPath(clean);
  const dynamic = reg.pages
    .filter((p) => routeIsDynamic(routeOf(p)))
    .map((p) => ({ p, segs: compileRoute(routeOf(p)) }))
    .sort((a, b) => specificity(b.segs) - specificity(a.segs));
  for (const { p, segs } of dynamic) {
    const params = matchSegs(segs, parts);
    if (params) return { page: p, params };
  }
  return null;
}

function PageHost({ page, params }: { page: GantryPage; params: RouteParams }) {
  // Re-announce when the page key OR the concrete params change: the same
  // dynamic page key ("pages/.../[id]") is reused across /1, /2, ... so a
  // param change is still a navigation the Go side must hear about.
  const paramsKey = JSON.stringify(params);
  useEffect(() => {
    ready(page.key, params);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page.key, paramsKey]);
  const Page = page.mod.default;
  return (
    <ParamsContext.Provider value={params}>
      <div className={"gantry-page " + keyClass(page.key)}>
        <Page />
      </div>
    </ParamsContext.Provider>
  );
}

// layoutsFor resolves a page's layout selection to component functions,
// outermost first.
//
//   (nothing)            -> "main" if it exists (chromeless pages skip)
//   layout = false       -> none
//   layout = "compact"   -> that one
//   layout = ["main","compact"] -> nested: <Main><Compact><Page/>...
function layoutsFor(page: GantryPage, chrome: boolean): FC<{ children?: ReactNode }>[] {
  const sel = page.mod.layout;
  let names: string[];
  if (sel === false) {
    names = [];
  } else if (typeof sel === "string") {
    names = [sel];
  } else if (Array.isArray(sel)) {
    names = sel;
  } else if (sel === true) {
    names = reg.layouts.main ? ["main"] : [];
  } else {
    // Default: the "main" layout on normal pages; chromeless pages
    // (widgets, popups) are their own surfaces and skip it.
    names = chrome && reg.layouts.main ? ["main"] : [];
  }
  const out: FC<{ children?: ReactNode }>[] = [];
  for (const n of names) {
    const mod = reg.layouts[n];
    if (mod?.default) {
      out.push(mod.default);
    } else {
      console.warn(`gantry: page ${page.key} wants unknown layout "${n}" (have: ${Object.keys(reg.layouts).join(", ") || "none"})`);
    }
  }
  return out;
}

// ErrorBoundary catches render crashes below it. It wraps the page
// content only - ResizeFrame and TitleBar stay outside, so a crashed
// page keeps its drag and close controls.
class ErrorBoundary extends Component<{ children: ReactNode }, { crashed: boolean }> {
  state = { crashed: false };
  static getDerivedStateFromError() {
    return { crashed: true };
  }
  componentDidCatch(error: Error, info: ReactErrorInfo) {
    reportError({
      kind: "react-render",
      code: "react.render",
      message: error.message,
      stack: error.stack,
      componentStack: info.componentStack ?? undefined,
      time: new Date().toISOString(),
      origin: "js",
    });
  }
  render() {
    // The fatal overlay renders from the store (ErrorHost); the crashed
    // subtree just goes away.
    return this.state.crashed ? null : this.props.children;
  }
}

// ErrorHost renders the (default or custom) error UI from the store:
// the fatal overlay and the notice banners.
function ErrorHost({ options }: { options: CreateAppOptions }) {
  const { fatal, notices } = useGantryErrors();
  const mode = useMode();
  if (options.errors?.enabled === false) return null;
  const Screen = options.errors?.screen ?? ErrorScreen;
  return (
    <>
      {fatal && <Screen error={fatal} mode={mode} variant="fatal" onDismiss={clearFatal} />}
      {notices.map((e, i) => (
        // Newest first: the CSS shows only the first banner, dismissing
        // it reveals the next.
        <Screen key={i} error={e} mode={mode} variant="notice" onDismiss={() => dismissNotice(i)} />
      )).reverse()}
    </>
  );
}

function AppRoot({ options }: { options: CreateAppOptions }) {
  const path = useRoute();
  const result = match(path);
  const indexPage = reg.pages.find((p) => routeOf(p) === "/");
  // A URL that matches no page (deleted during dev, stale bookmark) falls
  // back to the index page - normalize the address so Links light up. A
  // dynamic match is NOT a fallback: its URL is already canonical, so we
  // must never redirect it back to the [id] pattern.
  const matched = result !== null;
  useEffect(() => {
    if (!matched && indexPage) {
      redirect("/");
    }
  }, [matched, indexPage]);
  const page = result?.page ?? indexPage;
  if (!page) {
    return <div className="gantry-app">No pages found - add pages/index/index.tsx</div>;
  }
  const params = result?.params ?? {};
  const chrome = options.chrome !== false && page.mod.chrome !== false;
  let content = <PageHost page={page} params={params} />;
  for (const Layout of layoutsFor(page, chrome).reverse()) {
    content = <Layout>{content}</Layout>;
  }
  // The boundary wraps the page and layouts only: a render crash takes
  // the content down, never the window chrome - the error overlay
  // appears and the TitleBar keeps drag/close working.
  return (
    <div className="gantry-app">
      <ResizeFrame prefix={options.prefix} />
      {chrome && <TitleBar title={options.title} prefix={options.prefix} {...options.titleBar} />}
      <ErrorBoundary>{content}</ErrorBoundary>
      <ErrorHost options={options} />
    </div>
  );
}

/**
 * createApp boots a Gantry frontend: zoom guard, websocket, component
 * registry, TitleBar, the root layout (layout.tsx at the app root, if
 * present), and the page router (single-page mode when only pages/index
 * exists). The synthesized .gantry/main.tsx calls this with the
 * virtual:gantry-app module (`import * as app from "virtual:gantry-app"`);
 * an app only touches it to pass options.
 */
export function createApp(app: GantryAppModule, options: CreateAppOptions = {}): void {
  reg = app;
  // The optional root app.tsx wins over the synthesized defaults, so
  // apps customize everything without owning the entry file.
  if (reg.appConfig?.default) {
    options = { ...options, ...reg.appConfig.default };
  }
  installZoomGuard();
  installErrorHandlers(options.errors ?? {});
  connect(options.socketURL);
  // Resolve the mode for the error UI (dev shows full detail), and
  // surface a crash from the previous run if Go recorded one.
  if (options.errors?.enabled !== false) {
    fetchEnv()
      .then((env) => setDevMode(env.mode === "development"))
      .catch(() => {});
    callGo("gantry", "errors")
      .then((list) => {
        const errs = (list ?? []) as (Omit<GantryErrorInfo, "origin"> & { kind: string })[];
        const crash = errs.filter((e) => e.kind === "process-crash").at(-1);
        if (crash) reportError({ ...crash, origin: "go" });
      })
      .catch(() => {});
  }

  const registry: ComponentRegistry = {};
  for (const [key, mod] of Object.entries(reg.components)) {
    if (typeof mod.default === "function") {
      registry[key] = mod.default as FC<TeaComponentProps>;
    }
  }
  Object.assign(registry, options.components);
  setRegistry(registry);

  const container = document.getElementById("root");
  if (!container) throw new Error("gantry: no #root element");
  createRoot(container).render(
    <StrictMode>
      <AppRoot options={options} />
    </StrictMode>,
  );
}
