# Project structure

A Gantry app is a Go module with React files living inside it. There is no `web/` directory and no build config to maintain - the CLI synthesizes those. This page walks the layout `gantry new` lays down and then explains the one convention that ties it together: paired files. Here is the multi-page scaffold (a single-page app omits `layouts/`, `pages/settings/`, and `components/`):

```
myapp/
  main.go               the entrypoint: ~a dozen lines calling gantry.Run
  gantry_registry.go    generated page/component registrations - never edit
  embed.go              embeds webdist/ (the built frontend) into the exe
  app.tsx               optional: a custom TitleBar + app-wide options (create it yourself)
  go.mod                Go module name + dependency on the Gantry module
  package.json          npm dependencies (gantry-web, react, react-dom, vite, ...)
  tsconfig.json         tells the editor how to read the .tsx files
  gantry.json           app settings + build targets the gantry CLI reads
  index.css             app-wide styles and the theme (--gantry-* variables)
  .gitignore            ignores the generated + build folders below
  icons/                default icon files (icon.ico, icon.png) - swap for your art
  resources/            optional: files embedded into the exe, reachable from Go and tsx
  layouts/
    main/
      main.tsx          optional shared chrome (navbar etc.); pages pick layouts by name
      main.css
  pages/
    index/
      index.go          page logic (Go)
      index.tsx         page look (React)
      index.css         page styles (optional, auto-imported)
  components/
    example/
      example.go        component logic
      example.tsx       component look
      example.css       component styles (optional)
  tests/                generated smoke test; add your own end-to-end tests here
  .vscode/              editor settings (excludes, recommended extensions)
  .gantry/              synthesized Vite root - gitignored, regenerated every run
  webdist/              built frontend, embedded into the exe - gitignored
  dist/                 release artifacts per os/arch - gitignored
```

You edit `main.go`, `index.css`, and the files inside `pages/`, `components/`, `layouts/`, `resources/`, and `icons/`. You never edit the generated `gantry_*.go` files or the synthesized `.gantry/` folder - they are overwritten on every `gantry dev`/`build`/`gen`.

## The paired-file convention

This is the heart of a Gantry app. Every page and every component is a **folder** holding up to three same-named files:

![The app root holds main.go, gantry.json, and the pages and components folders; a page folder pairs a .go logic half with a .tsx look half under one key.](project-tree.svg)

- `<name>.go` - the **logic** half. A normal Go package that exports one `ui.Page` (in `pages/`) or `ui.Component` (in `components/`) value.
- `<name>.tsx` - the **look** half. A normal React component as the `default` export.
- `<name>.css` - optional styles, imported automatically when the folder is built. No `import` statement needed anywhere.

So `pages/index/` contains `index.go`, `index.tsx`, and optionally `index.css`, and they add up to one page.

### How the two halves find each other

They pair by **key**: the folder path relative to the app root, like `pages/index` or `components/example`. The **Go side writes the key explicitly** in its exported value:

```go
// pages/index/index.go
var Page = ui.Page{Key: "pages/index"}
```

The **tsx side never writes it** - the Gantry Vite plugin derives the same key from the file's location on disk and injects it into `usePaired()`. That is why inside a pair folder you call `usePaired()` with no argument: the plugin already knows which `.go` file sits next door. (Outside a pair folder you would pass the key by hand, e.g. `usePaired("pages/index")`.) The two halves then talk over one websocket carrying keyed messages - nothing about the pairing is magic at runtime. See [Pairs](../ui/pairs.md) for the full protocol.

### Why folders, not files

Because the folder is *also* a normal Go package. Go tooling completely ignores `.tsx` and `.css` files, so `pages/index/` builds as the Go package `index` with no special handling, while Vite sees the same folder as a React module with a stylesheet. One folder, two toolchains, no conflict. Both files carry the base name so the pairing is obvious at a glance and the key is unambiguous.

### Pairs can nest and can be half-only

Folders nest to any depth: `pages/account/settings/settings.go` pairs with `settings.tsx` at key `pages/account/settings`. A pair can also be **tsx-only** (no `.go` file) - a page that needs no Go logic - or, less commonly, Go-only. The registry only picks up folders whose Go half exports the conventional `Page`/`Component` var.

## Pages vs components

- **Pages are routable.** `pages/index` serves `/`, `pages/settings` serves `/settings`, and nested folders route by their path. Override the URL with `Route:` in the Go value or `export const route` in the tsx. Crucially, **with only `pages/index` the app is single-page and no router runs at all** - add a second page folder and routing switches on automatically.
- **Components are reusable pieces.** Import one like any React component (`import Example from "../../components/example/example"`), or drop it into a Tea `View()` from Go with `ui.Custom("components/example", nil)`.

Widgets and popup windows are pages too - a widget is just a small native window pointed at one of your routes. See [Widgets](../shell/widgets.md).

## Registration is automatic

You never list your pages by hand. On every `gantry dev`, `gantry build`, or `gantry gen`, the CLI scans `pages/`, `components/`, and `layouts/` for Go halves that export a `Page` or `Component` var and regenerates **`gantry_registry.go`** - a generated `gantryPairs()` function that imports each package and returns every registered value. `main.go` just passes it through: `gantry.Run(gantry.Config{... Pairs: gantryPairs() ...})`. Add a pair, run `gantry dev`, done. If a key ever fails to match (a typo in the `Key` string, or a `.tsx` with no matching Go handler), the tsx side's `send()` logs `no handler for ...` in the Go terminal.

Alongside `gantry_registry.go`, other generated `gantry_*.go` files appear only as you use the features that need them: `gantry_icons.go` (embeds `icons/`), `gantry_resources.go` (embeds `resources/` once it holds a file), `gantry_args.go` (bakes in declared app arguments), and `gantry_widgets.go` (mobile home-screen widgets). All are marked "Code generated by gantry gen; DO NOT EDIT" and are safe to delete - they regenerate.

## resources/: files both halves can read

Anything you drop in `resources/` - images, fonts, JSON, whatever - is embedded into the exe once and reachable from **both** halves of your app by the same relative path. Create the folder, add files, run `gantry dev`. Like `pages/`, the folder name is a fixed convention, and unlike `webdist/`/`dist/` it is your source, so commit it. An empty `resources/` is ignored (the `//go:embed` directive needs at least one file), and the generated `gantry_resources.go` appears only once the folder holds a file.

From tsx, reference a resource by URL with `resourceURL()`:

```tsx
import { resourceURL } from "gantry-web";

<img src={resourceURL("img/logo.png")} />;
const cfg = await fetch(resourceURL("cfg.json")).then((r) => r.json());
```

From Go, read the same bytes with `gantry.Resource` (or `gantry.Resources()` for the whole `fs.FS`):

```go
b, err := gantry.Resource("cfg.json") // []byte, or fs.ErrNotExist
```

One embedded copy backs both: a built app serves `resources/` at `/resources/...`; in `gantry dev` the same folder is served live off disk, so edits show up without a rebuild. Because it rides Go's `//go:embed`, the folder travels to Android and iOS builds automatically.

## gantry.json

Written by `gantry new` so `dev`/`build` never re-ask your scaffold choices. A single-page, Tea-style scaffold with a tray looks like this:

```json
{
  "$schema": "https://raw.githubusercontent.com/BlueBeard63/Gantry/main/gantry.schema.json",
  "name": "myapp",
  "title": "Myapp",
  "version": "0.1.0",
  "gantry": "0.4.0",
  "port": 8330,
  "mode": "single",
  "style": "tea",
  "tray": true,
  "buttons": { "minimize": true, "maximize": false, "close": true },
  "icons": "icons"
}
```

`name` is the exe/module name, `version` shows up in installers, `gantry` records the framework version the app was scaffolded with (the baseline `gantry upgrade` compares against), and `port` is the local server the app binds (also its single-instance guard). The rest just remembers your scaffold answers. Fields that default off are omitted until you set them: `tailwind`, `args` (custom [app arguments](../advanced/args.md)), `build` (extra targets, `console`, installers), and `mobile` (Android/iOS identity and permissions). Note that changing `buttons` here does **not** reconfigure a built app - the real switches live in `main.go`'s `Window` hook; this file only records what the scaffold generated.

## The synthesized .gantry/ folder

`gantry dev` and `gantry build` regenerate `.gantry/` (its `index.html`, `main.tsx`, and `vite.config.ts`) every run - it is the Vite root, derived entirely from `gantry.json` and your files. Never edit anything in it; your changes would be overwritten. Everything you would actually want to change lives in your own files: the theme in `index.css`, page layout in your `.tsx`, window behavior in `main.go`. If you outgrow the synthesis entirely, see [Without the CLI](../advanced/without-the-cli.md).
