// Owner: Claude
package web

import "embed"

// Files holds the UI served at the root URL: the page, its script and the vendored uPlot files.
//
//go:embed index.html app.js uplot.min.js uplot.min.css
var Files embed.FS
