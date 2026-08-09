package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// TestInjectProviderHeadersOverridesDefaultAuth: injected headers must replace
// the default Authorization, and literal values are forwarded verbatim.
func TestInjectProviderHeadersOverridesDefaultAuth(t *testing.T) {
	p := New(newDiscoveryStore(t))
	req, _ := http.NewRequest("POST", "http://upstream/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-default")
	req.Header.Set("User-Agent", "Go-http-client/1.1")

	prov := store.Provider{Headers: `{"Authorization":"Bearer sk-from-file","User-Agent":"command-code/0.1.0"}`}
	p.injectProviderHeaders(req, prov)

	if got := req.Header.Get("Authorization"); got != "Bearer sk-from-file" {
		t.Errorf("Authorization = %q, want injected value", got)
	}
	if got := req.Header.Get("User-Agent"); got != "command-code/0.1.0" {
		t.Errorf("User-Agent = %q, want injected value", got)
	}
}

// TestInjectProviderHeadersFileReadFresh: an "@file:" value must be read on
// every request, so a rotated CLI token is picked up without a restart.
func TestInjectProviderHeadersFileReadFresh(t *testing.T) {
	p := New(newDiscoveryStore(t))
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("user_first_token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("POST", "http://upstream/v1", nil)
	prov := store.Provider{Headers: `{"Authorization":"@file:` + token + `"}`}
	p.injectProviderHeaders(req, prov)
	if got := req.Header.Get("Authorization"); got != "user_first_token" {
		t.Fatalf("first read = %q, want user_first_token (trailing newline trimmed)", got)
	}

	// Simulate CLI token rotation.
	if err := os.WriteFile(token, []byte("user_second_token"), 0o600); err != nil {
		t.Fatal(err)
	}
	req2, _ := http.NewRequest("POST", "http://upstream/v1", nil)
	p.injectProviderHeaders(req2, prov)
	if got := req2.Header.Get("Authorization"); got != "user_second_token" {
		t.Errorf("after rotation = %q, want user_second_token", got)
	}
}

// TestInjectProviderHeadersSkipsBrokenEntries: malformed config and unreadable
// @file paths must be ignored rather than panicking or failing the request.
func TestInjectProviderHeadersSkipsBrokenEntries(t *testing.T) {
	p := New(newDiscoveryStore(t))

	req, _ := http.NewRequest("POST", "http://upstream/v1", nil)
	req.Header.Set("Authorization", "Bearer sk-default")
	prov := store.Provider{Headers: `{"Authorization":"@file:/nonexistent/definitely-missing","User-Agent":"kept"}`}
	p.injectProviderHeaders(req, prov)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-default" {
		t.Errorf("missing file should keep default auth, got %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != "kept" {
		t.Errorf("literal headers still apply, got %q", got)
	}

	req2, _ := http.NewRequest("POST", "http://upstream/v1", nil)
	req2.Header.Set("Authorization", "Bearer sk-default")
	p.injectProviderHeaders(req2, store.Provider{Headers: `{not json`})
	if got := req2.Header.Get("Authorization"); got != "Bearer sk-default" {
		t.Errorf("malformed headers should be ignored, got %q", got)
	}

	// content-type can never be overridden (breaks the JSON body framing).
	req3, _ := http.NewRequest("POST", "http://upstream/v1", nil)
	p.injectProviderHeaders(req3, store.Provider{Headers: `{"Content-Type":"text/plain"}`})
	if got := req3.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type injection must be ignored, got %q", got)
	}
}

// TestSendSendsInjectedHeaders: a proxied completion request to a provider
// with Headers configured must arrive carrying the injected Authorization and
// User-Agent, not the provider's API key.
func TestSendSendsInjectedHeaders(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := New(newDiscoveryStore(t))
	prov := store.Provider{
		Type:    "openai",
		BaseURL: srv.URL + "/v1",
		APIKey:  "sk-should-not-leak",
		Headers: `{"Authorization":"@file:` + tokenFile(t) + `","User-Agent":"command-code/0.1.0"}`,
	}
	resp, err := p.send(context.Background(), prov, "/v1/chat/completions", []byte(`{"model":"x","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotAuth != "user_cli_token" {
		t.Errorf("upstream Authorization = %q, want CLI token", gotAuth)
	}
	if strings.Contains(gotAuth, "sk-should-not-leak") {
		t.Errorf("provider API key leaked into Authorization: %q", gotAuth)
	}
	if gotUA != "command-code/0.1.0" {
		t.Errorf("upstream User-Agent = %q, want injected value", gotUA)
	}
}

// TestListModelsSendsInjectedHeaders: the model-scan GET must carry the same
// injected fingerprint so catalog discovery authenticates like the CLI too.
func TestListModelsSendsInjectedHeaders(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4-flash"}]}`))
	}))
	defer srv.Close()

	p := New(newDiscoveryStore(t))
	prov := store.Provider{
		Type:    "openai",
		BaseURL: srv.URL + "/v1",
		Headers: `{"Authorization":"@file:` + tokenFile(t) + `","User-Agent":"command-code/0.1.0"}`,
	}
	models, err := p.ListModels(context.Background(), prov)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("unexpected models: %v", models)
	}
	if gotAuth != "user_cli_token" {
		t.Errorf("model scan Authorization = %q, want CLI token", gotAuth)
	}
	if gotUA != "command-code/0.1.0" {
		t.Errorf("model scan User-Agent = %q, want injected value", gotUA)
	}
}

// TestProviderWithNoHeadersIsUntouched: no Headers configured → the default
// API-key auth is sent unchanged.
func TestProviderWithNoHeadersIsUntouched(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := New(newDiscoveryStore(t))
	prov := store.Provider{Type: "openai", BaseURL: srv.URL + "/v1", APIKey: "sk-default"}
	resp, err := p.send(context.Background(), prov, "/v1/chat/completions", []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer sk-default" {
		t.Errorf("upstream Authorization = %q, want default Bearer key", gotAuth)
	}
}

func tokenFile(t *testing.T) string {
	t.Helper()
	token := filepath.Join(t.TempDir(), "cli-token")
	if err := os.WriteFile(token, []byte("user_cli_token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return token
}
