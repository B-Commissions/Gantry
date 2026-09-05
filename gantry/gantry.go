// Package gantry boots a Gantry app in one call. It owns the
// boilerplate every app shares - flags, the helper-window roles, the
// HTTP server, the websocket, the shell window and tray - so a main.go
// is a handful of lines:
//
//	func main() {
//		gantry.Run(gantry.Config{
//			Name:  "myapp",
//			Title: "My App",
//			Port:  8330,
//			Dist:  dist(),
//			Pairs: gantryPairs(),
//			Tray:  true,
//		})
//	}
//
// Everything is still reachable: Window tweaks the window options,
// Setup registers services/state/routes, Roles adds widget windows.
// Apps that outgrow Run can copy its body and own the loop - see the
// "Without the CLI" docs page.
package gantry

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	nethttppprof "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BlueBeard63/Gantry/appicon"
	"github.com/BlueBeard63/Gantry/appshell"
	"github.com/BlueBeard63/Gantry/tray"
	"github.com/BlueBeard63/Gantry/ui"
)

// RoleArgs carries the standard child-window flags to a custom role.
type RoleArgs struct {
	Port     int
	URL      string
	Monitor  int
	Position string
}

// Default icons, registered by the generated gantry_icons.go when the
// app has an icons/ directory. Code-level settings override them.
var (
	defaultIconPNG []byte
	defaultIconICO []byte
)

// SetDefaultIcons registers the app's default icon bytes (PNG for
// windows and Linux trays, ICO for the Windows tray). gantry gen
// writes a file calling this when the icons directory exists; calling
// it by hand works too.
func SetDefaultIcons(png, ico []byte) {
	defaultIconPNG = png
	defaultIconICO = ico
}

// The app's embedded resources/ tree, registered by the generated
// gantry_resources.go when the app has a resources/ directory. Both
// planes read the same bytes: Go code via Resource/Resources, the
// frontend via the /resources/ HTTP route (resourceURL() in gantry-web).
var appResources fs.FS

// emptyFS is Resources' fallback when the app has no resources/ dir, so
// Resource never panics and always returns fs.ErrNotExist.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// SetResources registers the app's embedded resources/ tree. The
// generated gantry_resources.go calls this when the directory exists;
// calling it by hand (with any fs.FS) works too.
func SetResources(fsys fs.FS) { appResources = fsys }

// Resources returns the embedded resources/ tree (an empty FS if the
// app has none), for streaming or serving from Go code.
func Resources() fs.FS {
	if appResources == nil {
		return emptyFS{}
	}
	return appResources
}

// Resource reads one embedded resource by slash path, e.g.
// "img/logo.png". It returns fs.ErrNotExist if the app has no
// resources/ directory or the file is absent.
func Resource(name string) ([]byte, error) { return fs.ReadFile(Resources(), name) }

// Config describes the app. Only Name, Title, Port, Dist and Pairs are
// required; everything else has sensible defaults.
type Config struct {
	// Name is the exe/app identity (WebView2 data folders, logs).
	Name string
	// Title is the window and tray title.
	Title string
	// Port is the local server port (and single-instance guard).
	Port int
	// Dist is the embedded frontend (embed.go's dist()).
	Dist fs.FS
	// Pairs registers every page/component - pass gantryPairs(), the
	// generated registry.
	Pairs []any

	// Tray keeps the app running in the tray when the window closes.
	Tray bool
	// TrayMenu adds actions between the standard Open and Quit items.
	TrayMenu []tray.MenuItem

	// Window tweaks the shell window before it opens (buttons, size,
	// close hook, chrome metrics) - the defaults are already sane.
	Window func(w *appshell.WindowOptions)
	// Setup runs once before serving: register services
	// (app.Service), shared state (ui.NewState), pushes, and your own
	// HTTP routes on mux.
	Setup func(app *ui.App, mux *http.ServeMux)
	// Roles adds custom child-window kinds (widgets) invoked with
	// --shellrole <name>; the "popup" role is built in.
	Roles map[string]func(a RoleArgs) error

	// Errors configures crash detection and interception. The zero
	// value is on-by-default: panics and frontend errors are captured
	// with the page and action trail, recorded, and shown by the
	// built-in error UI. See ErrorOptions.
	Errors ErrorOptions
}

// Run parses flags, dispatches helper roles, and drives the app until
// it quits. It never returns except on fatal startup errors.
func Run(cfg Config) {
	var (
		port      = flag.Int("port", cfg.Port, "local server port")
		browser   = flag.Bool("browser", false, "open in the default browser instead of a native window")
		noOpen    = flag.Bool("no-open", false, "headless: serve only, no window")
		trayOn    = flag.Bool("tray", false, "run with the tray even if the app was configured without one")
		trayOff   = flag.Bool("no-tray", false, "run without the tray (closing the window exits)")
		devURL    = flag.String("dev-url", "", "dev: load the frontend from this URL (gantry dev sets it)")
		token     = flag.String("token", "", "require this token on every request, via the gantry_token cookie or ?gantry_token= (the mobile shell sets it)")
		announce  = flag.Bool("announce-ready", false, "print GANTRY_READY port=N to stdout once the server listens (works with --port 0)")
		emitOnly  = flag.Bool("emit-widgets", false, "print the widget snapshot as JSON and exit without serving")
		shellRole = flag.String("shellrole", "", "internal: run a helper window role instead of the app")
		roleURL   = flag.String("url", "", "internal: url for the helper window")
		monitor   = flag.Int("monitor", -1, "internal: monitor index for popups")
		position  = flag.String("position", "bottom", "internal: popup position top|bottom")
	)
	flag.Parse()

	// --dev-url is the development signal when GANTRY_MODE is absent
	// (a plain `go run . --dev-url ...` without the CLI).
	if *devURL != "" {
		setDevURLSeen()
	}

	// The tray is toggleable at RUN time, no rebuild: Config.Tray is
	// just the default.
	if *trayOn {
		cfg.Tray = true
	}
	if *trayOff {
		cfg.Tray = false
	}

	// Helper window roles run as child processes (crash isolation):
	// the exe re-invokes itself with --shellrole and renders exactly
	// one window.
	if *shellRole != "" {
		defer appshell.RoleLog(cfg.Name, *shellRole)()
		args := RoleArgs{Port: *port, URL: *roleURL, Monitor: *monitor, Position: *position}
		var err error
		switch {
		case *shellRole == "popup":
			err = appshell.RunPopup(appshell.PopupOptions{
				AppName:  cfg.Name,
				URL:      args.URL,
				Width:    460,
				Height:   140,
				Monitor:  args.Monitor,
				Position: args.Position,
			})
		case cfg.Roles[*shellRole] != nil:
			err = cfg.Roles[*shellRole](args)
		default:
			log.Fatalf("unknown --shellrole %q", *shellRole)
		}
		if err != nil {
			log.Fatalf("%s: %v", *shellRole, err)
		}
		return
	}

	if *emitOnly {
		if err := emitWidgets(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	flags := runFlags{
		port:          *port,
		browser:       *browser,
		noOpen:        *noOpen,
		devURL:        *devURL,
		token:         *token,
		announceReady: *announce,
	}
	if err := run(cfg, flags); err != nil {
		log.Fatal(err)
	}
}

// runFlags is the parsed command line run() acts on.
type runFlags struct {
	port          int
	browser       bool
	noOpen        bool
	devURL        string
	token         string
	announceReady bool
}

func run(cfg Config, f runFlags) error {
	start := time.Now()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	app := ui.NewApp(cfg.Pairs...)
	wireErrors(app, cfg.Errors)
	// Uncatchable crashes (a panic on a plain goroutine) leave their
	// trace in crash.log; a waiting trace from the last run enters the
	// pipeline now and reaches the frontend via the errors call.
	if !cfg.Errors.Disable {
		if trace := setupCrashLog(cfg.Name); trace != "" {
			reportLastCrash(app, trace)
		}
	}

	mux := http.NewServeMux()
	mux.Handle("/gantry/ws", app.Handler())
	mux.Handle("/gantry/widgets.json", widgetsHandler())
	mux.Handle("/gantry/notify/action", notifyActionHandler())
	// pprof for profiling the running app, off in production unless
	// GANTRY_PPROF=1. Mounted on this mux (not DefaultServeMux) so it sits
	// behind the same local-only server; pprof.Index also serves the named
	// runtime profiles (/debug/pprof/heap, /goroutine, ...).
	if pprofEnabled() {
		mux.HandleFunc("/debug/pprof/", nethttppprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", nethttppprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", nethttppprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", nethttppprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", nethttppprof.Trace)
		log.Printf("perf: pprof enabled at /debug/pprof/")
	}
	// Built-in app identity for the frontend: useAppInfo() /
	// call("gantry", "appInfo") resolve name, title and the version
	// stamped from gantry.json. Registered before Setup so an app can
	// override it.
	app.Service("gantry", ui.Calls{
		"appInfo": func(json.RawMessage) (any, error) {
			return map[string]string{
				"name":    cfg.Name,
				"title":   cfg.Title,
				"version": Version(),
			}, nil
		},
		// useEnv() / call("gantry", "env"): the mode and every declared
		// arg, so the frontend can gate pages and features.
		"env": func(json.RawMessage) (any, error) {
			return map[string]any{
				"mode": Mode(),
				"args": Args(),
			}, nil
		},
		// The error pipeline's frontend surface: fetch what was
		// captured (including errors that fired while disconnected and
		// last-run crashes), clear it, and report JS-side errors so
		// every crash - Go or JS - lands in one place.
		"errors": func(json.RawMessage) (any, error) {
			return app.RecentErrors(), nil
		},
		"clearErrors": func(json.RawMessage) (any, error) {
			app.ClearErrors()
			return nil, nil
		},
		"reportError": func(p json.RawMessage) (any, error) {
			var e ui.ErrorInfo
			if err := json.Unmarshal(p, &e); err != nil {
				return nil, err
			}
			log.Printf("frontend error [%s] %s: %s", e.Kind, e.Source, e.Message)
			app.ReportErrorFromClient(e)
			return nil, nil
		},
		// addBreadcrumb(): app-specific context in the error trail.
		"breadcrumb": func(p json.RawMessage) (any, error) {
			var c struct {
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(p, &c); err != nil {
				return nil, err
			}
			app.Breadcrumb(c.Detail)
			return nil, nil
		},
	})
	// The embedded resources/ tree (gantry_resources.go), shared with
	// Go's Resource/Resources. Registered before "/" so the SPA
	// catch-all does not swallow /resources/* into index.html; the
	// ServeMux longest-prefix match makes "/resources/" win regardless.
	if appResources != nil {
		res := http.StripPrefix("/resources/", http.FileServer(http.FS(appResources)))
		mux.Handle("/resources/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Embedded resources are referenced by stable path (not content-
			// hashed), so revalidate rather than cache-forever: a new app
			// build may change the bytes behind the same URL.
			w.Header().Set("Cache-Control", "no-cache")
			res.ServeHTTP(w, r)
		}))
	}
	if cfg.Setup != nil {
		cfg.Setup(app, mux)
	}
	mux.Handle("/", appshell.ServeSPA(cfg.Dist))

	ln, err := appshell.Listen(f.port) // also the single-instance guard
	if err != nil {
		return err
	}
	// --port 0 binds an ephemeral port; everything downstream needs
	// the real one.
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: tokenHandler(f.token, recoverHandler(app, mux))}
	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	if perfEnabled() {
		log.Printf("perf: listen+serve=%dms", time.Since(start).Milliseconds())
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	if f.announceReady {
		// The mobile shell scans stdout for this line to learn the port.
		fmt.Printf("GANTRY_READY port=%d\n", port)
	}

	url := "http://127.0.0.1:" + strconv.Itoa(port)
	if f.token != "" {
		url += "/?gantry_token=" + f.token // the first load sets the auth cookie
	}
	if f.devURL != "" {
		url = f.devURL // gantry dev: HMR inside the native window
	}
	log.Printf("%s serving on %s", cfg.Title, "http://127.0.0.1:"+strconv.Itoa(port))

	if f.noOpen {
		return <-errCh
	}

	// Icon precedence: the exe's embedded resource (gantry build bakes
	// icons/icon.ico in) is tried first by the shell; then the app's
	// registered defaults (gantry_icons.go); then the drawn placeholder.
	iconPNG, iconICO := defaultIconPNG, defaultIconICO
	if iconPNG == nil || iconICO == nil {
		glyph := appicon.Render(32, appicon.DefaultPalette())
		if iconPNG == nil {
			iconPNG = appicon.PNG(glyph)
		}
		if iconICO == nil {
			iconICO = appicon.ICO(glyph)
		}
	}
	window := appshell.WindowOptions{
		AppName:   cfg.Name,
		Title:     cfg.Title,
		URL:       url,
		Width:     1100,
		Height:    720,
		MinWidth:  480,
		MinHeight: 320,
		AutoFocus: true,
		Icon:      appshell.IconSource{PNG: iconPNG},
		Geometry:  appshell.FileGeometry(geometryPath(cfg.Name)),
	}
	if cfg.Window != nil {
		cfg.Window(&window)
	}
	// The test runner's DOM plane needs a real webview to attach CDP to,
	// but not one the user sees: parked far off-screen, no focus steal,
	// no geometry persistence (gantrytest sets this for non-headed
	// DOM-plane launches).
	if os.Getenv("GANTRY_WINDOW_OFFSCREEN") == "1" {
		window.X, window.Y = -10000, -10000
		window.AutoFocus = false
		window.Geometry = nil
	}

	shell := &appshell.App{
		Window:  window,
		Browser: f.browser,
		// Read at every window close: SetCloseToTray flips it live.
		KeepRunning: closeToTray.Load,
	}
	if cfg.Tray {
		shell.Tray = &tray.Options{
			Icon:    iconICO,
			IconPNG: iconPNG,
			Title:   cfg.Title,
			Tooltip: cfg.Title + " is running",
			Menu:    cfg.TrayMenu,
		}
	}
	if perfEnabled() {
		log.Printf("perf: opening window=%dms (first paint follows in the frontend perf log)", time.Since(start).Milliseconds())
	}
	return shell.Run(ctx, cancel)
}

// perfEnabled reports whether startup timing should be logged: on in
// development, or forced with GANTRY_PERF=1.
func perfEnabled() bool {
	switch os.Getenv("GANTRY_PERF") {
	case "1", "true", "yes", "on":
		return true
	}
	return IsDev()
}

// pprofEnabled reports whether the pprof endpoints should be mounted: on
// in development, or forced with GANTRY_PPROF=1. Off in a shipped binary
// unless explicitly enabled.
func pprofEnabled() bool {
	switch os.Getenv("GANTRY_PPROF") {
	case "1", "true", "yes", "on":
		return true
	}
	return IsDev()
}

func geometryPath(name string) string {
	base, err := os.UserConfigDir()
	if err != nil {
		return "geometry.json"
	}
	return filepath.Join(base, name, "geometry.json")
}
