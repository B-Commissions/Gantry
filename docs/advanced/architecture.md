# Architecture

How a Gantry app actually runs: the processes it spawns, the transports between them, and the build pipeline that turns source into a single binary. This page is the mental model; the [wire protocol](protocol.md), [Windows internals](win32-notes.md) and [manual wiring](without-the-cli.md) pages drill into the pieces.

![One binary runs the main process (a loopback HTTP server and a native webview), spawns a child process per widget and popup, and connects the webview to Go over a one-way bridge and the /gantry/ws websocket.](architecture-processes.svg)

## The process shape

```
myapp.exe (main process)
  |- local HTTP server on 127.0.0.1:<port>  (appshell.Listen)
  |    |- /             the embedded webdist/ build (or the Vite dev server in dev)
  |    |- /gantry/ws    the ui websocket (App.Handler)
  |    |- /gantry/widgets.json, /gantry/notify/action  framework endpoints
  |    |- /resources/   the embedded resources/ tree (when present)
  |    |- /api/...       your own endpoints, if any
  |- native main window (WebView2 on Windows, WebKitGTK on Linux) pointed at that server
  |- tray icon (optional)
  |- ProcManager children, each re-invoking this same exe:
       |- myapp.exe --shellrole popup --url ... --monitor N --position bottom
       |- myapp.exe --shellrole widget-timer --port N   (each widget: own process)
```

One binary plays every role. `gantry.Run` parses flags and checks `--shellrole` first: when set it runs exactly one helper window (`appshell.RunPopup` for the built-in `popup` role, or a `Config.Roles[name]` function) and returns, never starting the server. Helper windows load their pages from the MAIN process's server over HTTP, so all app logic stays in one place - the children are pure renderers. `RoleArgs` (`Port`, `URL`, `Monitor`, `Position`) carries the standard child flags into a custom role.

## Why serve HTTP at all?

The webview needs a URL, and serving the SPA locally buys several things at once: one code path for the native window, `--browser` mode, and dev (only the URL the window loads changes); a websocket that works naturally (the ui layer needs server push); and a fixed port that doubles as the single-instance guard - `appshell.Listen(port)` binding `127.0.0.1:<port>` is what makes a second launch fail. `--port 0` binds an ephemeral port instead (the real one is read back off the listener and printed, or announced with `--announce-ready` as `GANTRY_READY port=N`). The server binds `127.0.0.1` only, so nothing is reachable from the network. `Config.Setup(app, mux)` is where your own routes join the same mux; the SPA catch-all (`appshell.ServeSPA`) is registered last on `/` so real asset paths serve files and everything else falls back to `index.html` - see [Serving your own HTTP routes](../ui/http-endpoints.md).

## Why child processes for widgets and popups?

Two reasons. Crash isolation: a renderer crash kills one widget, never the app, and teardown is just a `Process.Kill()` - no half-dead window states, no orphaned webviews. And a hard WebView2 constraint: two webviews in different processes cannot share a browser user-data folder, so the second environment fails to initialize. Gantry keys each folder by app plus role (`%LocalAppData%\<app>\webview-<role>` via `webviewDataPath`), which is the concrete reason each role runs as its own process. `ProcManager` (`appshell/procman.go`) owns the child table with `Show`/`Toggle`/`Replace`/`Kill`/`CloseAll`, reaps each child so it never zombies, and hides the spawn console (`CREATE_NO_WINDOW`). See [Win32 notes](win32-notes.md) for the folder-per-role detail.

## The two transports

A Gantry window talks to Go over two separate channels, each suited to a different job:

- **The bridge** (`window.<prefix>*` functions, prefix `gantry` by default): tiny, window-scoped controls the Go side binds directly onto the webview - `gantryClose`, `gantryMinimize`, `gantryDrag`, `gantryMaximize`/`gantryRestore`/`gantryIsMaximized`, `gantrySetAlwaysOnTop`, `gantryResizeEdge`, `gantryAttention`, `gantryOpenExternal`, and `gantryCaps` (which window buttons this window actually supports). It works with zero setup and is injected per window, but it is one-way: a binding is a JS-initiated call into Go, Go cannot push through it. `gantry-web`'s `getShell()`/`useShell()` wrap it, and in a plain browser tab every method is a safe no-op (`available` is false).
- **The websocket** (`/gantry/ws`, `App.Handler`): everything app-level - paired events, pushes, Tea renders, awaited calls, shared state. One connection per client, it survives reloads by reconnecting, and the server treats the newest connection as THE client (older ones are detached and closed). Read-only `?observer=1` taps ride alongside without displacing the real client - that is how the test driver watches the protocol while the webview drives the UI.

Rule of thumb: window verbs go over the bridge, app data goes over the socket. The full frame-by-frame grammar is in the [protocol](protocol.md) page.

## The build pipeline

`gantry build` runs `prepareApp` (regenerate the `.gantry/` Vite root and the generated Go files, then `vite build`, which bundles every **page**, **component**, **layout**, **colocated css** and **gantry-web** itself into `webdist/`), then `go build` per target, where `embed.go`'s `//go:embed all:webdist` bakes `webdist/` into the exe. The result is a single file - Go binary plus frontend - written to `dist/<os>/<arch>/`. Note the two names: `webdist/` is the embedded frontend, `dist/` is where the release binaries land.

In dev the pipeline inverts: `gantry dev` runs `vite dev` for HMR and `go run . --dev-url http://localhost:<vite-port>` beside it, so the native window loads the Vite URL and Vite proxies `/api` and `/gantry/ws` back to the Go port. The Go and Vite processes run as separate child groups, so a `.go` save restarts only the Go half (the socket re-announces its page on reconnect) while Vite's HMR keeps handling `.tsx`/`.css`. Either way the app code is identical - only the URL the window loads changes. To own this pipeline yourself, see [Without the CLI](without-the-cli.md).

## Platforms

Windows (WebView2 + raw Win32) and Linux (WebKitGTK + GTK3) both get the full shell: frameless custom chrome, native drag, window buttons, min/max sizes, always-on-top, the close hook (allow/cancel/hide), geometry persistence, widgets, popups, monitors and the tray. Frameless edge resizing works the same on both now: `gantry-web`'s `ResizeFrame` renders eight invisible fixed-position strips that carry the resize cursors and call the `ResizeEdge` bridge binding on mousedown, because the webview child covers the client area and swallows any native hit-test hover. On Windows that hands off to a posted `WM_NCLBUTTONDOWN` with the edge's hit-test code (`resizeWindow`), on Linux to the compositor's interactive resize. The Win32 backend still keeps a `WM_NCHITTEST` resize margin in its subclass as a backstop (see [Win32 notes](win32-notes.md)).

Linux caveats: window positioning and saved geometry are best-effort under pure Wayland (the protocol does not let apps place themselves; X11 and XWayland honor it), corner rounding belongs to the compositor, and the tray needs a shell with a status-icon host (most desktops; not WSLg).

Mac is future work (no hardware to test against yet): apps still run there via the browser fallback, and `OpenInBrowser` already knows "open". The `nogui` build tag (`-tags nogui`) strips the native window on every platform - useful for CI and headless servers - and `--no-open` serves without opening any window at all. On Linux the shell links `webkit2gtk-4.1` by default; the `webkit2gtk40` tag selects the legacy `webkit2gtk-4.0` name for distros that never shipped 4.1 (Debian 11, Ubuntu 20.04). The build-tag split lives inside appshell (`*_windows.go`, `*_linux.go`, `*_fallback.go`); app code does not branch.
