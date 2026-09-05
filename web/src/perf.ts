// Lightweight startup/runtime timing for diagnosing lag. Enabled in
// development (Vite's import.meta.env.DEV) or with ?perf in the URL, so
// it can be switched on in a production build too. A no-op otherwise, so
// leaving perfMark() calls in place costs nothing when disabled.
//
// Marks are logged relative to performance.now() - i.e. milliseconds
// since the page began loading (the WebView2/WebKit navigation start),
// which is exactly the cold-start metric we care about.

function computeEnabled(): boolean {
  try {
    const meta = import.meta as unknown as { env?: { DEV?: boolean } };
    if (meta.env?.DEV) return true;
  } catch {
    // import.meta.env unavailable (non-Vite context) - fall through.
  }
  if (typeof location !== "undefined") {
    return new URLSearchParams(location.search).has("perf");
  }
  return false;
}

const enabled = computeEnabled();

/** perfEnabled reports whether timing marks are being logged. */
export function perfEnabled(): boolean {
  return enabled;
}

/** perfMark logs one timing point (ms since navigation start). No-op
 * when timing is disabled. */
export function perfMark(label: string): void {
  if (!enabled || typeof performance === "undefined") return;
  // eslint-disable-next-line no-console
  console.info(`perf: ${label}=${Math.round(performance.now())}ms`);
}
