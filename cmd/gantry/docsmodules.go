package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// This file folds installed-module docs into the same corpus the framework
// docs use. A module's pages are represented as docPages whose path is
// "<namespace>/<relpath>", so routeFor already yields the namespaced route
// (/whitegantry/notifications); the merge then just appends those pages and
// one nav category per module. See design/modules-and-docs.md.

// moduleDocsManifest is docs/manifest.json inside a module: the reading-order
// nav for that module's section, reusing the core page/group entry shape.
type moduleDocsManifest struct {
	Title      string             `json:"title"`
	Entries    []manifestEntry    `json:"entries"`
	Categories []manifestCategory `json:"categories"`
}

// docModule is one installed module that ships docs, with its cache docs dir.
type docModule struct {
	entry   moduleEntry
	docsDir string
}

// installedDocModules returns registry entries that declare docs and whose
// cached docs directory is present.
func installedDocModules() ([]docModule, error) {
	reg, err := loadRegistry()
	if err != nil {
		return nil, err
	}
	cacheDir, err := modulesCacheDir()
	if err != nil {
		return nil, err
	}
	var out []docModule
	for _, m := range reg.Modules {
		if m.DocsPrefix == "" {
			continue // no docs capability
		}
		dir := filepath.Join(cacheDir, m.Namespace+"@"+m.Version, "docs")
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		out = append(out, docModule{entry: m, docsDir: dir})
	}
	return out, nil
}

// moduleDocPages loads every installed module's markdown as namespaced
// docPages. Best-effort: it is called from loadDocs, which must still serve the
// framework docs if a module's cache is unreadable.
func moduleDocPages() ([]docPage, error) {
	mods, err := installedDocModules()
	if err != nil {
		return nil, err
	}
	var pages []docPage
	for _, m := range mods {
		dirFS := os.DirFS(m.docsDir)
		err := fs.WalkDir(dirFS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return err
			}
			data, rerr := fs.ReadFile(dirFS, p)
			if rerr != nil {
				return rerr
			}
			raw := string(data)
			pages = append(pages, docPage{
				path:     m.entry.Namespace + "/" + p, // p is forward-slashed (DirFS)
				title:    firstH1(raw, p),
				category: m.entry.Namespace,
				raw:      raw,
				lower:    strings.ToLower(raw),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return pages, nil
}

// moduleDocAssets returns each installed module's non-markdown docs files
// (images) keyed by their namespaced route, so the viewer can serve them.
func moduleDocAssets() (map[string][]byte, error) {
	mods, err := installedDocModules()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, m := range mods {
		dirFS := os.DirFS(m.docsDir)
		err := fs.WalkDir(dirFS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, ".md") || p == "manifest.json" {
				return err
			}
			b, rerr := fs.ReadFile(dirFS, p)
			if rerr != nil {
				return rerr
			}
			out["/"+m.entry.Namespace+"/"+p] = b
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// moduleNavCategories builds one sidebar category per module: from its
// docs/manifest.json when present (entries, or categories flattened to
// groups), else derived from the folder layout.
func moduleNavCategories() ([]manifestCategory, error) {
	mods, err := installedDocModules()
	if err != nil {
		return nil, err
	}
	var cats []manifestCategory
	for _, m := range mods {
		title := m.entry.Title
		entries := moduleNavEntries(m, &title)
		cats = append(cats, manifestCategory{ID: m.entry.Namespace, Title: title, Entries: entries})
	}
	return cats, nil
}

func moduleNavEntries(m docModule, title *string) []manifestEntry {
	ns := m.entry.Namespace
	data, err := os.ReadFile(filepath.Join(m.docsDir, "manifest.json"))
	if err != nil {
		return folderNavEntries(ns, m.docsDir) // no manifest: derive from folders
	}
	var md moduleDocsManifest
	if json.Unmarshal(data, &md) != nil {
		return folderNavEntries(ns, m.docsDir)
	}
	if md.Title != "" {
		*title = md.Title
	}
	switch {
	case len(md.Entries) > 0:
		return prefixEntries(ns, md.Entries)
	case len(md.Categories) > 0:
		// Each declared category becomes a group under the module's section.
		var out []manifestEntry
		for _, c := range md.Categories {
			out = append(out, manifestEntry{Group: c.Title, Pages: prefixEntries(ns, c.Entries)})
		}
		return out
	default:
		return folderNavEntries(ns, m.docsDir)
	}
}

// prefixEntries rewrites every page path to "<ns>/<path>" so it matches the
// namespaced docPage paths (and thus routeFor), recursing into groups.
func prefixEntries(ns string, entries []manifestEntry) []manifestEntry {
	out := make([]manifestEntry, 0, len(entries))
	for _, e := range entries {
		if e.isGroup() {
			out = append(out, manifestEntry{Group: e.Group, Pages: prefixEntries(ns, e.Pages)})
			continue
		}
		e.Path = ns + "/" + strings.TrimPrefix(e.Path, "/")
		out = append(out, e)
	}
	return out
}

// folderNavEntries derives nav from the layout: top-level pages listed first,
// then each subfolder as a group of its pages (one level deep), so a module
// needs no manifest to get sensible, routing-like grouping.
func folderNavEntries(ns, docsDir string) []manifestEntry {
	top, err := os.ReadDir(docsDir)
	if err != nil {
		return nil
	}
	var pages, groups []manifestEntry
	for _, e := range top {
		switch {
		case e.IsDir():
			sub, _ := os.ReadDir(filepath.Join(docsDir, e.Name()))
			var gp []manifestEntry
			for _, f := range sub {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") {
					gp = append(gp, manifestEntry{Path: ns + "/" + e.Name() + "/" + f.Name()})
				}
			}
			if len(gp) > 0 {
				groups = append(groups, manifestEntry{Group: title(e.Name()), Pages: gp})
			}
		case strings.HasSuffix(e.Name(), ".md"):
			pages = append(pages, manifestEntry{Path: ns + "/" + e.Name()})
		}
	}
	return append(pages, groups...)
}

// firstH1 returns the first markdown H1, or fallback when there is none.
func firstH1(raw, fallback string) string {
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return fallback
}

// --- namespace filtering (for the MCP tools) --------------------------------

// aiDocsIn returns the loaded pages whose route falls under a top-level
// section/namespace (e.g. "whitegantry" -> /whitegantry/...).
func (s *docsSite) aiDocsIn(ns string) []aiDoc {
	prefix := "/" + ns + "/"
	var out []aiDoc
	for _, d := range s.aiDocs {
		if strings.HasPrefix(d.Route, prefix) {
			out = append(out, d)
		}
	}
	return out
}

// tocFor is the list_docs table, optionally restricted to one namespace.
func (s *docsSite) tocFor(ns string) string {
	if ns == "" {
		return s.aiTOC
	}
	var b strings.Builder
	for _, d := range s.aiDocsIn(ns) {
		if d.Category != "" {
			fmt.Fprintf(&b, "- %s (%s) - %s\n", d.Title, d.Route, d.Category)
		} else {
			fmt.Fprintf(&b, "- %s (%s)\n", d.Title, d.Route)
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("No pages under namespace %q. Call list_docs with no namespace to see every section.\n", ns)
	}
	return b.String()
}
