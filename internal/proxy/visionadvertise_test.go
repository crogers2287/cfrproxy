package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestVisionRulesExtendExcludeQualified(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)
	// "+" keeps the defaults; "!" wins; a "/" pattern is provider-scoped
	s.SetSetting("vision_models", "+fred/*flash-next*, fred/deepseek-v4-flash, !fred/*-warmer")
	cases := map[string]bool{
		"fred/qwen38-flash-next-kvx":    true,
		"fred/qwen38-flash-next-warmer": false,
		"fred/deepseek-v4-flash":        true,
		"ccbudget/deepseek-v4-flash":    false, // same bare name, other provider
		"codex/gpt-5.6-terra":           true,  // default glob survives "+"
		"fred/ornith":                   false,
	}
	for id, want := range cases {
		if got := p.visionCapable(id); got != want {
			t.Errorf("visionCapable(%q) = %v, want %v", id, got, want)
		}
	}
	pats, custom, _ := p.VisionModelPatterns()
	if !custom || len(pats) != len(DefaultVisionModels)+3 || pats[len(pats)-1] != "!fred/*-warmer" {
		t.Errorf("patterns: custom=%v n=%d last=%q", custom, len(pats), pats[len(pats)-1])
	}
	// plain list still REPLACES the defaults
	s.SetSetting("vision_models", "only-this*")
	if p.visionCapable("gpt-5-mini") || !p.visionCapable("x/only-this-1") {
		t.Error("replace mode broken")
	}
}

func TestModelsListingAdvertisesModalities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[
			{"id":"seer","meta":{"llamaswap":{"isVision":true,"context":65536}}},
			{"id":"blind","meta":{"llamaswap":{"isVision":false}}},
			{"id":"flash-next-kvx","meta":{"llamaswap":{"context":98304}}},
			{"id":"mystery"}]}`))
	}))
	defer upstream.Close()
	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: upstream.URL, DefaultModel: "seer", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s.SetSetting("vision_models", "+fred/*flash-next*")
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var out struct {
		Data []struct {
			ID   string              `json:"id"`
			Mods []string            `json:"input_modalities"`
			Sees *bool               `json:"supports_vision"`
			Arch map[string][]string `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range out.Data {
		switch {
		case m.Sees == nil:
			got[m.ID] = "absent"
		case *m.Sees:
			got[m.ID] = "image"
			if len(m.Mods) != 2 || m.Mods[1] != "image" || len(m.Arch["input_modalities"]) != 2 {
				t.Errorf("%s: modalities %v arch %v", m.ID, m.Mods, m.Arch)
			}
		default:
			got[m.ID] = "text"
			if len(m.Mods) != 1 {
				t.Errorf("%s: modalities %v", m.ID, m.Mods)
			}
		}
	}
	want := map[string]string{
		"fred/seer":           "image",  // provider metadata
		"fred/blind":          "text",   // provider says no
		"fred/flash-next-kvx": "image",  // glob from the setting
		"fred/mystery":        "absent", // unknown: no field, harness decides
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: got %q want %q", id, got[id], w)
		}
	}
}
