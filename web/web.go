// Package web holds the static frontend assets (HTML, CSS, JS) embedded
// into the velocity binary via //go:embed.
package web

import "embed"

// FS holds the embedded dashboard assets. The server package parses *.html
// (plus partials/*.html) as Go templates so pages can share the nav fragment;
// .css and .js are served as plain static files.
//
//go:embed *.html *.css *.js partials/*.html
var FS embed.FS
