package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setupFakeModule redirects the config/cache dirs to temp locations and lays
// down a registered whitegantry module with three pages and a nav manifest
// (permissions.md is deliberately left out of the manifest).
func setupFakeModule(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	reg := moduleRegistry{Modules: []moduleEntry{{
		Namespace:  "whitegantry",
		Title:      "WhiteGantry",
		Module:     "github.com/BlueBeard63/WhiteGantry",
		Version:    "v0.2.0",
		Provides:   []string{"docs"},
		DocsPrefix: "/whitegantry",
	}}}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}

	cacheDir, err := modulesCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	docsDir := filepath.Join(cacheDir, "whitegantry@v0.2.0", "docs")
	mustWrite(t, filepath.Join(docsDir, "notifications.md"), "# Notifications\n\nHow WhiteGantry notifies.")
	mustWrite(t, filepath.Join(docsDir, "permissions.md"), "# Permissions\n\nBrokered scopes.")
	mustWrite(t, filepath.Join(docsDir, "guide", "setup.md"), "# Setup\n\nInstall and wire the module.")
	mustWrite(t, filepath.Join(docsDir, "manifest.json"), `{
      "title": "WhiteGantry",
      "entries": [
        { "path": "notifications.md", "title": "Notifications" },
        { "group": "Guides", "pages": [ { "path": "guide/setup.md", "title": "Setup" } ] }
      ]
    }`)
}

func TestModuleDocsMergeIntoSite(t *testing.T) {
	setupFakeModule(t)

	pages, err := loadDocs()
	if err != nil {
		t.Fatalf("loadDocs: %v", err)
	}
	// The module pages are present, namespaced.
	var haveNotif bool
	for _, p := range pages {
		if p.path == "whitegantry/notifications.md" {
			haveNotif = true
		}
	}
	if !haveNotif {
		t.Fatal("module page whitegantry/notifications.md not loaded")
	}

	site, err := newDocsSite(pages, false)
	if err != nil {
		t.Fatalf("newDocsSite: %v", err)
	}

	// Routes are namespaced and reachable, including the page NOT in the nav
	// manifest (permissions.md still renders, it just has no sidebar row).
	for _, route := range []string{"/whitegantry/notifications", "/whitegantry/permissions", "/whitegantry/guide/setup"} {
		if _, ok := site.pages[route]; !ok {
			t.Errorf("route %s not served", route)
		}
		if _, ok := site.docByRoute(route); !ok {
			t.Errorf("docByRoute(%s) missed", route)
		}
	}

	// A framework route is untouched.
	if _, ok := site.pages["/ui/pages"]; !ok {
		t.Error("framework route /ui/pages missing after merge")
	}

	// The sidebar gains one WhiteGantry section, ordered after the framework
	// categories, with the Guides group nesting setup.
	nav := site.buildNav("/whitegantry/notifications")
	if nav[len(nav)-1].Title != "WhiteGantry" {
		t.Errorf("last nav category = %q, want WhiteGantry", nav[len(nav)-1].Title)
	}
	wg := nav[len(nav)-1]
	var sawNotifActive, sawGuidesGroup, sawSetup bool
	for _, e := range wg.Entries {
		if e.Page != nil && e.Page.Title == "Notifications" && e.Page.Active {
			sawNotifActive = true
		}
		if e.Group != nil && e.Group.Title == "Guides" {
			sawGuidesGroup = true
			for _, p := range e.Group.Pages {
				if p.Route == "/whitegantry/guide/setup" {
					sawSetup = true
				}
			}
		}
	}
	if !sawNotifActive {
		t.Error("active Notifications page not in WhiteGantry nav")
	}
	if !sawGuidesGroup || !sawSetup {
		t.Errorf("Guides group / setup page missing (group=%v setup=%v)", sawGuidesGroup, sawSetup)
	}
}

func TestModuleNamespaceFilter(t *testing.T) {
	setupFakeModule(t)
	pages, err := loadDocs()
	if err != nil {
		t.Fatalf("loadDocs: %v", err)
	}
	site, err := newDocsSite(pages, false)
	if err != nil {
		t.Fatalf("newDocsSite: %v", err)
	}

	// list_docs namespace filter: only the module's three pages.
	in := site.aiDocsIn("whitegantry")
	if len(in) != 3 {
		t.Fatalf("aiDocsIn(whitegantry) = %d pages, want 3", len(in))
	}
	toc := site.tocFor("whitegantry")
	if !strings.Contains(toc, "/whitegantry/notifications") || strings.Contains(toc, "/ui/pages") {
		t.Errorf("tocFor(whitegantry) not scoped:\n%s", toc)
	}

	// search_docs namespace filter: a query that also matches framework pages
	// returns only the module's page when scoped.
	hits := retrieveFrom(site.aiDocsIn("whitegantry"), "setup install wire", 5)
	if len(hits) == 0 || !strings.HasPrefix(hits[0].Route, "/whitegantry/") {
		t.Errorf("scoped search did not return a whitegantry page: %+v", hits)
	}
}

func TestDocsServesImages(t *testing.T) {
	setupFakeModule(t)

	// Drop an image into the module's cached docs so the module-asset path
	// has something to serve.
	cacheDir, err := modulesCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"></svg>`
	mustWrite(t, filepath.Join(cacheDir, "whitegantry@v0.2.0", "docs", "diagram.svg"), svg)

	pages, err := loadDocs()
	if err != nil {
		t.Fatalf("loadDocs: %v", err)
	}
	site, err := newDocsSite(pages, false)
	if err != nil {
		t.Fatalf("newDocsSite: %v", err)
	}

	// Framework SVG is embedded and registered as an asset.
	if _, ok := site.assets["/cli/modules-flow.svg"]; !ok {
		t.Error("framework asset /cli/modules-flow.svg not registered")
	}
	// Module SVG is namespaced and registered.
	if got := string(site.assets["/whitegantry/diagram.svg"]); got != svg {
		t.Errorf("module asset content = %q", got)
	}

	// The handler serves both with the right content type.
	h := site.handler()
	for _, route := range []string{"/cli/modules-flow.svg", "/whitegantry/diagram.svg"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status %d", route, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "image/svg+xml" {
			t.Errorf("%s: content-type %q, want image/svg+xml", route, ct)
		}
		if rr.Body.Len() == 0 {
			t.Errorf("%s: empty body", route)
		}
	}
}
