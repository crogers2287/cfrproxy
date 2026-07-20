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
