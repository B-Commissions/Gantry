# Close behavior and app lifecycle

What happens when the user clicks the close button is a decision, not a default. This page covers the close hook (`OnCloseRequest`) and the standard `appshell.App` lifecycle in full, then the runtime toggle, single-instance guard and shutdown as advanced material.

![On a close request OnCloseRequest returns Allow, Cancel, or Hide: Cancel keeps the window open, Hide moves it to the tray where Show restores it, and Allow with no tray or tray Quit exits the app.](lifecycle-states.svg)

## OnCloseRequest: intercepting close

Set `OnCloseRequest` on `WindowOptions` and it runs first on every close request - the X button, Alt+F4, a `WM_CLOSE` from anywhere - and its `appshell.CloseAction` return decides the fate of that request:

```go
appshell.WindowOptions{
    // ...
    OnCloseRequest: func() appshell.CloseAction {
        return appshell.CloseHide // this app hides instead of quitting
    },
}
```

The three return values (from `appshell/options.go`):

- `appshell.CloseAllow` (the zero value, and what a `nil` hook means) - proceed: the window is destroyed and RunWindow returns.
- `appshell.CloseCancel` - swallow the request; the window stays open. Use it for "you have unsaved work" flows: return `CloseCancel`, tell the frontend to show its confirm dialog, and call `appshell.CloseMainWindow()` if the user confirms.
- `appshell.CloseHide` - hide the window and keep the app running. The window still exists with all its state; `appshell.ShowMainWindow()` brings it back instantly.

The hook is the natural place to hand off to something else: hide the window and fire a notification popup ("still running in the tray"), show a widget, whatever fits. One rule: it runs on the window's own thread, so do not block in it - kick real work onto a goroutine.

Two paths bypass the hook by design:

- `appshell.CloseMainWindow()` force-closes: it sets the internal `forceClose` flag and destroys the window without consulting the hook or `DisableClose`. Tray Quit uses this, so Quit always works no matter what the hook says.
- The `DisableClose` field (see [Frame & window chrome](window-chrome.md#disableclose-bool-default-false)) drops close requests entirely, without a hook.

Window geometry saves on any path that actually closes or hides (the subclass calls the geometry store from both `onClosing` and `onResize`), so `CloseHide` apps keep their position and size too.

## appshell.App: the standard lifecycle

Most apps do not call `RunWindow` directly; they fill in `appshell.App` and call `Run`. `Run` blocks the main goroutine (the native window requires it) until `ctx` is done. Its fields:

- `Window WindowOptions` - the main window options, exactly as above.
- `Tray *tray.Options` - when non-nil, shows a [tray icon](tray.md). App wraps your `Menu`: it prepends an "Open <Title>" item (unless `TrayOnly`), appends a "Quit" item, and wires Quit to close the window and cancel `ctx`.
- `TrayOnly bool` - skip the main window entirely; the app lives in the tray and shows only widgets and popups (see [Tray-only apps](tray.md#advanced-tray-only-apps)).
- `Browser bool` - open the URL in the default browser instead of a native window (the `--browser` flag).
- `OnWindowClosed func()` - runs each time the main window closes while the app keeps running in the tray.
- `KeepRunning func() bool` - on a tray app, consulted at each close: return `false` and that close quits the whole app, tray and all. `nil` keeps the default (a tray app stays running). Because it is read at close time, the value it reports can be flipped at runtime.

```go
shell := &appshell.App{
    Window: appshell.WindowOptions{ /* ... */ },
    Tray: &tray.Options{
        Icon:    appicon.ICO(appicon.Render(32, appicon.DefaultPalette())),
        Tooltip: "Myapp is running",
    },
}
return shell.Run(ctx, cancel)
```

What `Run` does:

- opens the window on launch and re-opens it from the tray;
- runs the tray on its own goroutine, with "Open <Title>" prepended and "Quit" appended around your `Menu`; Quit calls `CloseMainWindow()` then `cancel()`;
- does the minimize-to-tray dance: with a tray, closing the window leaves the app running (`continue`s the loop and waits for the tray to re-open it); without one, closing the window calls `cancel()` and the app exits;
- falls back to the default browser when `RunWindow` reports the native webview is unavailable, and honors `Browser` mode (`--browser`), which skips the window entirely;
- honors `TrayOnly`: no main window at all - the app is its tray icon, widgets and popups.

Reopening from the tray first tries `ShowMainWindow()` (instant if the window was hidden by `CloseHide` or just minimized), and only opens a fresh window if none is alive - so it composes cleanly with `CloseHide`.

## Two ways to "close to tray"

- **Window-per-open** (the Timekeep model, and `App.Run`'s default with a tray): close really closes; the tray opens a new window later. Page state resets and memory is freed while hidden.
- **One hidden window**: set `OnCloseRequest` to return `CloseHide`. Reopening is instant and page state survives, at the cost of the webview staying alive.

Pick per app; both are one line.

---

## Advanced: switching close behavior at runtime

Both models can be flipped while the app runs - the natural home for a "keep running in the tray" settings checkbox.

**Window-per-open apps** (a `gantry.Run` app with `Tray: true`): call `gantry.SetCloseToTray`. It is read at every window close, so the next close follows whatever was set last - `true` (the default) keeps the app in the tray, `false` makes close quit the whole app, tray icon included. The full recipe, from a settings-page checkbox down to Go:

```tsx
// pages/settings/settings.tsx
const { send } = usePaired();
<input type="checkbox" checked={tray}
       onChange={(e) => { setTray(e.target.checked); send("tray", e.target.checked); }} />
```

```go
// pages/settings/settings.go
var Page = ui.Page{
    Key: "pages/settings",
    On: ui.Handlers{
        "tray": func(p json.RawMessage) {
            var on bool
            _ = json.Unmarshal(p, &on)
            gantry.SetCloseToTray(on)
            // persist the preference and call SetCloseToTray again at
            // startup (e.g. in Config.Setup) so it survives restarts.
        },
    },
}
```

`gantry.CloseToTray()` reads the current setting. Note the limits: the toggle needs the tray to exist (`Config.Tray` or `--tray`) - a tray cannot be created at runtime - and without one, closing already exits.

**Hidden-window apps** (`OnCloseRequest` + `CloseHide`): the hook is invoked fresh on every close, so close over a mutable value instead of returning a constant:

```go
var hideOnClose atomic.Bool // flip from anywhere, e.g. a settings handler

Window: func(w *appshell.WindowOptions) {
    w.OnCloseRequest = func() appshell.CloseAction {
        if hideOnClose.Load() {
            return appshell.CloseHide
        }
        return appshell.CloseAllow
    }
},
```

Apps driving `appshell.App` directly get the same switch as the `KeepRunning func() bool` field - consulted at each close on tray apps; return `false` and that close quits.

## Advanced: single instance

Gantry apps bind a fixed local port for their frontend server, and that bind doubles as the single-instance guard via `appshell.Listen`:

```go
ln, err := appshell.Listen(cfg.Port)
if err != nil {
    // "already running?" - another instance holds the port
    return err
}
// hand ln to http.Server.Serve
```

Starting a second copy fails the bind and exits. There is no lockfile to go stale.

## Advanced: shutdown

The scaffold wires `ctx` to Ctrl+C (`signal.NotifyContext`) and tray Quit to `cancel()`. When `ctx` is done: `App.Run` returns, your server shuts down, widgets and popups die with their `ProcManager.CloseAll()`, and `main` returns. Nothing needs to be killed by hand.

Child roles (widgets, popups) are `windowsgui` processes with no console, so their crashes would be invisible; `appshell.RoleLog(appName, role)` redirects their logger to `%LocalAppData%\<appName>\<role>.log` and returns a cleanup func - the scaffold defers it in each `--shellrole` branch (see [Widgets](widgets.md)).
