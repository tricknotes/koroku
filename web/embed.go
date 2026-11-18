// Package web embeds the built Ember single-page app into the paasd binary.
package web

import (
	"embed"
	"io/fs"
)

// dist holds the output of `pnpm build`. Only a placeholder is tracked in
// git, so a binary built without running the frontend build serves no UI.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend with index.html at the root.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: "dist" is a valid path and is always embedded.
		panic(err)
	}
	return sub
}
