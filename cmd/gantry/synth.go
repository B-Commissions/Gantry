package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeSynth regenerates the .gantry/ Vite build root. These files are
// synthesized on every dev/build run and never hand-edited - the app's
// own files are main.go, pages/, components/, index.css and package.json.
func writeSynth(appDir string, cfg appConfig) (string, error) {
	dir := filepath.Join(appDir, ".gantry")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	indexHTML := fmt.Sprintf(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>%s</title>
  </head>
  <body>
    <div id="root"></div>
    <script type="module" src="/main.tsx"></script>
  </body>
</html>
`, cfg.Title)

	// The virtual:gantry-app import lives HERE, not inside gantry-web:
	// only app code goes through the Vite plugin that resolves it, and
	// keeping it out of the package is what lets gantry-web be
	// prebundled as one module (see the plugin's optimizeDeps note).
	// styles.css comes first so the app's index.css can override the
	// --gantry-* variables.
	mainTSX := fmt.Sprintf(`// Synthesized by gantry - regenerated on every dev/build run.
import "gantry-web/styles.css";
import { createApp } from "gantry-web";
import * as app from "virtual:gantry-app";

createApp(app, { title: %q });
`, cfg.Title)

	// Tailwind is first-class: gantry.json's "tailwind" flag adds the
	// vite plugin here, so apps never need to own this file for it.
	twImport, twPlugin := "", ""
	if cfg.Tailwind {
		twImport = "import tailwindcss from \"@tailwindcss/vite\";\n"
		twPlugin = "tailwindcss(), "
	}
	viteConfig := fmt.Sprintf(`// Synthesized by gantry - regenerated on every dev/build run.
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
%simport { gantry } from "gantry-web/vite";

export default defineConfig({
  plugins: [%sreact(), gantry({ appRoot: ".." })],
  build: {
    outDir: "../webdist",
    emptyOutDir: true,
    // WebView2 (Edge/Chromium) and modern WebKitGTK both support ES2022;
    // a modern target ships smaller, faster-parsing output. Matches the
    // tsconfig target.
    target: "es2022",
    // No source maps in production output (they inflate the build and are
    // only useful for the dev server, which makes its own).
    sourcemap: false,
    rollupOptions: {
      output: {
        // Keep the React runtime in its own chunk: it rarely changes, so
        // it stays cached across navigations and app updates.
        manualChunks(id) {
          if (
            id.includes("/node_modules/react/") ||
            id.includes("/node_modules/react-dom/") ||
            id.includes("/node_modules/scheduler/")
          ) {
            return "react";
          }
        },
      },
    },
  },
});
`, twImport, twPlugin)

	files := map[string]string{
		"index.html":     indexHTML,
		"main.tsx":       mainTSX,
		"vite.config.ts": viteConfig,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}
