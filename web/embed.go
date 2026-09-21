// Owner: Claude
package web

import "embed"

// Files holds the UI served at the root URL. M4 extends the pattern with app.js and the uPlot files.
//
//go:embed index.html
var Files embed.FS
