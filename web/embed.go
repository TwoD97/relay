package web

import "embed"

// Dist contains the complete browser application. Run npm run build to update it.
//
//go:embed dist
var Dist embed.FS
