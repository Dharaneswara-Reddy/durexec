// Package webui embeds the engine's demonstration dashboard — a single
// static HTML file with inline CSS and vanilla JavaScript — and exposes it
// as a plain http.Handler.
//
// There is no build step and no JavaScript dependency, so there is nothing
// for this package to do at runtime beyond serving one file from memory.
// go:embed bakes index.html into the compiled binary, so the container image
// needs no separate static-file volume and no CDN: the dashboard is part of
// the engined binary itself.
package webui

import (
	"embed"
	"net/http"
)

//go:embed index.html
var assets embed.FS

// Handler serves the dashboard. It answers "/" and "/index.html" from the
// embedded filesystem; every other path 404s from here, which is what makes
// it safe to mount at the root of a mux alongside the engine's own /v1/*,
// /healthz and /metrics routes — see the integration note in README.md.
func Handler() http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The page is baked into the binary, so a new build means new markup at
		// the same URL with no filename to change. http.FileServer offers an
		// ETag and a Last-Modified date, which a browser will happily reuse —
		// leaving an operator on yesterday's dashboard after a deploy, with no
		// clue that a reload is needed. Revalidating on every load costs one
		// conditional request and removes the whole class of confusion.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
