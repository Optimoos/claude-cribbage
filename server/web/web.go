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

// csp lets the page load only its own script and stylesheet and talk only
// to its own server (fetch and the signaling WebSocket; 'self' covers ws:
// and wss: to the same host). The icon is a data: URL. Nothing needs
// inline script or style attributes, so injected markup could not run or
// restyle anything. WebRTC is not covered by CSP.
// pages-build.sh writes the same policy for a page hosted elsewhere.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// noCache keeps browsers from running a stale client after an upgrade.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy", csp)
		h.ServeHTTP(w, r)
	})
}
