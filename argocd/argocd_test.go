package argocd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/assimilate/spec"
)

// call is one request as seen by the test server.
type call struct {
	method string
	path   string // escaped form, so path-escaping of names is observable
	query  string
	auth   string
	accept string
	ctype  string
	body   string
}

// recorder collects calls and answers per-path status overrides.
type recorder struct {
	mu    sync.Mutex
	calls []call
	fail  map[string]int    // escaped path → status code
	body  map[string]string // escaped path → response body for failures
}

func (r *recorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.calls = append(r.calls, call{
			method: req.Method,
			path:   req.URL.EscapedPath(),
			query:  req.URL.RawQuery,
			auth:   req.Header.Get("Authorization"),
			accept: req.Header.Get("Accept"),
			ctype:  req.Header.Get("Content-Type"),
			body:   string(b),
		})
		status, failed := r.fail[req.URL.EscapedPath()]
		body := r.body[req.URL.EscapedPath()]
		r.mu.Unlock()
		if failed {
			w.WriteHeader(status)
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, "{}")
	})
}

func (r *recorder) got() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.calls...)
}

func collectLog() (func(string), *[]string) {
	var lines []string
	return func(s string) { lines = append(lines, s) }, &lines
}

func TestRolloutRefreshThenSync(t *testing.T) {
	tests := []struct {
		name       string
		app        spec.ArgoApp
		serverPath string // appended to the server URL; "" = "/"
		wantPath   string // escaped path of the refresh call
		wantQuery  string
		wantBody   string // sync request body
	}{
		{
			name:      "no namespace",
			app:       spec.ArgoApp{Name: "my-app"},
			wantPath:  "/api/v1/applications/my-app",
			wantQuery: "refresh=normal",
			wantBody:  "{}",
		},
		{
			name:      "with namespace",
			app:       spec.ArgoApp{Name: "my-app", Namespace: "team-a"},
			wantPath:  "/api/v1/applications/my-app",
			wantQuery: "refresh=normal&appNamespace=team-a",
			wantBody:  `{"appNamespace":"team-a"}`,
		},
		{
			name:      "name needing path escaping",
			app:       spec.ArgoApp{Name: "team/app one"},
			wantPath:  "/api/v1/applications/team%2Fapp%20one",
			wantQuery: "refresh=normal",
			wantBody:  "{}",
		},
		{
			name:       "path-prefixed server (rootpath-hosted ArgoCD)",
			app:        spec.ArgoApp{Name: "my-app"},
			serverPath: "/argocd",
			wantPath:   "/argocd/api/v1/applications/my-app",
			wantQuery:  "refresh=normal",
			wantBody:   "{}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			srv := httptest.NewServer(rec.handler())
			defer srv.Close()
			sp := tt.serverPath
			if sp == "" {
				sp = "/" // trailing slash must not double up
			}
			tt.app.Server = srv.URL + sp

			logf, lines := collectLog()
			if err := Rollout(context.Background(), []spec.ArgoApp{tt.app}, "tok", false, logf); err != nil {
				t.Fatalf("Rollout: %v", err)
			}

			calls := rec.got()
			if len(calls) != 2 {
				t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
			}
			refresh, sync := calls[0], calls[1]
			if refresh.method != http.MethodGet || refresh.path != tt.wantPath {
				t.Errorf("refresh = %s %s, want GET %s", refresh.method, refresh.path, tt.wantPath)
			}
			if refresh.query != tt.wantQuery {
				t.Errorf("refresh query = %q, want %q", refresh.query, tt.wantQuery)
			}
			if sync.method != http.MethodPost || sync.path != tt.wantPath+"/sync" {
				t.Errorf("sync = %s %s, want POST %s/sync", sync.method, sync.path, tt.wantPath)
			}
			if sync.body != tt.wantBody {
				t.Errorf("sync body = %q, want %q", sync.body, tt.wantBody)
			}
			for i, c := range calls {
				if c.auth != "Bearer tok" {
					t.Errorf("call %d Authorization = %q, want %q", i, c.auth, "Bearer tok")
				}
				if c.accept != "application/json" {
					t.Errorf("call %d Accept = %q", i, c.accept)
				}
				if c.ctype != "application/json" {
					t.Errorf("call %d Content-Type = %q", i, c.ctype)
				}
			}
			want := []string{
				"argocd: refreshed " + tt.app.Name,
				"argocd: sync triggered " + tt.app.Name,
			}
			if len(*lines) != len(want) || (*lines)[0] != want[0] || (*lines)[1] != want[1] {
				t.Errorf("log = %q, want %q", *lines, want)
			}
		})
	}
}

func TestRolloutNon2xx(t *testing.T) {
	rec := &recorder{
		fail: map[string]int{"/api/v1/applications/my-app": http.StatusForbidden},
		body: map[string]string{"/api/v1/applications/my-app": `  {"error":"permission denied"}` + "\n"},
	}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	logf, lines := collectLog()
	err := Rollout(context.Background(), []spec.ArgoApp{{Server: srv.URL, Name: "my-app"}}, "tok", false, logf)
	if err == nil {
		t.Fatal("Rollout: want error")
	}
	for _, want := range []string{"my-app", "403", `{"error":"permission denied"}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error %q contains untrimmed body whitespace", err)
	}
	// Refresh failed → sync must not have been attempted.
	if calls := rec.got(); len(calls) != 1 {
		t.Errorf("got %d calls, want 1 (no sync after failed refresh): %+v", len(calls), calls)
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "403") {
		t.Errorf("log = %q, want one failure line with the status", *lines)
	}
}

func TestRolloutErrorBodyExcerptCapped(t *testing.T) {
	long := strings.Repeat("x", 1000)
	rec := &recorder{
		fail: map[string]int{"/api/v1/applications/app": http.StatusInternalServerError},
		body: map[string]string{"/api/v1/applications/app": long},
	}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	err := Rollout(context.Background(), []spec.ArgoApp{{Server: srv.URL, Name: "app"}}, "tok", false, func(string) {})
	if err == nil {
		t.Fatal("Rollout: want error")
	}
	if strings.Contains(err.Error(), long) {
		t.Error("error contains the full 1000-byte body")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", maxExcerpt)) {
		t.Errorf("error %q missing the %d-byte excerpt", err, maxExcerpt)
	}
}

func TestRolloutSyncFailureLogged(t *testing.T) {
	rec := &recorder{
		fail: map[string]int{"/api/v1/applications/app/sync": http.StatusBadGateway},
		body: map[string]string{"/api/v1/applications/app/sync": "upstream sad"},
	}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	logf, lines := collectLog()
	err := Rollout(context.Background(), []spec.ArgoApp{{Server: srv.URL, Name: "app"}}, "tok", false, logf)
	if err == nil || !strings.Contains(err.Error(), "sync app") || !strings.Contains(err.Error(), "upstream sad") {
		t.Fatalf("error = %v, want sync failure with body excerpt", err)
	}
	if len(*lines) != 2 || (*lines)[0] != "argocd: refreshed app" || !strings.Contains((*lines)[1], "502") {
		t.Errorf("log = %q, want refreshed line then sync failure", *lines)
	}
}

func TestRolloutFirstFailsSecondStillRuns(t *testing.T) {
	rec := &recorder{
		fail: map[string]int{"/api/v1/applications/bad": http.StatusInternalServerError},
		body: map[string]string{"/api/v1/applications/bad": "boom"},
	}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	apps := []spec.ArgoApp{
		{Server: srv.URL, Name: "bad"},
		{Server: srv.URL, Name: "good"},
	}
	logf, lines := collectLog()
	err := Rollout(context.Background(), apps, "tok", false, logf)
	if err == nil {
		t.Fatal("Rollout: want joined error")
	}
	if !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q missing the failing app", err)
	}
	if strings.Contains(err.Error(), "good") {
		t.Errorf("error %q mentions the succeeding app", err)
	}
	var paths []string
	for _, c := range rec.got() {
		paths = append(paths, c.path)
	}
	want := []string{
		"/api/v1/applications/bad",
		"/api/v1/applications/good",
		"/api/v1/applications/good/sync",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %q, want %q", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, paths[i], want[i])
		}
	}
	if len(*lines) != 3 {
		t.Errorf("log = %q, want failure + two success lines", *lines)
	}
}

func TestRolloutBothFailJoined(t *testing.T) {
	rec := &recorder{
		fail: map[string]int{
			"/api/v1/applications/a": http.StatusInternalServerError,
			"/api/v1/applications/b": http.StatusNotFound,
		},
	}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	err := Rollout(context.Background(), []spec.ArgoApp{
		{Server: srv.URL, Name: "a"},
		{Server: srv.URL, Name: "b"},
	}, "tok", false, func(string) {})
	if err == nil {
		t.Fatal("Rollout: want error")
	}
	for _, want := range []string{"refresh a", "500", "refresh b", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestRolloutContextCancelled(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Rollout(ctx, []spec.ArgoApp{{Server: srv.URL, Name: "app"}}, "tok", false, func(string) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls := rec.got(); len(calls) != 0 {
		t.Errorf("server saw %d calls, want 0", len(calls))
	}
}

// deadlineTransport records each request context's deadline and replies 200
// without any network I/O.
type deadlineTransport struct {
	deadlines []time.Time // zero Time = no deadline
}

func (d *deadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	dl, _ := req.Context().Deadline()
	d.deadlines = append(d.deadlines, dl)
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("{}")),
	}, nil
}

// Refresh blocks server-side until manifest generation completes, routinely
// longer than 30s; a client-wide Timeout would kill a rollout half-done.
// Deadlines must instead be generous, per-request, and derived from the
// caller's context.
func TestPerRequestDeadlines(t *testing.T) {
	if refreshTimeout <= 30*time.Second {
		t.Errorf("refreshTimeout = %v, must exceed the 30s that spuriously failed refreshes", refreshTimeout)
	}
	for _, insecure := range []bool{false, true} {
		if c := newClient(insecure); c.Timeout != 0 {
			t.Errorf("newClient(%v).Timeout = %v, want 0 (deadlines are per-request)", insecure, c.Timeout)
		}
	}

	rt := &deadlineTransport{}
	client := &http.Client{Transport: rt}
	app := spec.ArgoApp{Server: "http://argocd.internal", Name: "app"}

	before := time.Now()
	if err := refresh(context.Background(), client, app, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := syncApp(context.Background(), client, app, "tok"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	if len(rt.deadlines) != 2 {
		t.Fatalf("saw %d requests, want 2", len(rt.deadlines))
	}
	for i, want := range []time.Duration{refreshTimeout, syncTimeout} {
		dl := rt.deadlines[i]
		if dl.IsZero() {
			t.Fatalf("request %d has no deadline, want ~%v", i, want)
		}
		if dl.Before(before.Add(want)) || dl.After(after.Add(want)) {
			t.Errorf("request %d deadline %v outside [%v, %v]", i, dl, before.Add(want), after.Add(want))
		}
	}

	// A tighter caller deadline must win (per-request timeouts derive from
	// the caller ctx, they do not replace it).
	rt.deadlines = nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := refresh(ctx, client, app, "tok"); err != nil {
		t.Fatal(err)
	}
	if dl := rt.deadlines[0]; dl.After(time.Now().Add(time.Second)) {
		t.Errorf("caller's 1s deadline not honoured: request deadline %v", dl)
	}
}

func TestRolloutInsecureTLS(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewTLSServer(rec.handler())
	defer srv.Close()
	apps := []spec.ArgoApp{{Server: srv.URL, Name: "app"}}

	if err := Rollout(context.Background(), apps, "tok", false, func(string) {}); err == nil {
		t.Error("insecure=false against self-signed TLS: want certificate error")
	}
	if err := Rollout(context.Background(), apps, "tok", true, func(string) {}); err != nil {
		t.Errorf("insecure=true: %v", err)
	}
	if calls := rec.got(); len(calls) != 2 {
		t.Errorf("server saw %d calls, want 2 from the insecure run", len(calls))
	}
}
