# Frame & window chrome

By default the main window is *frameless*: `RunWindow` strips the OS title bar and your React frontend draws the chrome (the title band and the window buttons), while movement, resizing and the buttons drive the real native window through bound JS functions. This page covers that contract - frameless mode, the title-bar button toggles, the invisible hit-test metrics that make drawn chrome behave like a real caption, the bound bridge functions, and the `useShell()` surface that wraps them. For the React component that renders the chrome, see [The TitleBar](../ui/titlebar.md); for the plain field list, see [Window options](window-options.md).

![A frameless window: a top caption band drags the window except for a small left reserve and a right reserve holding the window buttons; the rest is HTCLIENT, and a thin resize margin around every edge starts a native resize.](window-chrome-zones.svg)

## Frameless vs framed

### Framed `bool` (default false)

`false` (the default) is frameless: RunWindow clears `WS_CAPTION` from the window style (a `WM_NCCALCSIZE` handler then makes the client area cover the whole window, killing the native frame strip) and your frontend draws the chrome. Set `true` to keep the ordinary OS title bar and frame and skip the custom chrome entirely - handy for a quick tool window where you do not want to build a TitleBar. When framed, the `<prefix>ResizeEdge` binding is not added (the OS frame handles edges) and `Caps().frameless` reports `false`.

## The title-bar button toggles

Which window buttons exist is configured once, in Go. Each toggle does two things: it flips the real native window style, and it flips the matching flag in `Caps()`, so the [TitleBar](../ui/titlebar.md) renders exactly the buttons the window actually supports - no manual mirroring of Go options into React props. The `caps()` mapping on the frontend is `props.showMinimize ?? caps?.minimize ?? false` (and likewise maximize/close), so an unset prop defers to the window and a plain browser tab (all caps false) shows no window buttons at all.

### DisableMinimize `bool` (default false)

Removes minimize: RunWindow clears `WS_MINIMIZEBOX` from the window style, does not bind `<prefix>Minimize`, and `Caps().minimize` returns `false` so the TitleBar hides its minimize button.

### EnableMaximize `bool` (default false)

Maximize is OFF by default - a deliberate Gantry choice, since many tool windows should resize but never fill the screen (the same default Timekeep ships). Setting it `true` binds `<prefix>Maximize`, `<prefix>Restore` and `<prefix>IsMaximized`, keeps the native maximize box, and makes `Caps().maximize` report `true` so the TitleBar shows the button. When it is `false`, RunWindow also clears `WS_MAXIMIZEBOX`, which additionally stops a caption double-click from maximizing. A frameless window that does maximize is clamped to the monitor *work area* (in `WM_GETMINMAXINFO`) so it never fills over the taskbar.

### DisableClose `bool` (default false)

The window ignores close requests entirely (the subclass drops `WM_CLOSE`); `Caps().close` reports `false` so the TitleBar hides its close button. Only `appshell.CloseMainWindow()` - which tray Quit uses - can still close it. To intercept close rather than forbid it, use `OnCloseRequest` instead (see [Close behavior and app lifecycle](close-and-lifecycle.md)).

## The custom-chrome hit-test metrics

Because the WebView2 child window covers the whole client area, the parent's `WM_NCHITTEST` never sees the mouse on its own - the web page gets the mousedown. Four metrics (all device pixels) shape the invisible native hit-test that, together with the frontend's drag strip and resize frame, makes the drawn chrome behave like a real title bar. Their defaults match gantry-web's TitleBar defaults, so normally you touch neither side. If you change one here, change its twin on the [TitleBar](../ui/titlebar.md) props - they are two halves of one hit-test, and a mismatch shows up as "the close button drags the window" or "this strip of page cannot be clicked".

| Metric | `WindowOptions` field | TitleBar prop | Default | What it does |
| --- | --- | --- | --- | --- |
| Caption height | `CaptionHeight` | `height` | 40 | Height of the top band that drags the window. |
| Left reserve | `CaptionLeftReserve` | `leftReserve` | 8 | Dead zone on the left that stays clickable (does not drag) - bump it, e.g. 90, when you put buttons in the left slot. |
| Right reserve | `CaptionRightReserve` | `rightReserve` | 150 | Dead zone on the right where the window buttons live, so clicking them never starts a drag. |
| Resize margin | `ResizeMargin` | (native only) | 6 | Edge thickness that shows resize cursors and starts a native resize, in all eight directions. |

In `WM_NCHITTEST` (frameless, and only while not zoomed) the subclass returns the eight edge/corner codes within `ResizeMargin` of any edge, then returns `HTCAPTION` for the empty middle of the top band (`y < top + CaptionHeight`, and `x` between `left + CaptionLeftReserve` and `right - CaptionRightReserve`), and `HTCLIENT` everywhere else. The centered title is pointer-transparent, so it drags too.

## The bound bridge functions

RunWindow binds these JS functions on the window (prefix `gantry` by default, set by [`BindingPrefix`](window-options.md#bindingprefix-string-default-gantry)). Some are conditional:

- `gantryClose`, `gantryDrag`, `gantryAttention`, `gantryOpenExternal`, `gantrySetAlwaysOnTop`, `gantryCaps` - always bound. Note `gantrySetAlwaysOnTop` is bound unconditionally, regardless of the `AlwaysOnTop` field, so the frontend can pin or unpin at any time.
- `gantryMinimize` - bound unless `DisableMinimize`.
- `gantryMaximize`, `gantryRestore`, `gantryIsMaximized` - bound only when `EnableMaximize`.
- `gantryResizeEdge` - bound only when frameless (`!Framed`).
- `gantryDrag` releases Chromium's mouse capture and posts a caption click (`WM_NCLBUTTONDOWN` with `HTCAPTION`); the native move loop then takes over, snapping included.
- `gantryResizeEdge` maps an edge name (`"n"`, `"se"`, ...) to its hit-test code and posts `WM_NCLBUTTONDOWN` with it, starting the native interactive resize. It is actively used on Windows (gantry-web's `ResizeFrame` draws eight invisible edge strips that carry the resize cursors and call it on mousedown, because the child window swallows the parent's `WM_NCHITTEST` margin) and on Linux (where it hands off to the compositor). The native hit-test margin remains only as a backstop.
- `gantryOpenExternal` opens a URL in the default browser (gantry-web's `ExternalLink` uses it so external links never navigate the app window).

The show-state verbs (`gantryMinimize`, `gantryMaximize`, `gantryRestore`) use `ShowWindowAsync`, never `ShowWindow` - a synchronous show-state change would re-enter the WndProc mid-dispatch and crash (see [Win32 notes](../advanced/win32-notes.md)).

RunWindow also disables the WebView2 status bar (the bottom-corner URL-preview bubble), a browser artifact that looks wrong in a desktop app.

## The `useShell()` surface

You will not call the raw bindings - gantry-web's `getShell()` (and its React hook `useShell()`) wraps them with feature detection, so the same frontend also runs in a plain browser tab where they simply do not exist. Each method is safe to call anywhere: outside a native window it just does nothing, and `shell.available` tells you which world you are in (it is `true` when the `Minimize` or `Close` binding exists).

- `shell.available` - `boolean`, `true` inside a Gantry native window.
- `shell.close()`, `shell.minimize()`, `shell.maximize()`, `shell.restore()` - window verbs.
- `shell.isMaximized()` - a `Promise<boolean>` for the current maximized state.
- `shell.drag()` - start the native window-move loop (call on mousedown from a custom caption).
- `shell.attention()` - system notification sound plus taskbar flash (see [Notifications](notifications.md)).
- `shell.caps()` - a `Promise<ShellCaps>` reporting `{minimize, maximize, close, alwaysOnTop, platform, frameless}` - which buttons the Go side enabled, so the TitleBar renders exactly those (all false in a browser).
- `shell.setAlwaysOnTop(on)` - pin or unpin the window above others.
- `shell.openExternal(url)` - open a URL in the user's default browser, never in the app window.
- `shell.resizeEdge(edge)` - start an interactive resize from an edge (`"n"`, `"se"`, ...) on frameless windows.

The React hooks add two conveniences: `useShell(prefix)` memoizes the bridge for a component, and `useShellCaps(prefix)` resolves the caps once (returning `null` while resolving, so you render nothing rather than flicker the buttons, then the caps - all false in a plain browser).

`shell.setVisible(show)` and `shell.resize(w, h)` exist in the surface too, but their native bindings live on [widgets](widgets.md) and [popups](notifications.md), not the main window - the main window has no `Visible`/`Resize` binding, so calling them there is a no-op.

## Linux

The same options drive a GTK window on Linux: frameless (undecorated), native drag, minimize/maximize/close, min/max sizes, always-on-top, the close hook and geometry persistence all work. Edge resizing comes from the frontend there too - gantry-web renders the same invisible `ResizeFrame` strips, which start the compositor's interactive resize via the `ResizeEdge` binding. Positioning and saved geometry are best-effort under pure Wayland (X11/XWayland honor them), and `Corners` is Windows-only.

## Related pages

- [Window options](window-options.md) - the full `WindowOptions` field reference.
- [The TitleBar](../ui/titlebar.md) - the React component that draws the chrome, its props, and replacing it.
- [Close behavior and app lifecycle](close-and-lifecycle.md) - `OnCloseRequest`, `DisableClose`, and `appshell.App`.
- [Win32 notes](../advanced/win32-notes.md) - the WndProc subclass and why bindings post messages instead of sending them.
