package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// llama.cpp's overflow rejection shares no wording with the cloud providers',
// so it was filed as a plain malformed 400: no failover, no compression
// rescue, and the harness turn died. This is the exact body Tiel/qwen-w6800
// return (trace 143923).
func TestContextExceededRecognizesLlamaCpp(t *testing.T) {
	llamaCpp := []byte(`{"error":{"code":400,"message":"request (395264 tokens) exceeds the available context size (262144 tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":395264,"n_ctx":262144}}`)
	if !contextExceeded(llamaCpp) {
		t.Fatal("llama.cpp context overflow not classified as a context overflow")
	}

	// the phrasings that already worked must keep working
	for name, body := range map[string]string{
		"openai":    `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 200000 tokens.","code":"context_length_exceeded"}}`,
		"anthropic": `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`,
		"vllm":      `{"object":"error","message":"This model's maximum context length is 262144 tokens","type":"BadRequestError"}`,
		"ctxshift":  `{"error":{"message":"the request exceeds the available context size, try increasing the context size or enable context shift","code":400}}`,
	} {
		if !contextExceeded([]byte(body)) {
			t.Errorf("%s overflow no longer classified", name)
		}
	}

	// and unrelated 4xx bodies must NOT be mistaken for overflow, or a genuine
	// bad request would be retried and failed over pointlessly
	for name, body := range map[string]string{
		"auth":       `{"error":{"message":"invalid api key","type":"authentication_error"}}`,
		"bad model":  `{"error":{"message":"model not found: gpt-9","code":"model_not_found"}}`,
		"bad param":  `{"error":{"message":"unsupported parameter: enable_thinking","param":"enable_thinking"}}`,
		"rate limit": `{"error":{"message":"rate limit exceeded, retry later","type":"rate_limit_error"}}`,
	} {
		if contextExceeded([]byte(body)) {
			t.Errorf("%s misclassified as a context overflow", name)
		}
	}
}

// A pinned local model (no_fallback) that overflows must not simply die: the
// bulky tool results get compressed and the SAME model is retried, which is
// the only recovery available once the operator has pinned the provider.
func TestContextOverflowRescuedByCompression(t *testing.T) {
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		if len(b) > 20000 { // stands in for "exceeds n_ctx"
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":400,"message":"request (395264 tokens) exceeds the available context size (262144 tokens), try increasing it","type":"exceed_context_size_error"}}`))
			return
		}
		w.Write([]byte(`{"id":"x","model":"tiel","choices":[{"message":{"role":"assistant","content":"fits now"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "tiel", Priority: 10, Enabled: true, NoFallback: true}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	// a harness turn whose tool result is enormous — an MCP call that returned
	// the whole network, which is exactly how the live failure happened
	huge := strings.Repeat("client ap-42 rssi -61 ssid home band 5g state connected uptime 41230s; ", 700)
	body := `{"model":"local/tiel","max_tokens":16,"messages":[
	  {"role":"user","content":"audit my network"},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_clients","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + huge + `"}]}]}`

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("overflow was not rescued: HTTP %d %s", rec.Code, snipStr(rec.Body.String(), 300))
	}
	if !strings.Contains(rec.Body.String(), "fits now") {
		t.Fatalf("unexpected body: %s", snipStr(rec.Body.String(), 300))
	}
	if len(bodies) < 2 {
		t.Fatalf("want an overflow attempt then a compressed retry, got %d attempts", len(bodies))
	}
	first, last := len(bodies[0]), len(bodies[len(bodies)-1])
	if last >= first {
		t.Errorf("retry was not smaller: first=%d last=%d", first, last)
	}
	// the compression must be recorded, not silent
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 {
		t.Fatal("no trace recorded")
	}
	if traces[0].CMMsgs == 0 {
		t.Errorf("trace does not record the compression: %+v", traces[0])
	}
	if traces[0].CMBefore <= traces[0].CMAfter {
		t.Errorf("trace compression stats look wrong: before=%d after=%d", traces[0].CMBefore, traces[0].CMAfter)
	}
}

// Nothing bulky to compress: the rescue must not loop or mask the error — the
// overflow still surfaces.
func TestContextOverflowWithNothingToCompressStillFails(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"request (395264 tokens) exceeds the available context size (262144 tokens)","type":"exceed_context_size_error"}}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "tiel", Priority: 10, Enabled: true, NoFallback: true}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"local/tiel","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == 200 {
		t.Fatal("an unrescuable overflow must not report success")
	}
	if hits > 3 {
		t.Errorf("rescue looped: %d upstream attempts", hits)
	}
}

// A caller that omits max_tokens must get a ceiling, or llama.cpp's
// n_predict = -1 parks a 262K slot for hours and everything queues behind it.
func TestDefaultMaxTokens(t *testing.T) {
	// omitted -> filled in
	got := string(defaultMaxTokens([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), 4096))
	if !strings.Contains(got, `"max_tokens":4096`) {
		t.Errorf("cap not applied: %s", got)
	}
	// an explicit limit always wins, even a large one
	for _, body := range []string{
		`{"model":"m","max_tokens":28000,"messages":[]}`,
		`{"model":"m","max_completion_tokens":9000,"messages":[]}`,
		`{"model":"m","max_output_tokens":123,"messages":[]}`,
		`{"model":"m","options":{"num_predict":512},"messages":[]}`,
	} {
		if out := string(defaultMaxTokens([]byte(body), 4096)); strings.Contains(out, "4096") {
			t.Errorf("overrode an explicit client limit: %s -> %s", body, out)
		}
	}
	// disabled, malformed, and null cases
	orig := `{"model":"m","messages":[]}`
	if string(defaultMaxTokens([]byte(orig), 0)) != orig {
		t.Error("cap applied while disabled")
	}
	if string(defaultMaxTokens([]byte("not json"), 4096)) != "not json" {
		t.Error("mangled a non-JSON body")
	}
	if !strings.Contains(string(defaultMaxTokens([]byte(`{"max_tokens":null,"messages":[]}`), 777)), `"max_tokens":777`) {
		t.Error("explicit null should be treated as absent")
	}
	// the body must stay valid and keep its other fields
	out := defaultMaxTokens([]byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`), 2048)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("produced invalid JSON: %v", err)
	}
	if doc["model"] != "m" || doc["stream"] != true || doc["messages"] == nil {
		t.Errorf("lost fields: %v", doc)
	}
}

// The upstream ceiling must default to "none": a fixed http.Client.Timeout
// covers the whole streamed body, so it truncates long agent generations and
// kills requests that are merely queued behind busy slots.
func TestUpstreamTimeoutDefaultsToUnlimited(t *testing.T) {
	s := newDiscoveryStore(t)
	if got := upstreamTimeout(s); got != 0 {
		t.Errorf("default upstream timeout = %v, want 0 (no ceiling)", got)
	}
	if p := New(s); p.Client.Timeout != 0 {
		t.Errorf("client timeout = %v, want 0", p.Client.Timeout)
	}
	// an operator can still impose one
	if err := s.SetSetting("upstream_timeout_minutes", "45"); err != nil {
		t.Skipf("settings not writable in this harness: %v", err)
	}
	if got := upstreamTimeout(s); got != 45*time.Minute {
		t.Errorf("configured timeout = %v, want 45m", got)
	}
	for _, bad := range []string{"", "0", "-3", "abc"} {
		s.SetSetting("upstream_timeout_minutes", bad)
		if got := upstreamTimeout(s); got != 0 {
			t.Errorf("%q -> %v, want 0", bad, got)
		}
	}
}

// Connection-level failure must still be fast, or "no ceiling" would mean a
// dead host hangs the caller.
func TestProviderTransportFailsFastOnDeadHost(t *testing.T) {
	tr := providerTransport()
	if tr.TLSHandshakeTimeout == 0 || tr.TLSHandshakeTimeout > 30*time.Second {
		t.Errorf("TLS handshake timeout = %v", tr.TLSHandshakeTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("no DialContext set; a dead host would hang forever")
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0: a queued request waits minutes for its first byte", tr.ResponseHeaderTimeout)
	}
	// 203.0.113.0/24 is TEST-NET-3, guaranteed unroutable
	start := time.Now()
	c := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", "http://203.0.113.1:9/x", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected a dial failure to an unroutable address")
	}
	if el := time.Since(start); el > 40*time.Second {
		t.Errorf("dead host took %v to fail; connect-level timeout not applied", el)
	}
}
