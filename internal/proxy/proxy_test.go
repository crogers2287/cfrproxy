package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"https://dialagram.me/router/v1/":                 "https://dialagram.me/router/v1",
		"dialagram.me/router/v1":                          "https://dialagram.me/router/v1",
		"https://dialagram.me/router/v1/chat/completions": "https://dialagram.me/router/v1",
		"https://api.anthropic.com/v1/messages":           "https://api.anthropic.com/v1",
		"fred:9069":                                       "http://fred:9069",
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
