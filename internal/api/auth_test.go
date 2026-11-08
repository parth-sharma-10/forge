package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// authedServer builds a server that requires the given bearer token.
func authedServer(t *testing.T, token string) (*httptest.Server, *fakeStore) {
	t.Helper()
	store := newStore()
	srv := httptest.NewServer((&server{store: store, followEvery: 10 * time.Millisecond, token: token}).routes())
	t.Cleanup(srv.Close)
	return srv, store
}

func do(t *testing.T, method, url, auth string, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body != "" {
		r, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestAuthRejectsAndAdmits proves the bearer token is a real gate on every
// route: a missing or wrong token is 401 and a submit is not accepted; the
// right token admits the request.
func TestAuthRejectsAndAdmits(t *testing.T) {
	srv, store := authedServer(t, "s3cret")

	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic s3cret", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token", "Bearer s3cret", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, "POST", srv.URL+"/jobs", tc.auth, `{"args":["/bin/true"]}`)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("POST /jobs with %q: got %d, want %d", tc.auth, resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate header")
			}
		})
	}

	// A rejected submit must not have reached the store.
	if n := len(store.jobs); n != 1 {
		t.Fatalf("store holds %d jobs, want 1 (only the authorized submit)", n)
	}
}

// TestAuthGuardsEveryRoute: reads and metrics are behind the token too, not
// just submission.
func TestAuthGuardsEveryRoute(t *testing.T) {
	srv, _ := authedServer(t, "s3cret")
	for _, path := range []string{"/jobs", "/jobs/1", "/jobs/1/logs", "/jobs/1/artifact", "/metrics"} {
		resp := do(t, "GET", srv.URL+path, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: got %d, want 401", path, resp.StatusCode)
		}
	}
	resp := do(t, "POST", srv.URL+"/jobs/1/cancel", "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /jobs/1/cancel without a token: got %d, want 401", resp.StatusCode)
	}
}

// TestNoTokenDisablesAuth: the proxy-terminates-auth mode passes requests
// through unauthenticated.
func TestNoTokenDisablesAuth(t *testing.T) {
	srv, _ := authedServer(t, "")
	resp := do(t, "POST", srv.URL+"/jobs", "", `{"args":["/bin/true"]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("with auth off, POST /jobs got %d, want 201", resp.StatusCode)
	}
}
