package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"https://router.example.com/api/v1/":                "https://router.example.com/api/v1",
		"router.example.com/api/v1":                        "https://router.example.com/api/v1",
		"https://router.example.com/api/v1/chat/completions":"https://router.example.com/api/v1",
		"https://api.anthropic.com/v1/messages":           "https://api.anthropic.com/v1",
		"myhost:9069":                                     "http://myhost:9069",
		"localhost:11434":                                 "http://localhost:11434",
		"192.168.1.5:8080/api/chat":                       "http://192.168.1.5:8080",
		"  https://openrouter.ai  ":                       "https://openrouter.ai",
	}
	for in, want := range cases {
		if got := NormalizeBase(in); got != want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func newDiscoveryStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfrproxy-proxy-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mock router that serves chat only under /api/v1 (openrouter-style)
func TestDiscoverBaseFindsApiV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/chat/completions" {
			w.WriteHeader(401) // exists but wants auth — must still count
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL})
	if base != srv.URL+"/api/v1" {
		t.Errorf("want %s/api/v1, got %s (note %q)", srv.URL, base, note)
	}
	if !strings.Contains(note, "resolved") {
		t.Errorf("note should mention resolution: %q", note)
	}
}

// base already correct → kept, verified
func TestDiscoverBaseKeepsWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(200)
			w.Write([]byte(`{"choices":[]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL})
	if base != srv.URL {
		t.Errorf("base changed unexpectedly: %s", base)
	}
	if !strings.Contains(note, "verified") {
		t.Errorf("note should say verified: %q", note)
	}
}

// nothing responds → keep normalized input, warn
func TestDiscoverBaseNothingResponds(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL + "/nope"})
	if base != srv.URL+"/nope" {
		t.Errorf("base should stay as entered: %s", base)
	}
	if !strings.Contains(note, "warning") {
		t.Errorf("expected warning note: %q", note)
	}
}

// pasted full endpoint URL → stripped, then verified
func TestDiscoverBaseStripsPastedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/router/v1/chat/completions" {
			w.WriteHeader(400) // exists; bad probe body is fine
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, _ := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL + "/router/v1/chat/completions"})
	if base != srv.URL+"/router/v1" {
		t.Errorf("want stripped base, got %s", base)
	}
}

// primary 503s → one retry → failover provider answers; trace notes failover
func TestFailover(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" { // ignore model-list scans
			w.WriteHeader(404)
			return
		}
		primaryHits++
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"upstream timeout"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"backup-model","choices":[{"message":{"role":"assistant","content":"saved"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "backup-model", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/backup-model"}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"primary/pm","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200 via failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "saved") {
		t.Fatalf("response not from backup: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "[cfrproxy] primary unavailable — failed over to backup/backup-model") {
		t.Fatalf("failover alert missing from visible content: %s", rec.Body.String())
	}
	if primaryHits != 2 {
		t.Errorf("want 2 attempts on primary (1 retry), got %d", primaryHits)
	}
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || traces[0].Provider != "backup" || !strings.Contains(traces[0].Err, "failover from primary") {
		t.Errorf("trace should record failover to backup: %+v", traces)
	}
}

// non-transient errors (auth) must NOT fail over
func TestNoFailoverOn401(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer primary.Close()
	backupHits := 0
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			backupHits++
		}
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "b", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/b"})
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"primary/pm","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 401 {
		t.Errorf("401 should pass through, got %d", rec.Code)
	}
	if backupHits != 0 {
		t.Errorf("backup should not be hit on auth error, got %d hits", backupHits)
	}
}

// scoped /p/{provider}/ mount lists only that provider's models (bare) and
// forces routing to it regardless of the model prefix sent.
func TestScopedProviderMount(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"alpha"},{"id":"beta"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"alpha","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer backend.Close()
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "scoped", Type: "openai", BaseURL: backend.URL, DefaultModel: "alpha", Priority: 10, Enabled: true})
	// a second provider that must NOT receive scoped traffic
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("other provider should not be hit") }))
	defer other.Close()
	s.SaveProvider(&store.Provider{Name: "other", Type: "openai", BaseURL: other.URL, DefaultModel: "z", Priority: 5, Enabled: true})

	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)

	// scoped model list = bare ids from that provider only
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/p/scoped/v1/models", nil))
	if !strings.Contains(rec.Body.String(), `"alpha"`) || strings.Contains(rec.Body.String(), "scoped/alpha") {
		t.Errorf("scoped models should be bare ids: %s", rec.Body.String())
	}

	// scoped chat with a bare model → routed to 'scoped' even though 'other'
	// has higher priority
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/p/scoped/v1/chat/completions",
		strings.NewReader(`{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("scoped chat failed: %d %s", rec.Code, rec.Body.String())
	}
}

// auto router: classifier answer picks the bucket route; failures use default
func TestAutoRoute(t *testing.T) {
	classifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"code"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer classifier.Close()
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "cls", Type: "openai", BaseURL: classifier.URL, DefaultModel: "tiny", Priority: 10, Enabled: true})
	s.SetSetting("auto_router", `{"enabled":true,"classifier":"cls/tiny","routes":{"code":"cls/coder-model","default":"cls/general"}}`)
	p := New(s)
	req := &wire.Request{Messages: []wire.Msg{{Role: "user", Content: "write me a go function"}}}
	target, bucket := p.AutoRoute(context.Background(), req)
	if bucket != "code" || target != "cls/coder-model" {
		t.Errorf("want code route, got bucket=%s target=%s", bucket, target)
	}
	// disabled -> no routing
	s.SetSetting("auto_router", `{"enabled":false}`)
	if tgt, _ := p.AutoRoute(context.Background(), req); tgt != "" {
		t.Errorf("disabled router should return empty, got %s", tgt)
	}
}

func TestEndsWithVersion(t *testing.T) {
	cases := map[string]bool{
		"https://gateway.example.com/api/v4": true,
		"https://x/router/v1":                 true,
		"https://api.anthropic.com/v1beta":    true,
		"http://myhost:9069":                  false,
		"https://openrouter.ai/api":           false,
		"https://openrouter.ai/api/v1":        true,
	}
	for in, want := range cases {
		if got := endsWithVersion(in); got != want {
			t.Errorf("endsWithVersion(%q)=%v want %v", in, got, want)
		}
	}
}
