// Owner: Claude
package main

import (
	"net/http"

	"github.com/prabenzo/cmek/web"
)

// routeUI serves the embedded page at the root URL (index.html for "/"). The embedded files carry no modification
// time, so the file server sends no validators; Cache-Control: no-cache makes every load fetch the current build's
// page instead of a copy the browser kept from before a redeploy.
func routeUI(mux *http.ServeMux) {
	files := http.FileServerFS(web.Files)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
}
