// Package controlplane provides the embedded Control Plane SPA handler.
package controlplane

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded dist directory
// containing the self-contained Control Plane SPA dashboard.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
