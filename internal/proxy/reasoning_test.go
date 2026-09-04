package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	return m
}

func TestApplyReasoningPerDialect(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	// openai: fills in, leaves the client's choice alone, force overrides, off → enable_thinking:false
	out, ok := applyReasoning([]byte(base), "openai", "low", false)
	if m := decode(t, out); !ok || m["reasoning_effort"] != "low" || m["messages"] == nil {
		t.Fatalf("openai fill: ok=%v body=%s", ok, out)
	}
	client := `{"model":"m","messages":[],"reasoning_effort":"high"}`
	if _, ok := applyReasoning([]byte(client), "openai", "low", false); ok {
		t.Fatal("openai: client level must survive without force")
	}
	out, ok = applyReasoning([]byte(client), "openai", "low", true)
	if m := decode(t, out); !ok || m["reasoning_effort"] != "low" {
		t.Fatalf("openai force: %s", out)
	}
	kw := `{"model":"m","messages":[],"chat_template_kwargs":{"enable_thinking":false,"keep":1}}`
	if _, ok := applyReasoning([]byte(kw), "openai", "medium", false); ok {
		t.Fatal("openai: a client enable_thinking in chat_template_kwargs counts as a choice")
	}
	out, _ = applyReasoning([]byte(kw), "openai", "medium", true)
	m := decode(t, out)
	kwm := m["chat_template_kwargs"].(map[string]any)
	if m["reasoning_effort"] != "medium" || kwm["enable_thinking"] != nil || kwm["keep"] != float64(1) {
		t.Fatalf("openai force over kwargs: %s", out)
	}
	out, _ = applyReasoning([]byte(base), "openai", "off", false)
	m = decode(t, out)
	if _, has := m["reasoning_effort"]; has {
		t.Fatalf("openai off must not send reasoning_effort: %s", out)
	}
	if m["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
		t.Fatalf("openai off: %s", out)
	}

	// responses
	out, _ = applyReasoning([]byte(`{"model":"m","input":"hi"}`), "responses", "xhigh", false)
	if decode(t, out)["reasoning"].(map[string]any)["effort"] != "xhigh" {
		t.Fatalf("responses: %s", out)
	}
	out, _ = applyReasoning([]byte(`{"model":"m","input":"hi"}`), "responses", "off", false)
	if decode(t, out)["reasoning"].(map[string]any)["effort"] != "none" {
		t.Fatalf("responses off: %s", out)
	}
	if _, ok := applyReasoning([]byte(`{"model":"m","reasoning":{"effort":"low"}}`), "responses", "xhigh", false); ok {
		t.Fatal("responses: client effort must survive without force")
	}

	// anthropic: budget stays under max_tokens; tiny max_tokens leaves it alone
	out, _ = applyReasoning([]byte(`{"model":"m","max_tokens":4096,"messages":[]}`), "anthropic", "xhigh", false)
	th := decode(t, out)["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(2048) {
		t.Fatalf("anthropic clamp: %s", out)
	}
	out, _ = applyReasoning([]byte(`{"model":"m","max_tokens":64000,"messages":[]}`), "anthropic", "medium", false)
	if decode(t, out)["thinking"].(map[string]any)["budget_tokens"] != float64(8192) {
		t.Fatalf("anthropic medium: %s", out)
	}
	if _, ok := applyReasoning([]byte(`{"model":"m","max_tokens":1000,"messages":[]}`), "anthropic", "low", false); ok {
		t.Fatal("anthropic: max_tokens too small to fit a budget must be left alone")
	}
	out, _ = applyReasoning([]byte(`{"model":"m","max_tokens":1000}`), "anthropic", "off", false)
	if decode(t, out)["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("anthropic off: %s", out)
	}

	// ollama
	out, _ = applyReasoning([]byte(`{"model":"m","messages":[]}`), "ollama", "off", false)
	if decode(t, out)["think"] != false {
		t.Fatalf("ollama off: %s", out)
	}

	// unknown dialect and garbage are untouched
	if _, ok := applyReasoning([]byte(base), "commandcode", "low", false); ok {
		t.Fatal("unknown dialect changed")
	}
	if _, ok := applyReasoning([]byte("nope"), "openai", "low", false); ok {
		t.Fatal("garbage changed")
	}
}

func TestReasoningForPrecedence(t *testing.T) {
	prov := store.Provider{ReasoningEffort: "medium"}
	if l, f := reasoningFor(nil, prov); l != "medium" || f {
		t.Fatalf("provider only: %s %v", l, f)
	}
	ep := &store.Endpoint{ReasoningEffort: "off", ReasoningForce: true}
	if l, f := reasoningFor(ep, prov); l != "off" || !f {
		t.Fatalf("share wins: %s %v", l, f)
	}
	if l, _ := reasoningFor(&store.Endpoint{}, prov); l != "medium" {
		t.Fatalf("blank share falls back to provider: %s", l)
	}
}

func TestNormalizeReasoning(t *testing.T) {
	for in, want := range map[string]string{"": "", " Low ": "low", "none": "off", "XHIGH": "xhigh"} {
		if got, err := store.NormalizeReasoning(in); err != nil || got != want {
			t.Errorf("%q → %q,%v want %q", in, got, err, want)
		}
	}
	if _, err := store.NormalizeReasoning("ultra"); err == nil {
		t.Error("bogus level accepted")
	}
}

// End to end through a share endpoint on the passthrough path: the provider
// default fills in, the share overrides it, and a client that chose its own
// level keeps it unless the share forces.
func TestShareReasoningLevelReachesUpstream(t *testing.T) {
	var bodies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, string(buf[:n]))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer backend.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: backend.URL, DefaultModel: "m", Priority: 10, Enabled: true, ReasoningEffort: "medium"})
	for _, ep := range []*store.Endpoint{
		{Name: "plain", APIKey: "cfr_plain", Enabled: true},
		{Name: "quiet", APIKey: "cfr_quiet", Enabled: true, ReasoningEffort: "off"},
		{Name: "forced", APIKey: "cfr_forced", Enabled: true, ReasoningEffort: "low", ReasoningForce: true},
	} {
		if err := s.SaveEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	New(s).Register(mux)
	post := func(ep, key, body string) string {
		bodies = nil
		r := httptest.NewRequest("POST", "/e/"+ep+"/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("/e/%s: %d %s", ep, rec.Code, rec.Body.String())
		}
		if len(bodies) != 1 {
			t.Fatalf("/e/%s: %d upstream calls", ep, len(bodies))
		}
		return bodies[0]
	}
	msg := `{"model":"local/m","messages":[{"role":"user","content":"hi"}]}`
	if b := post("plain", "cfr_plain", msg); !strings.Contains(b, `"reasoning_effort":"medium"`) {
		t.Errorf("provider default not applied: %s", b)
	}
	if b := post("quiet", "cfr_quiet", msg); !strings.Contains(b, `"enable_thinking":false`) || strings.Contains(b, "reasoning_effort") {
		t.Errorf("share off not applied: %s", b)
	}
	own := `{"model":"local/m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`
	if b := post("plain", "cfr_plain", own); !strings.Contains(b, `"reasoning_effort":"xhigh"`) {
		t.Errorf("client level overridden without force: %s", b)
	}
	if b := post("forced", "cfr_forced", own); !strings.Contains(b, `"reasoning_effort":"low"`) {
		t.Errorf("forced share did not override: %s", b)
	}
	// round-trips through the store, including validation
	if err := s.SaveEndpoint(&store.Endpoint{Name: "bad", APIKey: "k", Enabled: true, ReasoningEffort: "ultra"}); err == nil {
		t.Error("bogus level saved")
	}
	traces, _ := s.Traces(0, 1)
	if len(traces) == 0 || !strings.Contains(traces[0].Note, "thinking=low") {
		t.Errorf("trace note should record the applied level: %+v", traces)
	}
}
