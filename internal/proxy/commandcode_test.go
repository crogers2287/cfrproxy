package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// TestCommandCodeSendHitsAlphaGenerate verifies the send path for a
// commandcode provider: /alpha/generate (never /provider/v1), the CLI identity
// headers, a per-request session id, and an envelope that forces stream:true.
func TestCommandCodeSendHitsAlphaGenerate(t *testing.T) {
	var gotPath, gotAuth, gotEnv, gotVersion, gotSession string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotEnv = r.Header.Get("x-cli-environment")
		gotVersion = r.Header.Get("x-command-code-version")
		gotSession = r.Header.Get("x-session-id")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		w.Write([]byte(`{"type":"text-delta","id":"t","text":"pong"}` + "\n" +
			`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":1}}` + "\n"))
	}))
	defer srv.Close()

	p := New(newDiscoveryStore(t))
	prov := store.Provider{Type: "commandcode", BaseURL: srv.URL + "/provider/v1", APIKey: "user_test"}
	req, err := wire.BuildCommandCodeRequest(&wire.Request{Model: "deepseek/deepseek-v4-flash",
		Messages: []wire.Msg{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.send(context.Background(), prov, providerPath(prov.Type), req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if gotPath != "/alpha/generate" {
		t.Errorf("path = %q, want /alpha/generate (base had /provider/v1 suffix)", gotPath)
	}
	if gotAuth != "Bearer user_test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotEnv != "production" {
		t.Errorf("x-cli-environment = %q", gotEnv)
	}
	if gotVersion != wire.CommandCodeVersion {
		t.Errorf("x-command-code-version = %q, want %q", gotVersion, wire.CommandCodeVersion)
	}
	if gotSession == "" {
		t.Errorf("x-session-id empty")
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("envelope missing forced stream: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"config"`) || !strings.Contains(gotBody, `"permissionMode"`) {
		t.Errorf("envelope missing schema-strict blocks: %s", gotBody)
	}

	// and the NDJSON response parses into a normal response (non-stream client)
	norm, err := parseOutboundResponse("commandcode", body)
	if err != nil {
		t.Fatal(err)
	}
	if norm.Content != "pong" {
		t.Errorf("content = %q", norm.Content)
	}
}

// TestCommandCodeListModelsHitsProviderV1: the catalog lives under
// /provider/v1/models even though generation is /alpha/generate.
func TestCommandCodeListModelsHitsProviderV1(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4-flash"}]}`))
	}))
	defer srv.Close()

	p := New(newDiscoveryStore(t))
	prov := store.Provider{Type: "commandcode", BaseURL: srv.URL + "/provider/v1", APIKey: "user_test"}
	models, err := p.ListModels(context.Background(), prov)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/provider/v1/models" {
		t.Errorf("path = %q, want /provider/v1/models", gotPath)
	}
	if gotAuth != "Bearer user_test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if len(models) != 1 || models[0] != "deepseek/deepseek-v4-flash" {
		t.Errorf("models = %v", models)
	}
}
