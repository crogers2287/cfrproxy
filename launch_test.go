package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/proxy"
)

func TestCodexProxyProviderConfigIsValidTOML(t *testing.T) {
	addr := "http://127.0.0.1:8420"
	got := fmt.Sprintf(`model_providers.cfrproxy.base_url=%q`, addr+"/v1")
	if got != `model_providers.cfrproxy.base_url="http://127.0.0.1:8420/v1"` {
		t.Fatalf("bad Codex provider override: %s", got)
	}
}

func TestFuzzyModel(t *testing.T) {
	models := []string{"qwen-3.8-max-preview-thinking", "glm-5.2", "agents-a1", "agents-a1-q8"}
	cases := []struct {
		want    string
		expect  string
		matched bool
	}{
		{"glm-5.2", "glm-5.2", true},                       // exact
		{"GLM-5.2", "glm-5.2", true},                       // case-insensitive
		{"preview", "qwen-3.8-max-preview-thinking", true}, // unique substring
		{"Qwen3.8", "qwen-3.8-max-preview-thinking", true}, // punctuation-blind
		{"agents-a1", "agents-a1", true},                   // exact beats prefix ambiguity
		{"agents", "", false},                              // ambiguous substring
		{"nope", "", false},                                // no match
	}
	for _, c := range cases {
		got, ok := proxy.FuzzyModel(models, c.want)
		if ok != c.matched || got != c.expect {
			t.Errorf("proxy.FuzzyModel(%q) = %q,%v want %q,%v", c.want, got, ok, c.expect, c.matched)
		}
	}
}

// `cfrproxy opencode --model fred/deepseek-v4-flash` used to launch opencode on
// its own default model. opencode namespaces models as <its-provider>/<model>
// and its provider ids are its own, so a bare "fred/..." finds no provider and
// silently falls back. The launcher has to prefix with whichever opencode
// provider actually points at this proxy.
func TestOpencodeProviderForMatchesByHostPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"provider":{
	  "fred-local":{"options":{"baseURL":"http://fred:9069/v1"}},
	  "myproxy":{"options":{"baseURL":"http://127.0.0.1:8420/v1"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := opencodeProviderFor("http://127.0.0.1:8420"); got != "myproxy" {
		t.Errorf("want the provider pointing at the proxy, got %q", got)
	}
	// scheme and trailing path differ between launcher addr and config baseURL;
	// host:port is the stable part to match on
	if got := opencodeProviderFor("http://127.0.0.1:8420/"); got != "myproxy" {
		t.Errorf("trailing slash must not break the match, got %q", got)
	}
	// an unrelated address must NOT match, or we would prefix with a provider
	// that points somewhere else entirely
	if got := opencodeProviderFor("http://127.0.0.1:9999"); got != "" {
		t.Errorf("unrelated addr matched %q", got)
	}
}

func TestOpencodeProviderForMissingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := opencodeProviderFor("http://127.0.0.1:8420"); got != "" {
		t.Errorf("no config must yield \"\" so the caller can warn, got %q", got)
	}
}
