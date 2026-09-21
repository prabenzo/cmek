// Owner: Claude
package main

import (
	"net/http"

	"github.com/prabenzo/cmek/web"
)

// routeUI serves the embedded page at the root URL (index.html for "/").
func routeUI(mux *http.ServeMux) {
	mux.Handle("GET /", http.FileServerFS(web.Files))
}
