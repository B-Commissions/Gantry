# Mobile & docs commands

`mobile dev` runs the app on a plugged-in phone, `docs` browses these pages offline, and `--version` prints the CLI version. This page also carries the `gantry.json` reference and the two cross-cutting notes on declared app args and the update-check notice. Flags parse with Go's standard `flag` package, so each accepts both `-flag` and `--flag`; boolean flags take no value.

## gantry mobile dev

The phone equivalent of `gantry dev`; the `mobile` group has one subcommand today, `dev`, which takes the platform as its argument.

```
gantry mobile dev android
gantry mobile dev ios
```

For **android**: checks the toolchain (offering to install the missing SDK pieces - command-line tools, platform-tools/adb, the NDK; a JDK 17+ is the one thing it will not install for you), finds the single USB-connected device (Wi-Fi/network adb devices are rejected on purpose, emulators count as plugged in), builds the APK for that device's ABI (`arm64-v8a` or `x86_64`), installs it, launches it (`am start -n <id>/.MainActivity`), and streams the app's logcat (`gantry-go` tag, from "now") until Ctrl+C. It needs a `mobile` section with an `id` in `gantry.json`. For **ios**: requires a Mac with Xcode (`xcodebuild` on PATH), generates the experimental scaffold under `.gantry/ios/` and prints the Xcode hand-off (`xcodegen generate && xed .`) - running on a device stays manual. Details live in the Mobile docs: [Android builds](../mobile/android.md), [iOS](../mobile/ios.md). `mobile dev` takes no flags; an unknown or missing platform prints its usage.

## gantry docs

The documentation browser - these very pages, embedded in the CLI and readable fully offline. By default it opens the web viewer in your browser; `-tui` keeps the terminal browser instead. An optional `topic` argument jumps to the best-matching page (scored by title, path and body-text matches). Two opt-in extras hang off it: `--ai` adds a chat assistant to the web viewer, and `--mcp` turns the command into a docs server for coding agents.

```
gantry docs                  open the web viewer at the index
gantry docs window           open at the best match for "window"
gantry docs -tui             browse in the terminal instead
gantry docs --print tea      print a page as plain markdown to stdout
gantry docs --ai             open the web viewer with the chat assistant
gantry docs --mcp            run as a stdio MCP server for coding agents
```

Flags:

- `-tui` (bool, default: false) - browse in the terminal (a Bubble Tea viewer) instead of the web viewer.
- `--port N` (int, default: **8331**) - the port the web viewer binds, so the URL stays stable across runs and you can bookmark it. If that port is already in use (say a second docs viewer), the viewer falls back to an ephemeral port and prints the one it actually bound. Pass `--port 0` to always take an ephemeral port.
- `--ai` (bool, default: false) - enable the opt-in docs assistant: a chat widget in the web viewer, grounded on the docs (it retrieves the most relevant pages per question and cites them). The backend is pluggable - see [The docs assistant](#the-docs-assistant) below. The docs stay fully offline; the assistant is purely additive.
- `--mcp` (bool, default: false) - run a headless stdio MCP server that exposes the docs to coding agents, instead of opening a viewer - see [The MCP server](#the-mcp-server) below. No web server, no authentication.
- `--print` (bool, default: false) - print the selected page as plain markdown to stdout and exit (good for piping or grepping).
- `--frame` (bool, default: false) - render one frame at the terminal size to stdout and exit (a layout diagnostic for the terminal viewer).
- `--size WxH` (string, default: "") - force the `--frame` size, e.g. `--size 188x41`, for redirecting a frame to a file. When set (or when stdout is redirected), the frame is emitted plainly with colors stripped and each row annotated with its measured display width.

### The terminal browser (`-tui`)

The left pane holds a search box and the category tree, the right pane the page. `tab` switches focus; arrows (or `k`/`j`) move and `enter` opens; `/` starts a search across titles and content and `esc` cancels; `f` lists the current page's links and `enter` follows the pick (internal links navigate here, external links open in your browser, or land on the clipboard if no browser can open); `b`/`n` go back/forward through your history; `pgup`/`pgdown` (and space to page down) scroll the content; `q` (or Ctrl+C) quits. On narrow terminals (below 50 columns) the sidebar collapses and `tab` toggles between the two panes.

### The docs assistant

`gantry docs --ai` adds an "Ask" button to the web viewer. It retrieves the most relevant pages for your question, grounds the backend on them, streams the answer, and shows the pages it used as clickable chips - so pasted errors get routed to the right page and answers stay accurate to the docs. Everything runs on your machine, and the docs work with or without it. `GANTRY_DOCS_AI_BACKEND` selects the backend (default `auto`):

- `auto` (default) - use an installed coding-agent CLI if one is on your PATH (Claude Code first, then Codex); otherwise fall back to a local model. Usually what you want: no model download, and it reuses the capable agent you already have.
- `claude` - route each question to Claude Code in headless mode (`claude -p`, streamed token by token). Uses your Claude Code login and usage.
- `codex` - route to Codex (`codex exec`).
- `ollama` (aliases `http` / `openai` / `local`) - an OpenAI-compatible `/chat/completions` server. Defaults to a local Ollama at `http://localhost:11434/v1` with model `qwen2.5`; when Ollama is installed and the model is missing it is pulled automatically in the background. Override with `GANTRY_DOCS_AI_URL`, `GANTRY_DOCS_AI_MODEL` and `GANTRY_DOCS_AI_KEY` (llama.cpp, LM Studio or a hosted provider all work).

### The MCP server

`gantry docs --mcp` runs a local [Model Context Protocol](https://modelcontextprotocol.io) server over stdin/stdout instead of opening a viewer, so a coding agent working on a Gantry project can pull these docs for reference. It exposes three tools over the same embedded pages - `search_docs` (rank pages by a query), `read_doc` (the full markdown of a page by its route), and `list_docs` (the whole index). Any modules you have installed (see [Module commands](modules.md)) are part of the same corpus, so `search_docs` and `list_docs` return their pages alongside the framework docs; pass an optional `namespace` argument to either tool to scope it to one module. It needs **no network and no authentication**: the agent spawns `gantry docs --mcp` as a subprocess and talks to it directly over the pipe. Add it once to Claude Code with:

```
claude mcp add gantry-docs -- gantry docs --mcp
```

The same server works for any MCP client (Codex, Cursor, ...) through that client's own MCP configuration - for Codex, `codex mcp add gantry-docs -- gantry docs --mcp` or a `[mcp_servers.gantry-docs]` stanza in `~/.codex/config.toml` with `command = "gantry"` and `args = ["docs", "--mcp"]`.

### Agent files in scaffolded apps

`gantry new` sets all of this up for you: every app ships an `AGENTS.md` (the agent entry point), the `gantry` skill at both `.claude/skills/gantry/SKILL.md` and `.agents/skills/gantry/SKILL.md` (Claude Code and Codex read their respective directories), and a project-scope `.mcp.json` that registers `gantry-docs` for Claude Code automatically - approve it on first use. The skill teaches an agent the paired-file model, the CLI, testing, and to reach for `search_docs`/`read_doc` instead of guessing at framework APIs.

`gantry upgrade` maintains them: the two skill copies are tooling files (refreshed to the CLI's version by default, so agents always get current guidance), while `AGENTS.md` and `.mcp.json` are yours - extend them freely, upgrade defaults to keeping your version. An app scaffolded before these files existed gets an offer to create them on its next `gantry upgrade`; decline if you do not want them, or delete them - they are plain text with no runtime effect.

## gantry --version

Prints the installed CLI version. `gantry version` and `gantry -v` are equivalent.

```
gantry --version
Version: v0.4.0
```

Installed with `go install github.com/BlueBeard63/Gantry/cmd/gantry@latest` this reports the module tag - the quick way to confirm an update took. Built from a local checkout it reports `(devel)` plus the commit revision when no tag info is available.

## gantry.json

The file that makes a folder an app in the CLI's eyes; the app-finding commands walk up to the nearest one. It carries a `$schema` reference (added by `gantry new`) so editors validate it as you type - unknown keys are flagged and each field shows its docs on hover. Ports default to 8330 and versions to 0.1.0 when omitted.

```jsonc
{
  "$schema": "https://raw.githubusercontent.com/BlueBeard63/Gantry/main/gantry.schema.json",
  "name": "myapp",           // exe and Go module name
  "title": "Myapp",          // window title
  "version": "0.1.0",        // shown by installers; stamped into the exe by dev/build (gantry.Version() / useAppInfo())
  "gantry": "0.4.0",         // framework version this app was scaffolded with / last upgraded to (the upgrade baseline)
  "port": 8330,              // local server + single-instance port
  "mode": "single",          // or "multi" - records the scaffold choice
  "style": "tea",            // or "plain" - records the scaffold choice
  "tray": true,              // records the scaffold choice (runtime toggle: --tray/--no-tray)
  "tailwind": true,          // adds @tailwindcss/vite to the synthesized vite config (set by new/install --tailwind)
  "buttons": {               // records the scaffold choice
    "minimize": true, "maximize": false, "close": true
  },
  "icons": "icons",          // directory holding icon.ico + icon.png
  "args": { },               // custom app arguments - see App args
  "build": {
    "targets": ["windows/amd64", "linux/amd64", "mac/arm64"],
    "console": false,        // keep the console on Windows builds
    "installer": true        // produce Setup.exe / tar.gz / zip
  },
  "mobile": { }              // android/ios identity, permissions, widgets - see the Mobile docs
}
```

`name`, `title`, `port`, `version`, `icons`, `tailwind`, `args` and `build` feed `dev`/`build`; `mode`, `style`, `tray` and `buttons` record the scaffold choices (the live switches are in `main.go`, and the tray can be flipped at run time with the app's own `--tray`/`--no-tray` flags without a rebuild: `gantry dev -- --no-tray`, or `myapp.exe --no-tray`). `gantry` is maintained by `gantry new`/`gantry upgrade` and is the baseline `upgrade` reports against. The `mobile` section is documented under the [Mobile docs](../mobile/android.md); the `build` targets and installer behaviour are covered under [gantry build](project.md#gantry-build).

## Notes and advanced behaviour

**Update-check notice.** `new`, `dev`, `build`, `test` and `mobile` print a one-line "vX.Y.Z is available" notice when a newer tag exists. The check asks the module proxy at most once a day (cached), spends at most 1.5s on the network, stays silent on any failure, and never runs for local-checkout builds. `GANTRY_NO_UPDATE_CHECK=1` silences it; `gantry update` and `gantry upgrade` always ask the proxy directly (with a more generous timeout).

**Declared app args as env vars.** Each `gantry.json` `args` entry (name in lowercase kebab-case matching `^[a-z][a-z0-9-]*$`, `type` `string`/`bool`/`int`, optional `default`, `description`, `env`) travels to the app as an environment variable - the explicit `env` name (UPPER_SNAKE), or `GANTRY_ARG_<UPPER_SNAKE>` derived from the flag name (`api-host` becomes `GANTRY_ARG_API_HOST`). `gantry dev` registers them as real flags (validation and `--help` for free); a production binary reads the same variables via the generated `gantry_args.go`. Arg names may not shadow gantry's own flags (`port`, `vite-port`, `dev-url`, `tray`, `no-tray`, and so on) - a clash is a config error. See [App args](../advanced/args.md).
