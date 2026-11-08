package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/parth-sharma-10/forge/internal/queue"
)

// marker is planted in a job's args, log, error and worker name, so a test
// can tell whether a response carried job data.
const marker = "MARKER-7f3a9c"

// seededHandler is the real api.Handler, token required, over a store
// holding one finished job whose every field carries the marker.
func seededHandler(t *testing.T) (http.Handler, *fakeStore) {
	t.Helper()
	store := newStore()
	store.Submit(t.Context(), []string{"/bin/sh", "-c", "echo " + marker})
	store.set(1, queue.Status{State: "done", Worker: marker, Result: queue.Result{
		Error: marker, Artifact: []byte(marker),
		ArtifactBytes: int64(len(marker)),
		Log:           queue.Log{Tail: []byte(marker), Total: int64(len(marker))},
	}})
	return Handler(store, "s3cret"), store
}

// serve sends the request line exactly as given, with no client-side
// cleaning, the way a hostile client could.
func serve(h http.Handler, method, target, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var consoleAssets = []struct{ path, contentType string }{
	{"/ui/", "text/html"},
	{"/ui/app.js", "text/javascript"},
	{"/ui/style.css", "text/css"},
}

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// TestConsoleLoadsWithoutToken: a browser navigation carries no
// Authorization header, so the console's fixed files must load without one,
// each under the strict headers.
func TestConsoleLoadsWithoutToken(t *testing.T) {
	h, _ := seededHandler(t)
	for _, a := range consoleAssets {
		w := serve(h, "GET", a.path, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s without a token: %d, want 200", a.path, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, a.contentType) {
			t.Errorf("GET %s: Content-Type %q, want %s", a.path, ct, a.contentType)
		}
		checkConsoleHeaders(t, a.path, w.Header())
	}
}

func checkConsoleHeaders(t *testing.T, path string, h http.Header) {
	t.Helper()
	for name, want := range map[string]string{
		"Content-Security-Policy": wantCSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("GET %s: %s = %q, want %q", path, name, got, want)
		}
	}
	if h.Get("Set-Cookie") != "" {
		t.Errorf("GET %s sets a cookie; the console must not", path)
	}
}

// TestConsoleAssetsCarryNoJobData: what is served without a token is the
// same fixed bytes whatever the queue holds.
func TestConsoleAssetsCarryNoJobData(t *testing.T) {
	h, _ := seededHandler(t)
	empty := Handler(newStore(), "s3cret")
	for _, a := range consoleAssets {
		seeded, blank := serve(h, "GET", a.path, ""), serve(empty, "GET", a.path, "")
		if strings.Contains(seeded.Body.String(), marker) {
			t.Errorf("GET %s without a token returned job data", a.path)
		}
		if seeded.Body.String() != blank.Body.String() {
			t.Errorf("GET %s differs with the queue's contents; assets must be fixed files", a.path)
		}
	}
}

// TestRootRedirectsToConsole: the API's bare address opens the console.
func TestRootRedirectsToConsole(t *testing.T) {
	h, _ := seededHandler(t)
	w := serve(h, "GET", "/", "")
	if w.Code/100 != 3 || w.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET / without a token: %d Location %q, want a redirect to /ui/", w.Code, w.Header().Get("Location"))
	}
}

// TestConsoleUnknownPathIs404: nothing under /ui/ but the fixed files, no
// listing, no fallback to index.html.
func TestConsoleUnknownPathIs404(t *testing.T) {
	h, _ := seededHandler(t)
	for _, path := range []string{"/ui/nope", "/ui/index.html", "/ui/index.html/", "/ui/app.js/x", "/ui/sub/", "/ui/console/", "/ui/.git/config"} {
		w := serve(h, "GET", path, "")
		if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
			t.Errorf("GET %s: %d %q, want the plain 404", path, w.Code, w.Body.String())
		}
	}
}

// TestEverythingElseNeedsToken: the console's GET is the only door left
// open. Unknown paths, other methods on /ui/, and methods a data route does
// not serve all meet the token check first.
func TestEverythingElseNeedsToken(t *testing.T) {
	h, store := seededHandler(t)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/nope"}, {"POST", "/ui/"}, {"PUT", "/ui/app.js"},
		{"DELETE", "/ui/"}, {"POST", "/"}, {"DELETE", "/jobs/1"}, {"OPTIONS", "/jobs"},
		{"PATCH", "/jobs/1"}, {"HEAD", "/jobs"}, {"HEAD", "/metrics"},
	} {
		if w := serve(h, rt.method, rt.path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: %d, want 401", rt.method, rt.path, w.Code)
		}
	}
	if n := len(store.jobs); n != 1 {
		t.Fatalf("store holds %d jobs, want 1", n)
	}
	// /ui without the slash is redirected to the console page itself.
	if w := serve(h, "GET", "/ui", ""); w.Code/100 != 3 || w.Header().Get("Location") != "/ui/" {
		t.Errorf("GET /ui: %d Location %q, want a redirect to /ui/", w.Code, w.Header().Get("Location"))
	}
	if w := serve(h, "HEAD", "/ui/", ""); w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != wantCSP {
		t.Errorf("HEAD /ui/: %d, CSP %q", w.Code, w.Header().Get("Content-Security-Policy"))
	}
}

// TestMetricsMatchTheConsoleParser: the console reads queue counts from the
// /metrics text with this pattern (console/app.js, loadCounts). A format
// change would blank the counters silently; this makes it fail here.
func TestMetricsMatchTheConsoleParser(t *testing.T) {
	h, _ := seededHandler(t)
	body := serve(h, "GET", "/metrics", "Bearer s3cret").Body.String()
	for _, state := range []string{"queued", "running", "done"} {
		re := regexp.MustCompile(`(?m)^forge_queue_jobs\{state="` + state + `"\} (\d+)$`)
		if !re.MatchString(body) {
			t.Errorf("/metrics has no line the console can read for %s:\n%s", state, body)
		}
	}
	js, _ := consoleFS.ReadFile("console/app.js")
	if !strings.Contains(string(js), `'^forge_queue_jobs\\{state="' + state + '"\\} (\\d+)$'`) {
		t.Error("console/app.js no longer uses the pattern this test checks; update both together")
	}
}

// dataRoutes is every route that reads or changes job state.
var dataRoutes = []struct{ method, path string }{
	{"GET", "/jobs"},
	{"POST", "/jobs"},
	{"GET", "/jobs/1"},
	{"GET", "/jobs/1/logs"},
	{"GET", "/jobs/1/artifact"},
	{"POST", "/jobs/1/cancel"},
	{"GET", "/metrics"},
}

// TestDataRoutesStillNeedToken is the reverse direction: mounting the
// console must not have opened a single data route.
func TestDataRoutesStillNeedToken(t *testing.T) {
	h, store := seededHandler(t)
	for _, rt := range dataRoutes {
		for _, auth := range []string{"", "Bearer wrong", "Basic s3cret", "Bearer s3cret "} {
			w := serve(h, rt.method, rt.path, auth)
			if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), marker) {
				t.Errorf("%s %s with %q: %d, want 401 with no job data", rt.method, rt.path, auth, w.Code)
			}
		}
	}
	if n := len(store.jobs); n != 1 {
		t.Fatalf("store holds %d jobs after unauthenticated submits, want 1", n)
	}
	// And the right token still works, so the 401s above are not a broken route.
	for _, rt := range dataRoutes {
		if rt.method != "GET" {
			continue
		}
		if w := serve(h, rt.method, rt.path, "Bearer s3cret"); w.Code != http.StatusOK {
			t.Errorf("%s %s with the token: %d, want 200", rt.method, rt.path, w.Code)
		}
	}
}

// trickPaths try to reach a data route through the console's unauthenticated
// prefix: dot segments, encoded dots and slashes, backslashes, doubled
// slashes, case and trailing-slash variants.
var trickPaths = []string{
	"/ui/../jobs",
	"/ui/../jobs/1",
	"/ui/../jobs/1/logs",
	"/ui/../jobs/1/artifact",
	"/ui/../metrics",
	"/ui/./../jobs",
	"/ui//../jobs",
	"/ui/app.js/../../jobs",
	"/ui/%2e%2e/jobs",
	"/ui/%2E%2E/jobs/1",
	"/ui/..%2fjobs",
	"/ui/..%2Fjobs/1/artifact",
	"/ui/%2e%2e%2fjobs",
	"/ui%2f..%2fjobs",
	"/ui/..%5cjobs",
	"/ui/..\\jobs",
	"/ui\\..\\jobs",
	"/ui/%5c..%5cjobs",
	"/ui/%252e%252e/jobs",
	"//jobs",
	"//jobs/1",
	"/./jobs",
	"/UI/../jobs",
	"/Jobs",
	"/jobs/",
	"/jobs/1/",
	"/jobs/1/artifact/",
	"/ui/jobs",
	"/ui/jobs/1",
	"/ui/metrics",
	"/ui;/../jobs",
	"/ui/%00/../jobs",
}

// TestPathTricksDoNotBypassAuth follows every redirect the server hands out,
// without a token, and requires that no hop returns job data or a success
// from a data route.
func TestPathTricksDoNotBypassAuth(t *testing.T) {
	h, store := seededHandler(t)
	for _, path := range trickPaths {
		for _, method := range []string{"GET", "POST"} {
			target := path
			for hop := 0; ; hop++ {
				w := serveRaw(t, h, method, target)
				if strings.Contains(w.Body.String(), marker) {
					t.Errorf("%s %s (hop %d, %s): returned job data", method, path, hop, target)
				}
				loc := w.Header().Get("Location")
				if w.Code/100 != 3 || loc == "" || hop == 5 {
					if w.Code/100 == 2 && !isConsoleAsset(target) {
						t.Errorf("%s %s (hop %d, %s): %d, want no success off the console", method, path, hop, target, w.Code)
					}
					break
				}
				target = loc
			}
		}
	}
	if n := len(store.jobs); n != 1 {
		t.Fatalf("store holds %d jobs after trick submits, want 1", n)
	}
}

// serveRaw parses the request line the way the server reads one off the
// wire, so no client-side cleaning (URL.Parse resolves "..") hides a trick.
// A line the server's own parser refuses never reaches a handler at all.
func serveRaw(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(
		method + " " + target + " HTTP/1.1\r\nHost: forge\r\nContent-Length: 0\r\n\r\n")))
	if err != nil {
		// Every trick path must reach the handler; one the parser refuses
		// would be tested for nothing.
		t.Fatalf("%s %s: refused by the request parser: %v", method, target, err)
	}
	h.ServeHTTP(w, r)
	return w
}

func isConsoleAsset(target string) bool {
	for _, a := range consoleAssets {
		if target == a.path {
			return true
		}
	}
	return false
}

// TestConsoleScriptHasNoHTMLSinks is a cheap static guard, not proof: the
// script must write API values as text, so it may not use any API that
// parses a string as HTML or evaluates one as code. The browser check in the
// PR is the real proof.
func TestConsoleScriptHasNoHTMLSinks(t *testing.T) {
	js, err := consoleFS.ReadFile("console/app.js")
	if err != nil {
		t.Fatal(err)
	}
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|` +
		`\beval\s*\(|new\s+Function|setTimeout\s*\(\s*["'\x60]|setInterval\s*\(\s*["'\x60]|` +
		`createContextualFragment|DOMParser|srcdoc|javascript:|\bFunction\s*\(|\[\s*["']eval|` +
		`createElement\(\s*["']script|setAttribute\(\s*["']on|\.src\s*=|\.srcdoc`)
	if m := banned.FindAll(js, -1); m != nil {
		t.Errorf("console/app.js uses banned sinks: %q", m)
	}

	page, err := consoleFS.ReadFile("console/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).FindAll(page, -1) {
		if !regexp.MustCompile(`(?is)^<script\s[^>]*src="[^"]+"[^>]*></script>$`).Match(tag) {
			t.Errorf("console/index.html has an inline script: %q", tag)
		}
	}
	if m := regexp.MustCompile(`(?i)\son[a-z]+\s*=|\sstyle\s*=|<style`).FindAll(page, -1); m != nil {
		t.Errorf("console/index.html has inline handlers or styles the CSP would block: %q", m)
	}
	if strings.Contains(string(js)+string(page), "localStorage") || strings.Contains(string(js), "document.cookie") {
		t.Error("the console must keep the token in sessionStorage only, never localStorage or a cookie")
	}
}

// TestConsoleWorksWithAuthOff: in proxy-terminates-auth mode the console is
// served the same way and the data routes stay open, as they were.
func TestConsoleWorksWithAuthOff(t *testing.T) {
	h := Handler(newStore(), "")
	if w := serve(h, "GET", "/ui/", ""); w.Code != http.StatusOK {
		t.Errorf("GET /ui/ with auth off: %d", w.Code)
	}
	if w := serve(h, "GET", "/jobs", ""); w.Code != http.StatusOK {
		t.Errorf("GET /jobs with auth off: %d", w.Code)
	}
	if w := serve(h, "GET", "/", ""); w.Code/100 != 3 || w.Header().Get("Location") != "/ui/" {
		t.Errorf("GET / with auth off: %d Location %q", w.Code, w.Header().Get("Location"))
	}
}
