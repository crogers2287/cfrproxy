package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

// upstream serves an OpenAI-shaped catalog and records the Authorization
// header it was called with, so tests can prove which key was used.
func modelsUpstream(t *testing.T, models ...string) (*httptest.Server, *string) {
	t.Helper()
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		sawAuth = r.Header.Get("Authorization")
		data := make([]map[string]string, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &sawAuth
}

func newScanAPI(t *testing.T) *API {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &API{Store: s, Proxy: proxy.New(s)}
}

func scan(t *testing.T, a *API, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/api/providers/scan-models", strings.NewReader(body))
	w := httptest.NewRecorder()
	a.hProviderScanModels(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out
}

func modelIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["models"].([]any)
	ids := make([]string, 0, len(raw))
	for _, m := range raw {
		ids = append(ids, m.(string))
	}
	return ids
}

// The bug: "Scan models" required a saved provider id, so the picker stayed
// empty for the entire Add-provider flow.
func TestScanModelsWorksBeforeProviderIsSaved(t *testing.T) {
	a := newScanAPI(t)
	srv, sawAuth := modelsUpstream(t, "qwen3-max", "qwen3-coder")

	out := scan(t, a, `{"id":0,"type":"openai","base_url":"`+srv.URL+`","api_key":"sk-typed-in-form"}`)

	if err, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", err)
	}
	got := modelIDs(t, out)
	if len(got) != 2 || got[0] != "qwen3-max" {
		t.Fatalf("models = %v, want [qwen3-max qwen3-coder]", got)
	}
	if *sawAuth != "Bearer sk-typed-in-form" {
		t.Fatalf("upstream saw auth %q, want the key typed into the form", *sawAuth)
	}
}

// Editing a saved provider never round-trips the API key through the form, so
// a blank key must fall back to the stored one instead of scanning unauthed.
func TestScanModelsFallsBackToStoredKey(t *testing.T) {
	a := newScanAPI(t)
	srv, sawAuth := modelsUpstream(t, "glm-5.2")

	p := store.Provider{Name: "qwen", Type: "openai", BaseURL: srv.URL, APIKey: "sk-stored", Enabled: true}
	if err := a.Store.SaveProvider(&p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	out := scan(t, a, `{"id":`+strconv.FormatInt(p.ID, 10)+`,"type":"openai","base_url":"`+srv.URL+`","api_key":""}`)

	if err, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := modelIDs(t, out); len(got) != 1 || got[0] != "glm-5.2" {
		t.Fatalf("models = %v, want [glm-5.2]", got)
	}
	if *sawAuth != "Bearer sk-stored" {
		t.Fatalf("upstream saw auth %q, want the stored key", *sawAuth)
	}
}

func TestScanModelsRequiresBaseURL(t *testing.T) {
	a := newScanAPI(t)
	out := scan(t, a, `{"id":0,"type":"openai","base_url":"  ","api_key":""}`)
	if out["error"] == nil {
		t.Fatalf("want an error telling the user to enter a base URL, got %v", out)
	}
	if got := modelIDs(t, out); len(got) != 0 {
		t.Fatalf("models = %v, want empty", got)
	}
}

// A dead base URL must surface the failure rather than a silent empty picker.
func TestScanModelsReportsUpstreamFailure(t *testing.T) {
	a := newScanAPI(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 401)
	}))
	t.Cleanup(srv.Close)

	out := scan(t, a, `{"id":0,"type":"openai","base_url":"`+srv.URL+`","api_key":"bad"}`)
	if out["error"] == nil {
		t.Fatalf("want an error, got %v", out)
	}
}

// Route wiring: POST /admin/api/providers/scan-models must reach the handler
// and not collide with POST /admin/api/providers (create).
func TestScanModelsRouteIsWired(t *testing.T) {
	a := newScanAPI(t)
	if err := a.Store.SetSetting("admin_user", "admin"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := a.SetPassword("pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	srv, _ := modelsUpstream(t, "qwen3-max")

	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest("POST", "/admin/api/providers/scan-models",
		strings.NewReader(`{"id":0,"type":"openai","base_url":"`+srv.URL+`","api_key":"k"}`))
	req.Header.Set("Content-Type", "application/json") // the CSRF guard refuses non-JSON writes
	req.SetBasicAuth("admin", "pw")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if got := modelIDs(t, out); len(got) != 1 || got[0] != "qwen3-max" {
		t.Fatalf("models = %v, want [qwen3-max]", got)
	}
	// the create route must still exist and be distinct
	if n := len(a.Store.Providers()); n != 0 {
		t.Fatalf("scan created %d providers; it must be read-only", n)
	}
}

// A shared upstream (CLIProxyAPI, OpenRouter) answers /v1/models with its whole
// catalog; models_filter is what narrows it to the provider actually being set
// up. Scanning unfiltered offered ~139 models the router would then refuse.
func TestScanModelsAppliesProviderFilter(t *testing.T) {
	a := newScanAPI(t)
	srv, _ := modelsUpstream(t, "claude-opus-4-6", "claude-sonnet-4-6", "gpt-5.6", "gemini-3-pro", "kimi-k2")

	p := store.Provider{Name: "oauth-claude", Type: "openai", BaseURL: srv.URL,
		APIKey: "sk-stored", ModelsFilter: "claude-*", Enabled: true}
	if err := a.Store.SaveProvider(&p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}

	out := scan(t, a, `{"id":`+strconv.FormatInt(p.ID, 10)+`,"type":"openai","base_url":"`+srv.URL+`"}`)

	got := modelIDs(t, out)
	if len(got) != 2 || got[0] != "claude-opus-4-6" || got[1] != "claude-sonnet-4-6" {
		t.Fatalf("models = %v, want only the claude-* pair", got)
	}
	if out["scanned"] != float64(5) {
		t.Fatalf("scanned = %v, want 5 so the UI can say \"2 of 5\"", out["scanned"])
	}
	if out["filter"] != "claude-*" {
		t.Fatalf("filter = %v, want claude-*", out["filter"])
	}
}

// Typing a filter into the form must re-scope the scan before the provider is
// saved — that is the whole point of being able to tune it and rescan.
func TestScanModelsHonorsFormFilterOverride(t *testing.T) {
	a := newScanAPI(t)
	srv, _ := modelsUpstream(t, "claude-opus-4-6", "gpt-5.6", "gpt-5.6-mini")

	p := store.Provider{Name: "oauth", Type: "openai", BaseURL: srv.URL,
		APIKey: "sk", ModelsFilter: "claude-*", Enabled: true}
	if err := a.Store.SaveProvider(&p); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	id := strconv.FormatInt(p.ID, 10)

	// form narrows further than the stored filter
	out := scan(t, a, `{"id":`+id+`,"base_url":"`+srv.URL+`","models_filter":"gpt-*,!*-mini"}`)
	if got := modelIDs(t, out); len(got) != 1 || got[0] != "gpt-5.6" {
		t.Fatalf("models = %v, want [gpt-5.6] from the form's filter", got)
	}

	// explicitly-empty means "no filter", not "keep the stored one"
	out = scan(t, a, `{"id":`+id+`,"base_url":"`+srv.URL+`","models_filter":""}`)
	if got := modelIDs(t, out); len(got) != 3 {
		t.Fatalf("models = %v, want all 3 once the filter is cleared", got)
	}

	// omitting the field entirely keeps the stored filter
	out = scan(t, a, `{"id":`+id+`,"base_url":"`+srv.URL+`"}`)
	if got := modelIDs(t, out); len(got) != 1 || got[0] != "claude-opus-4-6" {
		t.Fatalf("models = %v, want the stored claude-* filter to survive omission", got)
	}
}

// No filter configured must not mean "hide everything".
func TestScanModelsWithoutFilterReturnsFullCatalog(t *testing.T) {
	a := newScanAPI(t)
	srv, _ := modelsUpstream(t, "a", "b", "c")
	out := scan(t, a, `{"id":0,"type":"openai","base_url":"`+srv.URL+`","api_key":"k"}`)
	if got := modelIDs(t, out); len(got) != 3 {
		t.Fatalf("models = %v, want all 3", got)
	}
	if out["scanned"] != float64(3) {
		t.Fatalf("scanned = %v, want 3", out["scanned"])
	}
}
