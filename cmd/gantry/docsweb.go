package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"

	"github.com/BlueBeard63/Gantry/docs"
	"github.com/BlueBeard63/Gantry/internal/launch"
)

// serveDocsWeb renders the embedded docs to HTML, serves them from a
// loopback port, and opens the browser. It is the default for
// `gantry docs`; the terminal viewer stays reachable behind -tui.
func serveDocsWeb(pages []docPage, start int, aiOn bool, port int) error {
	site, err := newDocsSite(pages, aiOn)
	if err != nil {
		return err
	}

	// With --ai on the Ollama backend, self-provision the model in the
	// background: the docs serve immediately; pull progress streams to the
	// terminal. Agent-CLI backends (claude/codex) need no model.
	if aiOn && site.aiCfg.backend == "ollama" {
		go ensureOllamaModel(site.aiCfg)
	}

	// Prefer the requested (stable) port so the URL stays put across runs; fall
	// back to an ephemeral port only if it is taken (e.g. another docs viewer).
	ln, err := launch.Listen(port)
	if err != nil && port != 0 {
		info("docs port %d is busy - using an ephemeral port instead", port)
		ln, err = launch.Listen(0)
	}
	if err != nil {
		return err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://127.0.0.1:%d", actualPort)

	// If a topic was matched, deep-link straight to that page.
	openURL := base + site.firstRoute
	if start >= 0 && start < len(pages) {
		if _, ok := site.pages[routeFor(pages[start].path)]; ok {
			openURL = base + routeFor(pages[start].path)
		}
	}

	srv := &http.Server{Handler: site.handler()}
	go func() { _ = srv.Serve(ln) }()

	info("serving docs at %s", base)
	if err := launch.OpenInBrowser(openURL); err != nil {
		info("open %s in your browser", openURL)
	}
	info("press Ctrl+C to stop")

	// Block forever; the process exits on Ctrl+C.
	select {}
}

// docsSite holds everything the HTTP handlers need: the pre-rendered
// pages keyed by route, the manifest that drives the nav, the shell
// template and the pre-built search index.
type docsSite struct {
	tmpl         *template.Template
	manifest     docsManifest
	pages        map[string]renderedPage
	assets       map[string][]byte // inline image assets (svg/png) by route
	firstRoute   string
	version      string
	searchJSON   []byte
	highlightCSS template.CSS

	// docs assistant (opt-in via `gantry docs --ai`)
	aiOn   bool
	aiCfg  aiConfig
	aiDocs []aiDoc // every page, for retrieval
	aiTOC  string  // "- Title (/route) - Category" per page
}

// renderedPage is one doc page turned into HTML plus the metadata the
// shell needs around it.
type renderedPage struct {
	Route         string
	PageTitle     string
	CategoryTitle string
	Body          template.HTML
	TOC           []tocEntry
	plain         string
}

type tocEntry struct {
	ID    string
	Text  string
	Level int // 2 or 3
}

// --- manifest -------------------------------------------------------------

type docsManifest struct {
	Categories []manifestCategory `json:"categories"`
}

type manifestCategory struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	Entries []manifestEntry `json:"entries"`
}

// manifestEntry is either a page ({path,title}) or a sub-category group
// ({group,pages:[...]}) that nests pages under a collapsible heading.
type manifestEntry struct {
	Path  string          `json:"path"`
	Title string          `json:"title"`
	Group string          `json:"group"`
	Pages []manifestEntry `json:"pages"`
}

func (e manifestEntry) isGroup() bool { return e.Group != "" }

// --- build ----------------------------------------------------------------

func newDocsSite(pages []docPage, aiOn bool) (*docsSite, error) {
	data, err := docs.FS.ReadFile("manifest.json")
	if err != nil {
		return nil, fmt.Errorf("reading docs manifest: %w", err)
	}
	var mf docsManifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parsing docs manifest: %w", err)
	}

	// Append one sidebar category per installed module. Best-effort: a bad
	// module registry must not break the framework docs.
	if cats, err := moduleNavCategories(); err != nil {
		warn("skipping module docs nav: %v", err)
	} else {
		mf.Categories = append(mf.Categories, cats...)
	}

	tmpl, err := template.New("docs").Parse(docsShellHTML)
	if err != nil {
		return nil, err
	}

	md := goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			// Class-based highlighting: no inline styles, so a light and a
			// dark chroma stylesheet can flip with the theme toggle.
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)

	hlCSS, err := buildHighlightCSS()
	if err != nil {
		return nil, err
	}

	// category id -> title, so breadcrumbs/search show the friendly name.
	catTitle := map[string]string{}
	// page path -> nav title (from the manifest, richer than the H1).
	navTitle := map[string]string{}
	// page path -> owning category title.
	pageCat := map[string]string{}
	var walk func(catT string, entries []manifestEntry)
	walk = func(catT string, entries []manifestEntry) {
		for _, e := range entries {
			if e.isGroup() {
				walk(catT, e.Pages)
				continue
			}
			navTitle[e.Path] = e.Title
			pageCat[e.Path] = catT
		}
	}
	for _, c := range mf.Categories {
		catTitle[c.ID] = c.Title
		walk(c.Title, c.Entries)
	}

	site := &docsSite{
		tmpl:         tmpl,
		manifest:     mf,
		pages:        map[string]renderedPage{},
		version:      docsDisplayVersion(),
		highlightCSS: hlCSS,
		aiOn:         aiOn,
		aiCfg:        newAIConfig(),
	}

	// Inline image assets (SVGs) by route: the framework-embedded ones plus any
	// installed module's cached images (best-effort for modules). Built before
	// the pages loop so page rendering can inline them.
	site.assets = map[string][]byte{}
	_ = fs.WalkDir(docs.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".md") || p == "manifest.json" {
			return err
		}
		if b, err := docs.FS.ReadFile(p); err == nil {
			site.assets["/"+p] = b
		}
		return nil
	})
	if ma, err := moduleDocAssets(); err != nil {
		warn("skipping module docs images: %v", err)
	} else {
		for k, v := range ma {
			site.assets[k] = v
		}
	}

	// Pre-render every embedded page. Pages not named in the manifest
	// (e.g. README.md) still render and are reachable by route; they just
	// have no nav row.
	var searchDocs []searchDoc
	for _, p := range pages {
		rp, err := renderDocPage(md, p)
		if err != nil {
			return nil, fmt.Errorf("rendering %s: %w", p.path, err)
		}
		// Never let a module page (appended after the core pages) shadow a
		// framework route.
		if _, dup := site.pages[rp.Route]; dup {
			warn("skipping duplicate doc route %s (from %s)", rp.Route, p.path)
			continue
		}
		if t := navTitle[p.path]; t != "" {
			rp.PageTitle = t
		}
		if c := pageCat[p.path]; c != "" {
			rp.CategoryTitle = c
		} else {
			rp.CategoryTitle = catTitle[p.category]
		}
		// Inline any diagram SVGs so they inherit the page theme (light/dark).
		rp.Body = template.HTML(inlineDiagrams(string(rp.Body), rp.Route, site.assets)) //nolint:gosec // svg from our own embedded/cached assets
		site.pages[rp.Route] = rp
		searchDocs = append(searchDocs, searchDoc{
			Title:    rp.PageTitle,
			Route:    rp.Route,
			Category: rp.CategoryTitle,
			Text:     rp.plain,
		})
		site.aiDocs = append(site.aiDocs, aiDoc{
			Title:    rp.PageTitle,
			Route:    rp.Route,
			Category: rp.CategoryTitle,
			Raw:      p.raw,
			Plain:    strings.ToLower(rp.plain),
		})
	}

	// Compact index of every page, always sent to the assistant so it can
	// link to any page even when a page's full text was not retrieved.
	var toc strings.Builder
	for _, d := range site.aiDocs {
		if d.Category != "" {
			fmt.Fprintf(&toc, "- %s (%s) - %s\n", d.Title, d.Route, d.Category)
		} else {
			fmt.Fprintf(&toc, "- %s (%s)\n", d.Title, d.Route)
		}
	}
	site.aiTOC = toc.String()

	site.firstRoute = firstManifestRoute(mf)
	if site.firstRoute == "" {
		site.firstRoute = routeFor(pages[0].path)
	}

	site.searchJSON, err = json.Marshal(searchDocs)
	if err != nil {
		return nil, err
	}
	return site, nil
}

// firstManifestRoute is the route of the first real page in reading
// order, drilling into the first group if the first entry is one.
func firstManifestRoute(mf docsManifest) string {
	var first func(entries []manifestEntry) string
	first = func(entries []manifestEntry) string {
		for _, e := range entries {
			if e.isGroup() {
				if r := first(e.Pages); r != "" {
					return r
				}
				continue
			}
			return routeFor(e.Path)
		}
		return ""
	}
	for _, c := range mf.Categories {
		if r := first(c.Entries); r != "" {
			return r
		}
	}
	return ""
}

// --- markdown rendering ---------------------------------------------------

// renderDocPage turns one page's markdown into HTML, rewriting intra-doc
// links to app routes, harvesting the h2/h3 TOC, and collecting plain
// text for the search index.
func renderDocPage(md goldmark.Markdown, p docPage) (renderedPage, error) {
	source := []byte(p.raw)
	reader := text.NewReader(source)
	doc := md.Parser().Parse(reader)
	pageDir := path.Dir(p.path)

	var toc []tocEntry
	var plain strings.Builder
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := n.(type) {
		case *ast.Link:
			node.Destination = []byte(rewriteDocLink(string(node.Destination), pageDir))
		case *ast.Heading:
			if node.Level == 2 || node.Level == 3 {
				id := ""
				if v, ok := node.AttributeString("id"); ok {
					if b, ok := v.([]byte); ok {
						id = string(b)
					}
				}
				toc = append(toc, tocEntry{ID: id, Text: nodeText(node, source), Level: node.Level})
			}
		case *ast.Text:
			plain.Write(node.Segment.Value(source))
			plain.WriteByte(' ')
		}
		return ast.WalkContinue, nil
	})

	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, source, doc); err != nil {
		return renderedPage{}, err
	}

	return renderedPage{
		Route:     routeFor(p.path),
		PageTitle: p.title,
		Body:      template.HTML(buf.String()), //nolint:gosec // goldmark escapes raw HTML (no WithUnsafe)
		TOC:       toc,
		plain:     plain.String(),
	}, nil
}

// nodeText concatenates the plain text of a node's descendants.
func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			b.Write(t.Segment.Value(source))
		} else {
			b.WriteString(nodeText(c, source))
		}
	}
	return b.String()
}

// routeFor maps a doc path to its app route: "advanced/args.md" ->
// "/advanced/args", "README.md" -> "/README".
func routeFor(docPath string) string {
	return "/" + strings.TrimSuffix(docPath, ".md")
}

// rewriteDocLink turns a markdown link that points at another doc into
// the matching app route, resolving "../"-relative paths against the
// current page's folder and preserving any "#anchor" fragment. External
// links, mailto and bare anchors pass through unchanged.
func rewriteDocLink(dest, pageDir string) string {
	if dest == "" || strings.HasPrefix(dest, "#") {
		return dest
	}
	if strings.Contains(dest, "://") || strings.HasPrefix(dest, "mailto:") {
		return dest
	}
	frag := ""
	if i := strings.IndexByte(dest, '#'); i >= 0 {
		frag = dest[i:]
		dest = dest[:i]
	}
	if dest == "" {
		return frag
	}
	if !strings.HasSuffix(dest, ".md") {
		return dest + frag // asset or unknown target; leave as-is
	}
	resolved := path.Clean(path.Join(pageDir, dest))
	return routeFor(resolved) + frag
}

// docsDisplayVersion is the string for the header pill: the release tag
// when this is a tagged build, "dev" otherwise.
func docsDisplayVersion() string {
	if v := taggedVersion(); v != "" {
		return v
	}
	return "dev"
}

// --- syntax highlighting CSS ----------------------------------------------

// buildHighlightCSS renders two chroma stylesheets (light + dark) into one
// blob: the light theme applies by default, the dark theme is scoped so it
// takes over under [data-theme=dark] and, via prefers-color-scheme, when
// the OS is dark and the user has not forced light. Both use class-based
// output (WithClasses), matching highlighting.WithFormatOptions above.
func buildHighlightCSS() (template.CSS, error) {
	light, err := chromaCSS("github")
	if err != nil {
		return "", err
	}
	dark, err := chromaCSS("github-dark")
	if err != nil {
		return "", err
	}
	// Scope BOTH themes mutually-exclusively, not light-as-default. chroma's
	// two styles color different token sets, so if the light sheet applied
	// unscoped it would leave its dark punctuation/name colors on tokens that
	// github-dark leaves to inherit - invisible dark-on-dark code in dark
	// mode. Mirrors the --var scheme: a forced theme wins, else the OS
	// preference decides, and the two never both apply.
	var b strings.Builder
	// Light: forced light, or OS-light and not forced dark.
	b.WriteString(scopeCSS(light, ":root[data-theme=light]"))
	b.WriteString("\n@media (prefers-color-scheme: light) {\n")
	b.WriteString(scopeCSS(light, ":root:not([data-theme=dark])"))
	b.WriteString("\n}\n")
	// Dark: forced dark, or OS-dark and not forced light.
	b.WriteString(scopeCSS(dark, ":root[data-theme=dark]"))
	b.WriteString("\n@media (prefers-color-scheme: dark) {\n")
	b.WriteString(scopeCSS(dark, ":root:not([data-theme=light])"))
	b.WriteString("\n}\n")
	return template.CSS(b.String()), nil //nolint:gosec // generated by chroma from a named built-in style
}

// chromaCSS renders one chroma style as class-based CSS.
func chromaCSS(styleName string) (string, error) {
	var b strings.Builder
	f := chromahtml.New(chromahtml.WithClasses(true))
	if err := f.WriteCSS(&b, styles.Get(styleName)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// scopeCSS prefixes every selector in a stylesheet with scope (e.g.
// ":root[data-theme=dark]") so a second theme's identical chroma classes
// only bite under that ancestor. Leading /* comments */ chroma emits are
// preserved.
func scopeCSS(css, scope string) string {
	var b strings.Builder
	for _, rule := range strings.SplitAfter(css, "}") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		brace := strings.Index(rule, "{")
		if brace < 0 {
			b.WriteString(rule + "\n")
			continue
		}
		head, body := rule[:brace], rule[brace:]
		comment := ""
		if i := strings.LastIndex(head, "*/"); i >= 0 {
			comment, head = head[:i+2]+" ", head[i+2:]
		}
		parts := strings.Split(head, ",")
		for i, p := range parts {
			parts[i] = scope + " " + strings.TrimSpace(p)
		}
		b.WriteString(comment + strings.Join(parts, ", ") + " " + body + "\n")
	}
	return b.String()
}

// --- nav view model -------------------------------------------------------

type navPage struct {
	Title  string
	Route  string
	Active bool
}

type navGroup struct {
	Title    string
	Pages    []navPage
	Expanded bool
}

type navEntry struct {
	Page  *navPage
	Group *navGroup
}

type navCategory struct {
	Title   string
	Entries []navEntry
}

// buildNav renders the manifest into the sidebar view model, marking the
// current page active and expanding the group that contains it.
func (s *docsSite) buildNav(current string) []navCategory {
	var cats []navCategory
	for _, c := range s.manifest.Categories {
		nc := navCategory{Title: c.Title}
		for _, e := range c.Entries {
			if e.isGroup() {
				g := &navGroup{Title: e.Group}
				for _, sub := range e.Pages {
					if sub.isGroup() {
						continue // one level of nesting is enough for v1
					}
					r := routeFor(sub.Path)
					p := navPage{Title: sub.Title, Route: r, Active: r == current}
					if p.Active {
						g.Expanded = true
					}
					g.Pages = append(g.Pages, p)
				}
				nc.Entries = append(nc.Entries, navEntry{Group: g})
				continue
			}
			r := routeFor(e.Path)
			nc.Entries = append(nc.Entries, navEntry{Page: &navPage{Title: e.Title, Route: r, Active: r == current}})
		}
		cats = append(cats, nc)
	}
	return cats
}

// --- HTTP -----------------------------------------------------------------

// pageData is the template payload for one rendered page.
type pageData struct {
	SiteVersion   string
	GitHubURL     string
	PageTitle     string
	CategoryTitle string
	Content       template.HTML
	Logo          template.HTML
	HighlightCSS  template.CSS
	Nav           []navCategory
	TOC           []tocEntry
	HasTOC        bool
	AIEnabled     bool
}

func (s *docsSite) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(s.searchJSON)
	})
	if s.aiOn {
		mux.HandleFunc("/api/ask", s.handleAsk)
		mux.HandleFunc("/api/ai", s.handleAIStatus)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, s.firstRoute, http.StatusFound)
			return
		}
		if b, ok := s.assets[r.URL.Path]; ok {
			w.Header().Set("Content-Type", assetContentType(r.URL.Path))
			_, _ = w.Write(b)
			return
		}
		rp, ok := s.pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data := pageData{
			SiteVersion:   s.version,
			GitHubURL:     "https://github.com/BlueBeard63/Gantry",
			PageTitle:     rp.PageTitle,
			CategoryTitle: rp.CategoryTitle,
			Content:       rp.Body,
			Logo:          template.HTML(docsLogoSVG), //nolint:gosec // trusted inline literal
			HighlightCSS:  s.highlightCSS,
			Nav:           s.buildNav(rp.Route),
			TOC:           rp.TOC,
			HasTOC:        len(rp.TOC) > 0,
			AIEnabled:     s.aiOn,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.tmpl.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

// searchDoc is one entry in the /search.json index.
type searchDoc struct {
	Title    string `json:"title"`
	Route    string `json:"route"`
	Category string `json:"category"`
	Text     string `json:"text"`
}

var docImgRe = regexp.MustCompile(`<img\b[^>]*>`)
var imgSrcRe = regexp.MustCompile(`\bsrc="([^"]*)"`)

// inlineDiagrams replaces <img> tags that point at an SVG asset with the SVG
// inlined into the page, tagged `class="diagram"`. Inlining (rather than a
// plain <img>) lets the diagram inherit the page's theme, so it follows the
// light/dark toggle. Non-SVG images and unknown sources are left untouched.
func inlineDiagrams(html, pageRoute string, assets map[string][]byte) string {
	dir := path.Dir(pageRoute)
	return docImgRe.ReplaceAllStringFunc(html, func(tag string) string {
		m := imgSrcRe.FindStringSubmatch(tag)
		if m == nil || !strings.HasSuffix(m[1], ".svg") {
			return tag
		}
		route := m[1]
		if !strings.HasPrefix(route, "/") {
			route = path.Clean(path.Join(dir, route))
		}
		if svg, ok := assets[route]; ok {
			return strings.Replace(string(svg), "<svg ", `<svg class="diagram" `, 1)
		}
		return tag
	})
}

// assetContentType maps an image asset route to its MIME type.
func assetContentType(p string) string {
	switch {
	case strings.HasSuffix(p, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(p, ".gif"):
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}
