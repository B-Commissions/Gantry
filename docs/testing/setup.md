# Testing: setup

Gantry ships an end-to-end test driver that drives the real running app - the real Go process, the real websocket, the real window - not a simulation of the wire protocol against mocked pages. Tests live in your app repo, are plain Go tests written with the `gantrytest` package, and run with one command: `gantry test`.

This page gets a first test running and documents the `gantry test` command and what `Launch` sets up. From there, the protocol-plane API is split across [pages & the tree](pages-and-tree.md), [events & calls](events-and-calls.md) and [state, pushes & restarts](state-and-restarts.md); [the DOM plane](dom.md) covers real clicks and screenshots, [errors and artifacts](errors-and-artifacts.md) covers error assertions and traces, [widget snapshots](widgets.md) covers host-side widget tests, [mobile](mobile.md) covers device testing, and [CI](ci.md) covers pipelines.

![gantrytest.Launch builds and starts the real app, then drives it on two planes: the protocol plane speaks the /gantry/ws wire protocol to the Go process, and the optional DOM plane drives the webview over CDP.](test-driver.svg)

## Your first test

Create a `tests/` directory at the app root (next to `gantry.json`) and add a Go test file whose package is `tests`. `gantry new` scaffolds a first `tests/smoke_test.go` for you, so a fresh app is testable out of the box. The driver finds the app root by walking up from the test's working directory to the nearest `gantry.json`, so the file just has to live somewhere under the app.

```go
// tests/smoke_test.go
package tests

import (
	"strings"
	"testing"

	"github.com/BlueBeard63/Gantry/gantrytest"
)

func TestCounter(t *testing.T) {
	t.Parallel()
	app := gantrytest.Launch(t)         // builds + starts the app, cleans up after
	app.Ready("pages/index")            // mount a page, like the frontend would

	tree := app.WaitTree("first render", func(n *gantrytest.Node) bool {
		return strings.Contains(n.Text(), "count is 0")
	})
	app.Click(tree.Find("button"))      // fire the button's Tea click handler
	app.WaitTree("count advanced", func(n *gantrytest.Node) bool {
		return strings.Contains(n.Text(), "count is 1")
	})

	app.ExpectNoErrors()                // nothing landed in the error pipeline
}
```

Then run:

```
gantry test
```

That is a complete end-to-end test: it launches the real app binary, mounts a page exactly as the frontend does when a page component mounts, fires the real click handler, waits for the state to advance, and asserts the error pipeline stayed clean. Because these are standard Go tests, everything from `go test` works - `t.Parallel()`, subtests, `-run` filters, timeouts, IDE test-runner integration.

## The `gantry test` command

```
gantry test [flags] [pattern]
```

`gantry test` prepares the app exactly like a build - it regenerates `.gantry/` and the registries and runs one vite build so the embedded frontend is current - then builds the app binary once for the whole suite into `.gantry/test/<name>` (`.exe` on Windows), stamped with the `gantry.json` version via `-ldflags -X`. It then runs `go test ./tests/... -count=1 -parallel N -timeout <d>` with `GANTRY_TEST_BIN` pointing every `Launch` at that one prebuilt binary (so parallel tests never each race their own build), and `-count=1` because e2e runs are never cacheable. When you pass a `pattern` it is forwarded verbatim as `go test -run <pattern>`. Every run also writes a self-contained `gantry_test_report.html` into `test-results/` - the Run Overview plus a per-test Screencast / Screenshots / Trace / Logs viewer. See [the report](report.md).

| Flag | Meaning |
| --- | --- |
| `pattern` | test name filter, passed to `go test -run` |
| `--headed` | real window instead of headless (and the [DOM plane](dom.md) with it, on Windows or Linux) |
| `--record` | record `screencast.avi` for every DOM-plane test (implies keeping those artifacts) |
| `--retries N` | re-run each failed test up to N times; one that then passes is reported flaky ([report](report.md)) |
| `--show` | open the most recent `gantry_test_report.html` instead of running (a `pattern` deep-links to that test) |
| `--open` | open the report in the browser when the run finishes |
| `--mode production` | run apps in production mode (default: `development`) |
| `--device android[:SERIAL]` | run the suite on a plugged-in phone or emulator instead of the desktop ([mobile](mobile.md)) |
| `--allow-device-data` | allow the hermetic `pm clear` (wipes the app's on-device data) on a physical device; emulators always allow it |
| `-p N` | parallelism (default NumCPU/2, floored at 1 - each parallel test is a full app process; forced to 1 with `--device`) |
| `--keep-artifacts` | keep passing tests' artifacts too |
| `--update` | rewrite golden files ([widget snapshots](widgets.md)) instead of comparing |
| `-v` | verbose `go test` output |
| `--timeout` | overall suite timeout (default 10m) |

A run exits non-zero (`test.failed`) when any test failed, and prints a one-line tally (`N passed, M flaky, K failed in 12.3s`) plus the report path. `gantry test --show` reopens the last report without running anything.

## Notes

### What a test can see

A test session talks to the app on two planes, and can assert on either or both. The **protocol plane** is what Go thinks: the driver dials `/gantry/ws` and speaks the [wire protocol](../advanced/protocol.md) exactly like the frontend does - it mounts pages, fires Tea and paired events, awaits calls, and observes renders, pushes, shared state and error frames. Everything on this plane is headless and cross-platform, and it is enough to test a Tea-style app end to end. The **DOM plane** is what the user sees: element queries, real clicks and typing, screenshots and screencasts, driven over the webview's devtools protocol (CDP); enable it with `WithDOM()`. See [pages & the tree](pages-and-tree.md) and [the DOM plane](dom.md).

### What Launch gives you

Every `gantrytest.Launch(t)` starts the app binary with `--port 0` and `--announce-ready`, so each test learns its own ephemeral port from the process's `GANTRY_READY` line and parallel tests never collide with each other or a dev instance; it passes `--no-open` (headless) unless `--headed` / `WithHeaded()` / `WithDOM()` asks for a window; it redirects the app's config dir - where `geometry.json` and `crash.log` live - into a per-test temp directory (`APPDATA`+`LOCALAPPDATA` on Windows, `XDG_CONFIG_HOME`+`XDG_CACHE_HOME` on Linux, `HOME` on macOS), so tests are hermetic and crash assertions are per-test; it defaults the app to `development` mode so error detail is full (override with `WithMode("production")`); and it registers `t.Cleanup` so the whole process tree is hard-killed (no orphaned webview or helper processes survive), `crash.log` is collected, and the artifact directory is kept when the test failed. The full option set is on [State, pushes & restarts](state-and-restarts.md#launch-options).

### Running without gantry test

A bare `go test ./tests/...` also works - the driver finds the app root by walking up to `gantry.json` and, if `GANTRY_TEST_BIN` is unset, builds the binary itself once on the first `Launch` (every parallel test reuses it via a `sync.Once` keyed on the app dir). The difference is freshness: `gantry test` refreshes the frontend first, whereas a bare `go test` embeds whatever the last `gantry dev` / `gantry build` left in `webdist/`. The whole `GANTRY_TEST_*` environment is optional; the driver derives every default when it is absent.

### Out of scope

Unit-testing React components in isolation (use vitest + testing-library directly - nothing Gantry-specific about it) and Go unit tests (plain `go test` on your own packages).

Next: [Pages & the tree](pages-and-tree.md).
