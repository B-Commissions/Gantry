// The Gantry Vite plugin. Plain JavaScript on purpose: vite.config is
// executed by Node, which cannot load TypeScript out of node_modules.
//
// What it does:
//   - scans <appRoot>/pages/** and <appRoot>/components/** for paired
//     <name>/<name>.tsx files and generates the virtual:gantry-app
//     module (route table + component registry, keyed by folder path)
//   - auto-imports <appRoot>/index.css and every colocated same-name
//     .css (root css first, so per-pair css can override variables)
//   - injects the pairing key into bare usePaired() calls, derived from
//     the file's folder path, so the tsx never repeats it
//   - proxies /api and /gantry/ws to the Go server during gantry dev
//   - watches pages/ and components/ so adding or removing a pair
//     regenerates the app without restarting the dev server

import fs from "node:fs";
import path from "node:path";

const VIRTUAL_ID = "virtual:gantry-app";
const RESOLVED_ID = "\0" + VIRTUAL_ID;

// Content types for the dev /resources/ middleware. Keep in rough sync
// with the extensions developers drop in resources/ (images, fonts,
// data). Unknown types fall back to a generic binary stream.
const RESOURCE_MIME = {
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".gif": "image/gif",
  ".webp": "image/webp",
  ".svg": "image/svg+xml",
  ".ico": "image/x-icon",
  ".avif": "image/avif",
  ".woff": "font/woff",
  ".woff2": "font/woff2",
  ".ttf": "font/ttf",
  ".otf": "font/otf",
  ".json": "application/json",
  ".txt": "text/plain; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".wasm": "application/wasm",
  ".mp3": "audio/mpeg",
  ".mp4": "video/mp4",
  ".webm": "video/webm",
  ".pdf": "application/pdf",
};

function resourceMime(file) {
  return RESOURCE_MIME[path.extname(file).toLowerCase()] || "application/octet-stream";
}

/**
 * @param {{ appRoot?: string, goPort?: number }} [opts]
 * @returns {import("vite").Plugin}
 */
export function gantry(opts = {}) {
  /** @type {string} */
  let appRoot = "";
  let goPort = opts.goPort ?? 0;

  // Pairs nest to any depth: pages/account/settings/settings.tsx is
  // the pair "pages/account/settings" and serves /account/settings. A
  // folder both IS a pair and CONTAINS pairs when it wants to
  // (pages/account/account.tsx + pages/account/settings/...).
  function scanKind(kind) {
    const root = path.join(appRoot, kind);
    const out = [];
    if (!fs.existsSync(root)) return out;
    const walk = (dir) => {
      for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        if (!entry.isDirectory()) continue;
        const sub = path.join(dir, entry.name);
        const tsx = path.join(sub, entry.name + ".tsx");
        if (fs.existsSync(tsx)) {
          const rel = path.relative(root, sub).split(path.sep).join("/");
          const css = path.join(sub, entry.name + ".css");
          out.push({
            key: kind + "/" + rel,
            name: rel,
            tsx: tsx.split(path.sep).join("/"),
            css: fs.existsSync(css) ? css.split(path.sep).join("/") : null,
          });
        }
        walk(sub);
      }
    };
    walk(root);
    out.sort((a, b) => a.key.localeCompare(b.key));
    return out;
  }

  function generate() {
    const pages = scanKind("pages");
    const components = scanKind("components");
    const lines = [];

    const layouts = scanKind("layouts");

    const rootCss = path.join(appRoot, "index.css");
    if (fs.existsSync(rootCss)) {
      lines.push(`import ${JSON.stringify(rootCss.split(path.sep).join("/"))};`);
    }
    for (const item of [...layouts, ...pages, ...components]) {
      if (item.css) lines.push(`import ${JSON.stringify(item.css)};`);
    }

    layouts.forEach((l, i) => {
      lines.push(`import * as l${i} from ${JSON.stringify(l.tsx)};`);
    });

    // Optional root app.tsx: default-exports CreateAppOptions so apps
    // customize createApp (TitleBar slots, title placement, extra
    // components) without owning the synthesized entry file.
    const appTsx = path.join(appRoot, "app.tsx");
    if (fs.existsSync(appTsx)) {
      lines.push(`import * as appMod from ${JSON.stringify(appTsx.split(path.sep).join("/"))};`);
      lines.push(`export const appConfig = appMod;`);
    } else {
      lines.push(`export const appConfig = null;`);
    }

    pages.forEach((p, i) => {
      lines.push(`import * as p${i} from ${JSON.stringify(p.tsx)};`);
    });
    components.forEach((c, i) => {
      lines.push(`import * as c${i} from ${JSON.stringify(c.tsx)};`);
    });

    const pageEntries = pages.map((p, i) => {
      // The derived route only; an optional "export const route" on the
      // page module overrides it at runtime (createApp checks mod.route
      // there - referencing it here would make rollup warn on every
      // page that does not export it). Nested pages route by their
      // path; an "index" leaf maps to the parent:
      //   pages/index -> /, pages/account/settings -> /account/settings,
      //   pages/account/index -> /account
      let route;
      if (p.name === "index") {
        route = "/";
      } else if (p.name.endsWith("/index")) {
        route = "/" + p.name.slice(0, -"/index".length);
      } else {
        route = "/" + p.name;
      }
      return `{ key: ${JSON.stringify(p.key)}, route: ${JSON.stringify(route)}, mod: p${i} }`;
    });
    lines.push(`export const pages = [${pageEntries.join(", ")}];`);

    const compEntries = components.map((c, i) => `${JSON.stringify(c.key)}: c${i}`);
    lines.push(`export const components = { ${compEntries.join(", ")} };`);

    // Layouts are keyed by their short name: layouts/main -> "main",
    // which is what pages reference in "export const layout".
    const layoutEntries = layouts.map((l, i) => `${JSON.stringify(l.name)}: l${i}`);
    lines.push(`export const layouts = { ${layoutEntries.join(", ")} };`);
    lines.push(`export const singlePage = ${pages.length <= 1};`);
    return lines.join("\n");
  }

  /** Derive the pairing key when id is a paired tsx/css file. */
  function keyFor(id) {
    const file = id.split("?")[0].split(path.sep).join("/");
    const root = appRoot.split(path.sep).join("/");
    if (!file.startsWith(root + "/")) return null;
    const rel = file.slice(root.length + 1);
    const m = rel.match(/^(pages|components|layouts)\/(.+)\/([^/]+)\.tsx$/);
    if (!m) return null;
    const segs = m[2].split("/");
    if (segs[segs.length - 1] !== m[3]) return null;
    return m[1] + "/" + m[2];
  }

  return {
    name: "gantry",

    config(userConfig, { command }) {
      const cfgRoot = userConfig.root
        ? path.resolve(userConfig.root)
        : process.cwd();
      appRoot = path.resolve(cfgRoot, opts.appRoot ?? "..");

      if (!goPort) {
        try {
          const cfg = JSON.parse(fs.readFileSync(path.join(appRoot, "gantry.json"), "utf8"));
          goPort = cfg.port ?? 8330;
        } catch {
          goPort = 8330;
        }
      }
      const target = "http://127.0.0.1:" + goPort;

      return {
        // gantry-web MUST stay prebundlable (which is why it never
        // imports virtual:gantry-app - esbuild cannot resolve virtual
        // ids). A registry install is bundled into one optimized module;
        // do NOT add it to optimizeDeps.exclude: served as raw
        // node_modules source, Vite can reference the same file under
        // two URLs (?v=hash vs bare) and instantiate the package twice,
        // splitting the router's subscribers so navigation stops
        // rendering. An npm file: link (framework development) resolves
        // outside node_modules and is served as source automatically -
        // there the force-included deps below matter: the CJS ones
        // (react-dom/client) otherwise get served raw and lose their
        // named exports, and lucide-react would waterfall one request
        // per icon module.
        optimizeDeps: {
          include: [
            "react",
            "react-dom",
            "react-dom/client",
            "react/jsx-runtime",
            "react/jsx-dev-runtime",
            "lucide-react",
          ],
        },
        // dedupe makes imports inside the symlinked gantry-web source
        // resolve from the APP's node_modules (the package itself has
        // none) and guarantees a single React instance.
        resolve: { dedupe: ["react", "react-dom", "lucide-react"] },
        server:
          command === "serve"
            ? {
                proxy: {
                  "/api": { target },
                  "/gantry/ws": { target, ws: true },
                },
              }
            : undefined,
      };
    },

    resolveId(id) {
      if (id === VIRTUAL_ID) return RESOLVED_ID;
      return null;
    },

    load(id) {
      if (id === RESOLVED_ID) return generate();
      return null;
    },

    transform(code, id) {
      const key = keyFor(id);
      if (key && code.includes("usePaired()")) {
        return code.replaceAll("usePaired()", `usePaired(${JSON.stringify(key)})`);
      }
      return null;
    },

    configureServer(server) {
      // Serve /resources/<path> live off <appRoot>/resources during dev,
      // the same tree the Go side embeds and serves in production. Kept
      // off the Go server (no proxy) so edits appear without a rebuild.
      const resourcesDir = path.join(appRoot, "resources");
      server.middlewares.use((req, res, next) => {
        const url = (req.url || "").split("?")[0];
        if (!url.startsWith("/resources/")) return next();
        const rel = decodeURIComponent(url.slice("/resources/".length));
        const abs = path.join(resourcesDir, rel);
        // Contain the request inside resources/ (reject ../ traversal).
        const rootPrefix = resourcesDir + path.sep;
        if (abs !== resourcesDir && !abs.startsWith(rootPrefix)) {
          res.statusCode = 403;
          return res.end("Forbidden");
        }
        let stat;
        try {
          stat = fs.statSync(abs);
        } catch {
          return next();
        }
        if (!stat.isFile()) return next();
        res.setHeader("Content-Type", resourceMime(abs));
        res.setHeader("Content-Length", stat.size);
        fs.createReadStream(abs).pipe(res);
      });

      server.watcher.add(appRoot);
      const refresh = (file) => {
        const f = file.split(path.sep).join("/");
        const root = appRoot.split(path.sep).join("/");
        if (!f.startsWith(root)) return;
        if (
          !/\/(pages|components|layouts)\/.+\.(tsx|css)$/.test(f) &&
          !f.endsWith("/index.css") &&
          !f.endsWith("/app.tsx")
        ) {
          return;
        }
        const mod = server.moduleGraph.getModuleById(RESOLVED_ID);
        if (mod) server.moduleGraph.invalidateModule(mod);
        server.ws.send({ type: "full-reload" });
      };
      server.watcher.on("add", refresh);
      server.watcher.on("unlink", refresh);
    },
  };
}

export default gantry;
