package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestRejectedParam(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			// the real production body, verbatim
			"alibaba enable_thinking",
			`{"error":{"code":"invalid_parameter_error","param":null,"message":"The value of the enable_thinking parameter is restricted to True.","type":"invalid_request_error"},"id":"chatcmpl-1"}`,
			"enable_thinking",
		},
		{"structured param field", `{"error":{"param":"top_k","message":"nope"}}`, "top_k"},
		{"quoted parameter", `{"error":{"message":"parameter 'frequency_penalty' is not supported"}}`, "frequency_penalty"},
		{"unsupported parameter", `{"error":{"message":"Unsupported parameter: logprobs"}}`, "logprobs"},
		{"openai unrecognized", `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`, "reasoning_effort"},

		// must NOT fire
		{"no parameter named", `{"error":{"message":"invalid api key"}}`, ""},
		{"structural key is protected", `{"error":{"param":"messages","message":"bad"}}`, ""},
		{"nested path is not a top-level key", `{"error":{"param":"messages[0].content","message":"bad"}}`, ""},
		{"tool_choice is protected", `{"error":{"message":"The value of the tool_choice parameter is restricted"}}`, ""},
		{"garbage", `not json at all`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rejectedParam([]byte(c.body)); got != c.want {
				t.Errorf("rejectedParam = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripBodyParam(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"enable_thinking":false}`)
	out, ok := stripBodyParam(body, "enable_thinking")
	if !ok {
		t.Fatal("param not reported as removed")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, still := m["enable_thinking"]; still {
		t.Error("enable_thinking survived the strip")
	}
	if m["model"] != "m" || m["messages"] == nil {
		t.Errorf("strip damaged the rest of the body: %s", out)
	}
	if _, ok := stripBodyParam(body, "absent"); ok {
		t.Error("absent key reported as removed")
	}
}

// End to end: the provider 400s naming a parameter, cfrproxy drops it and
// retries, and the harness gets a normal 200 instead of the error.
func TestProviderRejectedParamIsDroppedAndRetried(t *testing.T) {
	var bodies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body, "enable_thinking") {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":"invalid_parameter_error","param":null,"message":"The value of the enable_thinking parameter is restricted to True.","type":"invalid_request_error"}}`))
			return
		}
		w.Write([]byte(`{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer backend.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "qwenish", Type: "openai", BaseURL: backend.URL, DefaultModel: "m", Priority: 10, Enabled: true})
	mux := http.NewServeMux()
	New(s).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"qwenish/m","messages":[{"role":"user","content":"hi"}],"enable_thinking":false}`)))

	if rec.Code != 200 {
		t.Fatalf("want 200 after dropping the rejected param, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovered") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (reject then retry), got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], "enable_thinking") {
		t.Error("first call should have carried the parameter")
	}
	if strings.Contains(bodies[1], "enable_thinking") {
		t.Error("retry still carried the rejected parameter")
	}
	// the drop is recorded, not silent
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || !strings.Contains(traces[0].Err, "enable_thinking") {
		t.Errorf("trace should record the dropped parameter: %+v", traces)
	}
}

// A 400 that names no droppable parameter must still fail fast — one call, no
// speculative retry loop.
func TestUnrelated400IsNotRetried(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		calls++
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer backend.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "p", Type: "openai", BaseURL: backend.URL, DefaultModel: "m", Priority: 10, Enabled: true})
	mux := http.NewServeMux()
	New(s).Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"p/m","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Errorf("want the 400 surfaced, got %d", rec.Code)
	}
	if calls != 1 {
		t.Errorf("want exactly 1 upstream call, got %d", calls)
	}
}
