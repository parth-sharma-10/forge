package api

import (
	"embed"
	"net/http"
)

// The console is three fixed files compiled into the binary. They are served
// without the bearer token, because a browser's page load cannot carry an
// Authorization header; that is safe only because they hold no job data and
// are never templated. Everything the page shows it fetches from the data
// routes, with the token, like any other client.
//
//go:embed console
var consoleFS embed.FS

// consoleFiles is the whole of what /ui/ serves. Anything else under the
// prefix is a 404: no listing, no index fallback.
var consoleFiles = map[string]struct{ file, contentType string }{
	"/ui/":          {"console/index.html", "text/html; charset=utf-8"},
	"/ui/app.js":    {"console/app.js", "text/javascript; charset=utf-8"},
	"/ui/style.css": {"console/style.css", "text/css; charset=utf-8"},
}

// consoleCSP lets the page load only its own script and style and talk only
// to its own origin. With no inline script allowed, an injection that slipped
// past the text-only rendering still could not run.
const consoleCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func serveConsole(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", consoleCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	f, ok := consoleFiles[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := consoleFS.ReadFile(f.file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", f.contentType)
	w.Write(b)
}
