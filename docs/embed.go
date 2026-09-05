// Package docs embeds the documentation tree so the gantry CLI can
// browse it offline (gantry docs).
package docs

import "embed"

// FS holds every docs page plus any inline image assets (SVGs). Paths are
// relative to this directory, e.g. "getting-started/first-app.md" or
// "cli/modules-flow.svg".
//
//go:embed README.md */*.md */*.svg manifest.json
var FS embed.FS
