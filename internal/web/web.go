// Package web embeds the portal UI, including the API viewer bundle.
//
// The viewer is vendored rather than loaded from a CDN because this portal is
// meant to run on an internal network, where outbound access to a CDN is often
// blocked — and a documentation portal that needs the public internet to render
// documentation is not much of a portal.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// FS returns the UI file system rooted at the static directory.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	return sub
}
