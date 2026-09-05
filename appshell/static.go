package appshell

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// ServeSPA serves an embedded frontend build: real files (assets) are
// served as-is, and every other path falls back to index.html so
// client-side routes like /settings work on reload and in widget or
// popup windows.
func ServeSPA(dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			// Unknown path: fall back to index.html for client-side routes.
			r.URL.Path = "/"
			p = "index.html"
		}
		// Vite writes content-hashed files under assets/, so they can be
		// cached forever; index.html (and the SPA fallback) must revalidate
		// so an updated build is picked up. This mainly helps popup/widget
		// windows and webview reloads, which re-fetch the shell.
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}
