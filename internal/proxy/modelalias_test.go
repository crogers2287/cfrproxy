package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// mapAliasIDs decides which operator-declared names a client may enumerate.
func TestMapAliasIDsAdvertisesExactKeysOnly(t *testing.T) {
	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai",
		BaseURL: "http://127.0.0.1:1", DefaultModel: "ornith", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "parked", Type: "openai",
		BaseURL: "http://127.0.0.1:2", DefaultModel: "x", Priority: 20}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelMap(map[string]string{
		"ornith-aggregate": "fred/ornith",    // exact, live provider — advertise
		"claude-sonnet*":   "fred/agents-a1", // interception pattern — not an id
		"shelved":          "parked/x",       // target provider is disabled
		"ghost":            "nosuchprov/x",   // target provider does not exist
		"everything":       "auto",           // another virtual name, no prefix
	}); err != nil {
		t.Fatal(err)
	}
	got := New(s).mapAliasIDs()
	if strings.Join(got, ",") != "everything,ornith-aggregate" {
		t.Errorf("mapAliasIDs = %v, want [everything ornith-aggregate]", got)
	}
}

func TestMapAliasIDsEmptyMap(t *testing.T) {
	if got := New(newDiscoveryStore(t)).mapAliasIDs(); got != nil {
		t.Errorf("mapAliasIDs on an unset map = %v, want nil", got)
	}
}

// modelsListing returns the ids GET /v1/models advertises, and each id's
// advertised context window.
func modelsListing(t *testing.T, p *Proxy, path string) (ids []string, ctx map[string]int) {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("%s → %d: %s", path, rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	ctx = map[string]int{}
	for _, m := range out.Data {
		ids = append(ids, m.ID)
		ctx[m.ID] = m.ContextLength
	}
	return ids, ctx
}

// The listing gap this fixes: an alias that routes correctly on a direct POST
// but never appears where a client enumerates is unusable from a model picker.
// It must also carry the window of the model it resolves to — an alias that
// advertises nothing makes the harness guess, which is how REQ-086's context
// overflow happened.
func TestModelsListingIncludesMapAliasWithItsContextWindow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[
			{"id":"ornith","meta":{"llamaswap":{"context":131072}}},
			{"id":"ornith-kvx-w6800","meta":{"llamaswap":{"context":131072}}}]}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "ornith", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	before, _ := modelsListing(t, p, "/v1/models")
	if err := s.SetModelMap(map[string]string{"ornith-aggregate": "fred/ornith"}); err != nil {
		t.Fatal(err)
	}
	after, ctx := modelsListing(t, p, "/v1/models")

	if ctx["ornith-aggregate"] != 131072 {
		t.Errorf("ornith-aggregate advertised context %d, want 131072 (same as fred/ornith)",
			ctx["ornith-aggregate"])
	}
	if ctx["fred/ornith"] != 131072 {
		t.Errorf("fred/ornith advertised context %d, want 131072", ctx["fred/ornith"])
	}
	// Purely additive: the set grows by exactly the alias, and every id that
	// was advertised before is still advertised, unchanged.
	if len(after) != len(before)+1 {
		t.Errorf("listing went from %d to %d ids, want exactly one more", len(before), len(after))
	}
	still := map[string]bool{}
	for _, id := range after {
		still[id] = true
	}
	for _, id := range before {
		if !still[id] {
			t.Errorf("id %q disappeared from the listing", id)
		}
	}
}

// An alias is a router-level name, not a provider's model: the per-provider
// mount must keep listing only what that provider serves.
func TestScopedListingOmitsMapAlias(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"ornith"}]}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "ornith", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelMap(map[string]string{"ornith-aggregate": "fred/ornith"}); err != nil {
		t.Fatal(err)
	}
	ids, _ := modelsListing(t, New(s), "/p/fred/v1/models")
	for _, id := range ids {
		if id == "ornith-aggregate" {
			t.Fatalf("provider-scoped listing advertised the router-level alias: %v", ids)
		}
	}
}

// The alias must resolve to the pooled model, not merely be listed.
func TestMapAliasResolvesToItsTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"ornith"}]}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "somethingelse", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModelMap(map[string]string{"ornith-aggregate": "fred/ornith"}); err != nil {
		t.Fatal(err)
	}
	prov, model, err := New(s).ResolveModel(context.Background(), "ornith-aggregate")
	if err != nil {
		t.Fatal(err)
	}
	if prov.Name != "fred" || model != "ornith" {
		t.Errorf("ornith-aggregate resolved to %s/%s, want fred/ornith", prov.Name, model)
	}
}
