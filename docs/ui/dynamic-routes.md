# Dynamic routes

Most pages have a fixed path (`pages/settings` -> `/settings`). A dynamic page instead captures part of the URL, so one page serves many addresses - `/examples/page1/1`, `/examples/page1/2`, `/user/jack`, `/files/docs/intro`. This is how you build pagination, detail pages, and anything keyed by an id or slug, and it follows the same paired-folder convention as every other [page](pages.md).

![A URL is matched most-specific-first (static, then [id], then catch-all); the winning page receives the params in the tsx via useParams and in Go via ParamsMsg and App.Params.](dynamic-routes-resolve.svg)

## The convention

Name a page folder with brackets, NextJS-style:

- `[id]` matches exactly **one** path segment. `pages/examples/page1/[id]` serves `/examples/page1/1`, `/examples/page1/2`, ... but not `/examples/page1/a/b`.
- `[...slug]` is a **catch-all**: it matches one or more trailing segments. `pages/files/[...slug]` serves `/files/a`, `/files/docs/intro`, `/files/a/b/c`. It needs at least one segment - `/files` alone does not match.

The paired files sit inside the bracket folder and share its name, exactly like any other pair:

```
pages/
  examples/
    page1/
      [id]/
        [id].tsx      -> the React half
        [id].go       -> the (optional) Go half
  files/
    [...slug]/
        [...slug].tsx -> a catch-all, frontend-only here
```

## Which route wins

Gantry resolves a URL by scoring candidates so the most concrete always wins: an exact **static** route is tried first, then dynamic routes **most-specific first**, scored segment by segment (a literal outranks an `[id]`, which outranks a `[...slug]`). So you can put `pages/examples/page1/new/` next to `pages/examples/page1/[id]/` and `/examples/page1/new` still lands on the static page, while `/examples/page1/7` falls through to `[id]`. Likewise `[id]` beats `[...slug]` when both could match one segment.

## Reading the params in the tsx

`useParams()` returns the captured segments: a `string` for an `[id]`, a `string[]` for a `[...slug]`. Type it with the shape you expect.

```tsx
import { useParams, Link } from "gantry-web";

export default function Page() {
  const { id } = useParams<{ id: string }>();
  return (
    <div>
      <p>Showing item {id}</p>
      <Link to={`/examples/page1/${Number(id) + 1}`}>Next</Link>
    </div>
  );
}
```

For a catch-all, the value is the array of segments: `const { slug } = useParams<{ slug: string[] }>()` gives `["docs", "intro"]` on `/files/docs/intro`. On a static page `useParams()` is `{}`.

Navigate to a concrete URL like any other route - `navigate("/examples/page1/2")` or `<Link to="/examples/page1/2">` (see [Routing](routing.md)). The same page component stays mounted as the id changes; `useParams()` re-renders it with the new value, and Gantry re-announces the new params to the Go half.

## The Go half

A dynamic page can have a Go half like any other page. Because Go cannot import a folder named `[id]`, gantry copies the Go half into an importable package under `internal/gantrydyn/` when it regenerates (`gantry dev`, `gantry build`, `gantry gen`). Two rules make that work:

1. Start the file with `//go:build ignore`. That keeps `go build ./...` and your editor from choking on the un-importable original; gantry strips the line from the copy it generates. (Your app's own build - `gantry dev`/`build`, which run `go build .` - never touches the bracket folder, only the generated copy.)
2. Set `Key` to the bracket path, matching the tsx: `Key: "pages/examples/page1/[id]"`. The mirror lands in a package Gantry names for you, so the `Key` is the only place the bracket path is spelled out - it must be exact.

A `Model` receives `ui.ParamsMsg` whenever the page activates **and every time the concrete param changes for the same page key** (`/1` -> `/2`), so it always knows which one is open:

```go
//go:build ignore

package dynid

import (
	"fmt"

	"github.com/BlueBeard63/Gantry/ui"
)

var Page = ui.Page{
	Key:   "pages/examples/page1/[id]",
	Model: func() ui.Model { return model{} },
}

type model struct{ id string }

func (m model) Init() ui.Cmd { return nil }

func (m model) Update(msg ui.Msg) (ui.Model, ui.Cmd) {
	if p, ok := msg.(ui.ParamsMsg); ok {
		m.id = p.Params.Get("id") // "7"; a [...slug] joins as "a/b/c" - use .List for the slice
	}
	return m, nil
}

func (m model) View() ui.Node {
	return ui.Text(fmt.Sprintf("item %s", m.id))
}
```

`ui.ParamsMsg` wraps a `ui.RouteParams` (`map[string][]string`, every value normalized to a slice). Read it two ways: `Params.Get("id")` joins the segments with `/` (a single `[id]` gives `"7"`, a `[...slug]` gives `"a/b/c"`), and `Params.List("slug")` returns the `[]string` - the natural read for a catch-all.

The currently active page's params are also available off the `*App`, for a `Call`/`On` handler or app-level code that captured the app: `app.Param("id")` returns the joined value and `app.Params()` returns a copy of the whole `RouteParams`.

`internal/gantrydyn/` is generated - never edit it by hand; edit the `[id].go` source and let gantry regenerate. It is rebuilt from scratch on every gen (so a deleted or renamed `[id]` page never leaves a stale importable copy), with the `//go:build ignore` line stripped, the package clause rewritten to a sanitized letter-first name, and each bracketed file name (`[id].go`) renamed to something the Go tool will actually compile. One limitation falls out of the copy: the Go half **cannot rely on files colocated in its bracket folder** (a relative path or a `//go:embed` of a sibling) - put shared assets in `resources/` instead (see [Resources](resources.md)).

Frontend-only dynamic pages (a `[id].tsx`/`[...slug].tsx` with no `.go`) need none of this - they just work.

## Try it

The demo app has both an `[id]` page (`/examples/page1/1`, reached from the "Dynamic" nav link) and a `[...slug]` catch-all (`/files/docs/intro`, "Catch-all"). Run `gantry dev` in `examples/demo` and edit the id in the URL or click between the numbered links.
