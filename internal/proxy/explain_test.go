package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func explainFixture(t *testing.T) (*Proxy, *store.Store) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(up.Close)
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "b", Type: "openai", BaseURL: up.URL, DefaultModel: "bm", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "a", Type: "openai", BaseURL: up.URL, DefaultModel: "am", Priority: 10, Enabled: true, Fallback: "b/bm"})
	if err := s.SaveEndpoint(&store.Endpoint{Name: "team", APIKey: "cfr_k", Models: "a/*", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return New(s), s
}

func TestExplainShareEndpointDeniesOffListModel(t *testing.T) {
	p, _ := explainFixture(t)
	res := p.Explain(context.Background(), ExplainRequest{Model: "b/bm", Endpoint: "team"})
	if res.Status != 403 || !strings.Contains(res.Error, "allow-list: a/*") {
		t.Fatalf("want 403 naming the allow-list, got %d %q", res.Status, res.Error)
	}
	res = p.Explain(context.Background(), ExplainRequest{Model: "a/am", Endpoint: "team"})
	if res.Status != 200 || res.Resolved != "a/am" {
		t.Fatalf("want a/am allowed, got %d %q %q", res.Status, res.Resolved, res.Error)
	}
}

func TestExplainShowsProviderChainOnlyWhenEnabled(t *testing.T) {
	p, s := explainFixture(t)
	s.SetSetting("provider_fallback", "")
	res := p.Explain(context.Background(), ExplainRequest{Model: "a/am"})
	if len(res.Candidates) != 1 {
		t.Fatalf("provider chain should be off by default, candidates=%+v", res.Candidates)
	}
	if !strings.Contains(res.Text(), "provider_fallback is off") {
		t.Fatalf("explain should say why the hop is skipped:\n%s", res.Text())
	}
	s.SetSetting("provider_fallback", `{"enabled":true}`)
	res = p.Explain(context.Background(), ExplainRequest{Model: "a/am"})
	if len(res.Candidates) != 2 || res.Candidates[1].Provider != "b" || res.Candidates[1].Why != "provider fallback" {
		t.Fatalf("expected b as provider fallback, candidates=%+v", res.Candidates)
	}
}

func TestExplainReportsModelMapAndScope(t *testing.T) {
	p, s := explainFixture(t)
	s.SetSetting("model_map", `{"fast":"b/bm"}`)
	res := p.Explain(context.Background(), ExplainRequest{Model: "fast"})
	if res.Resolved != "b/bm" || !strings.Contains(res.Text(), "model_map") {
		t.Fatalf("model_map alias not explained: %s", res.Text())
	}
	res = p.Explain(context.Background(), ExplainRequest{Model: "b/bm", Scope: "a"})
	if res.Resolved != "a/bm" || !strings.Contains(res.Text(), "/p/a mount") {
		t.Fatalf("scoped mount not explained: %s", res.Text())
	}
}
