// Package web serves the HTML test client.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the client. If dir is non-empty, files are read from disk
// there instead of the embedded copy, so edits show up without rebuilding.
func Handler(dir string) http.Handler {
	if dir != "" {
		return noCache(http.FileServer(http.Dir(dir)))
	}
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return noCache(http.FileServer(http.FS(sub)))
}

// noCache keeps browsers from running a stale client after an upgrade.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
