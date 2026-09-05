# Pairs: the paired-file model

A Gantry feature is a **pair**: a `.tsx` file and the `.go` file next to it, talking over one websocket. Pages and components are both pairs - they share this whole mechanism and differ only in what makes one a page and the other a component. This page is the pairing system itself: the key that ties the halves together, the four things `usePaired()` gives the tsx side, how data flows both ways, and how a pair registers itself. Everything here is identical whether the pair is a [page](pages.md), a [component](components.md), or shared chrome under [layouts/](layouts.md).

![A pair's tsx and go halves share a key and talk over one websocket: send fires a one-way handler, call awaits a result, and Go pushes events and state back to the tsx side.](pairs-flow.svg)

## Keys: how the halves find each other

Every pair is identified by its **folder path relative to the app root**: `pages/index`, `pages/settings`, `components/example`. The Go half declares it in the `Key` field; the tsx half gets the same string injected by the Gantry Vite plugin from the file's location, so only one side ever writes it out.

```go
// pages/index/index.go
var Page = ui.Page{Key: "pages/index"}
```

```tsx
// pages/index/index.tsx
const { send, call, on, state } = usePaired(); // key injected at build time
```

Call `usePaired()` with **no argument** inside `pages/` or `components/` and the plugin fills the key in. Anywhere else (a shared hook, a file outside the app root) the plugin cannot know which pair you mean, so pass the key explicitly - `usePaired("pages/index")` - or the hook throws with a message telling you to. The key is always the folder path, and the CSS scope class follows it (a pair at `pages/account/settings/profile` scopes under `gantry-pages-account-settings-profile` - see [Styling](styling.md)). Pages, components and layouts all nest to any depth and key the same way.

## What usePaired() returns

`usePaired()` hands back exactly four members - the whole surface of a pair:

- **`send(event, payload?)`** - fire a named [handler](#data-flow-tsx---go) on the Go half. One-way, no return value.
- **`call(name, payload?)`** - `await` a named function on the Go half and get its result back. This is the [Awaited Go calls](calls.md) topic; the mechanism is a pair-scoped `ui.Calls`.
- **`on(event, fn)`** - subscribe to a named [push](#data-flow-go---tsx) from Go. Returns an unsubscribe function, so it drops straight into a `useEffect`.
- **`state`** - the latest payload Go pushed under the event name `"state"`, kept in React state for you. The one-liner for mirroring Go data into a component.

`send`, `call` and `on` are stable across renders (memoized on the key), so they are safe as `useEffect` dependencies.

## Data flow: tsx -> Go

`send` fires a named handler with any JSON-serializable payload:

```tsx
send("save", { name: "jack" });
```

```go
On: ui.Handlers{
    "save": func(p json.RawMessage) {
        var body struct{ Name string `json:"name"` }
        _ = json.Unmarshal(p, &body)
        // ...
    },
},
```

`ui.Handlers` is `map[string]func(payload json.RawMessage)`. Handlers run **inline on the websocket read loop**, so do quick work directly and spawn a goroutine for anything slow. A panicking handler does not take the app down - Gantry recovers it, logs `%s.%s handler panicked`, and reports it as a `panic.event` [error](../advanced/errors.md). This channel is one-way and fire-and-forget: nothing comes back to the caller. When the tsx needs an ANSWER, or the functionality is app-wide (auth, settings), or the state lives in Go, reach for [Awaited Go calls](calls.md) and [Services & hooks](services.md).

## Data flow: Go -> tsx

`App.Push(key, event, payload)` sends a named event to a pair's tsx side:

```go
app.Push("components/example", "state", currentStats)
```

The tsx receives it two ways:

- **`state`**: a push named `"state"` lands in `usePaired().state` automatically - the one-liner for mirroring Go data into a component.
- **`on`**: subscribe to any event name, and unsubscribe by returning the result straight from an effect:

```tsx
useEffect(() => on("progress", (p) => setProgress(p as number)), [on]);
```

`Push` fans out to every connected client. A desktop app has exactly one real client at a time (a reload or dev restart replaces it, and the new client re-announces its page), so `Push` is fire-and-forget - a push with no client attached is silently dropped rather than queued.

## Registration is automatic

Adding a new pair is two steps:

1. Make the folder and files: `pages/stats/stats.go` + `stats.tsx` (+ `stats.css` if you want styles).
2. In `stats.go`: `package stats`, and export `var Page = ui.Page{...}` (or `var Component = ui.Component{...}`).

That is it. The frontend discovers the tsx by its location (the Vite plugin). The Go side is generated: `gantry dev`/`build` walk `pages/`, `components/` and `layouts/` for any folder whose `.go` file declares `var Page =` or `var Component =`, and write `gantry_registry.go` - a `gantryPairs()` function that `main.go` consumes as `ui.NewApp(gantryPairs()...)`. Run `gantry gen` to regenerate it by hand (for example before a plain `go build`); never edit the generated file. During `gantry dev`, adding a folder hot-reloads the frontend, but the Go side needs a dev restart because Go recompiles.

## Two styles, mixed freely

- **Paired handlers (plain style)**: the UI is React through and through, Go is the backend. Familiar if you come from web dev; state lives in the browser. This is `send`/`on`/`state` plus `ui.Handlers`.
- **Tea Model**: the UI logic and state live in Go, React renders the tree. One language for the whole feature, state that survives frontend reloads, and everything trivially testable in Go. See [The Tea model](tea-model.md).

Mix them per pair, or use both on one pair.

## Notes

- **Nothing crashes on a bad key.** An unregistered handler logs `ui: no handler for pages/index.foo (check the Key in your Page/Component and that main.go registers it)` on the Go side; an unresolved `ui.Custom()` name renders a visible `[unknown component: x]` placeholder rather than failing silently.
- **Where pairs go from here.** A pair becomes a routable [page](pages.md) when its Go half is a `ui.Page`, a reusable [component](components.md) when it is a `ui.Component`, and shared chrome when it lives under [layouts/](layouts.md). The mechanism above is identical across all three - only the Go type and a couple of page-only fields differ.
