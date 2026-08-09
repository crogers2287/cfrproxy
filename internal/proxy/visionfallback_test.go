package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestBodyHasImage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"plain text openai", `{"model":"m","messages":[{"role":"user","content":"hello"}]}`, false},
		{"text content parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, false},
		{"openai image_url", `{"messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`, true},
		{"openai input_image", `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,x"}]}]}`, true},
		{"anthropic image", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`, true},
		{"ollama images array", `{"messages":[{"role":"user","content":"describe","images":["iVBOR"]}]}`, true},
		{"ollama empty images", `{"messages":[{"role":"user","content":"describe","images":[]}]}`, false},
		{"malformed json", `{not json`, false},
		// the word appearing in prose must not trip the walk
		{"word image in prose", `{"messages":[{"role":"user","content":"describe the image i sent earlier"}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bodyHasImage([]byte(c.body)); got != c.want {
				t.Fatalf("bodyHasImage(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestVisionFailure(t *testing.T) {
	// The exact payload that took Winston down, from the captured request dump.
	qwen := `{"error":{"code":"invalid_parameter_error","param":null,"message":"Download multimodal file timed out","type":"invalid_request_error"}}`
	if !visionFailure([]byte(qwen)) {
		t.Fatal("qwen multimodal timeout must classify as a vision failure")
	}

	positives := []string{
		`{"error":{"message":"This model does not support image input"}}`,
		`{"error":{"message":"Invalid image: could not decode"}}`,
		`{"error":{"message":"image too large"}}`,
		`{"error":{"message":"Failed to download image from url"}}`,
		`{"error":{"message":"vision is not supported for this model"}}`,
	}
	for _, p := range positives {
		if !visionFailure([]byte(p)) {
			t.Errorf("expected vision failure for %s", p)
		}
	}

	// Must NOT swallow unrelated errors — those have their own handling paths
	// and misclassifying them would route perfectly good requests off-provider.
	negatives := []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"context length exceeded"}}`,
		`{"error":{"message":"rate limit reached"}}`,
		`{"error":{"message":"model not found"}}`,
		`{"error":{"message":"insufficient quota"}}`,
	}
	for _, n := range negatives {
		if visionFailure([]byte(n)) {
			t.Errorf("must not classify as vision failure: %s", n)
		}
	}
}

// A context-overflow body and a vision body must not both match the other's
// classifier, or a request would take the wrong recovery path.
func TestVisionAndContextClassifiersAreDisjoint(t *testing.T) {
	vision := []byte(`{"error":{"message":"Download multimodal file timed out"}}`)
	ctxOver := []byte(`{"error":{"message":"prompt is too long: 300000 tokens > 200000 maximum"}}`)

	if contextExceeded(vision) {
		t.Error("vision failure must not classify as context overflow")
	}
	if visionFailure(ctxOver) {
		t.Error("context overflow must not classify as vision failure")
	}
}

// The rejection that actually broke this in production: OpenCode's Rust-backed
// console answers an image part with a serde enum error, which says nothing
// about vision at all.
func TestVisionFailureRecognizesSerdeUnknownVariant(t *testing.T) {
	opencode := `{"error":{"param":null,"type":"invalid_request_error","code":"invalid_request_error",` +
		`"message":"Error from provider (Console): Upstream request failed: [invalid_request_error] ` +
		"Failed to deserialize the JSON body into the target type: messages[0]: unknown variant `image_url`, expected `text` at line 1 column 1853\"}}"
	if !visionFailure([]byte(opencode)) {
		t.Fatal("serde `unknown variant \\`image_url\\`` must classify as a vision failure")
	}
	for _, p := range []string{
		"{\"error\":{\"message\":\"unknown variant `image`, expected one of `text`\"}}",
		`{"error":{"message":"unknown variant \"image_url\""}}`,
	} {
		if !visionFailure([]byte(p)) {
			t.Errorf("want vision failure for %s", p)
		}
	}
}

// Cases are real model ids from this deployment. The provider-prefixed ones are
// the whole reason the list matches substrings instead of vendor prefixes: a
// `claude-*` prefix rule would call `claude-opencode-go-deepseek-v4-flash`
// vision-capable and hand it a picture it cannot see.
func TestVisionCapable(t *testing.T) {
	p := New(newDiscoveryStore(t))
	cases := []struct {
		model string
		want  bool
	}{
		// genuinely multimodal
		{"gemini-3-flash", true},
		{"gpt-5.6-terra", true},
		{"grok-4.5", true},
		{"claude-opus-5", true},
		{"claude-sonnet-5", true},
		{"q27b-vl", true},
		{"qwen36-27b-vl", true},
		{"llava-1.6", true},
		{"llama-3.2-11b-vision", true},
		// renamed-but-still-multimodal behind a provider prefix
		{"claude-opencode-sonnet-5", true},
		{"claude-opencode-opus-4-8", true},
		{"claude-opencode-gpt-5.6-terra", true},
		{"command/claude-command-gpt-5.6-sol", true},
		// text-only wearing a claude- prefix: the trap this list must not fall into
		{"claude-opencode-deepseek-v4-flash-free", false},
		{"claude-opencode-go-deepseek-v4-flash", false},
		{"claude-command-deepseek-v4-flash", false},
		{"claude-command-glm-5.2", false},
		// plainly text-only
		{"deepseek-v4-flash", false},
		{"deepseek-v4-pro", false},
		{"qwen3.8-max", false},
		{"kimi-k2.6", false},
		{"minimax-m3", false},
		// provider scope must be stripped before matching
		{"fred/deepseek-v4-flash", false},
		{"gemini/gemini-3-flash", true},
	}
	for _, c := range cases {
		if got := p.visionCapable(c.model); got != c.want {
			t.Errorf("visionCapable(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestVisionCapableKillSwitchAndOverride(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)

	// "-" disables the proactive gate: everything counts as capable, so nothing
	// is ever rerouted before it has actually failed.
	if err := s.SetSetting("vision_models", "-"); err != nil {
		t.Fatal(err)
	}
	if !p.visionCapable("deepseek-v4-flash") {
		t.Fatal(`vision_models="-" must treat every model as capable`)
	}

	// a custom list REPLACES the defaults rather than extending them
	if err := s.SetSetting("vision_models", "my-eyes-*, *-seer"); err != nil {
		t.Fatal(err)
	}
	if !p.visionCapable("my-eyes-7b") || !p.visionCapable("local-seer") {
		t.Fatal("custom globs must match")
	}
	if p.visionCapable("gemini-3-flash") {
		t.Fatal("a custom list must replace the defaults, not extend them")
	}
}

// A provider that publishes per-model capability outranks the name heuristic.
// llama-swap does this; a local build called "qwythos-9b" matches no naming
// convention, but its own server knows it has a vision projector.
func TestVisionCapableForPrefersProviderDeclaration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// qwythos: unguessable name, really sees.  fake-vl: guessable name,
		// really blind.  bare: no declaration at all.
		w.Write([]byte(`{"data":[
		  {"id":"qwythos-9b","meta":{"llamaswap":{"isVision":true}}},
		  {"id":"fake-vl","meta":{"llamaswap":{"isVision":false}}},
		  {"id":"mystery-7b"}]}`))
	}))
	defer srv.Close()

	s := newDiscoveryStore(t)
	p := New(s)
	prov := store.Provider{Name: "jetson", Type: "openai", BaseURL: srv.URL, Enabled: true}
	if err := s.SaveProvider(&prov); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// the name says nothing, the provider says yes → sighted
	if p.visionCapable("qwythos-9b") {
		t.Fatal("precondition: the glob list must not already know this name")
	}
	if !p.visionCapableFor(ctx, prov, "qwythos-9b") {
		t.Error("provider declaration isVision:true must make the model sighted")
	}

	// the name says yes, the provider says no → the provider wins, or we hand
	// a picture to something that cannot read it
	if !p.visionCapable("fake-vl") {
		t.Fatal("precondition: *-vl must match the glob list")
	}
	if p.visionCapableFor(ctx, prov, "fake-vl") {
		t.Error("isVision:false must override a matching name glob")
	}

	// no declaration and no glob match → blind, the safe direction
	if p.visionCapableFor(ctx, prov, "mystery-7b") {
		t.Error("an undeclared, unrecognised model must be treated as blind")
	}
}

// Deduped so a picker does not show the same model twice.
func TestListModelsDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"a"},{"id":""}]}`))
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	ids, err := p.ListModels(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("want [a b], got %v", ids)
	}
}

func TestContextLengthResolutionOrder(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)
	prov := store.Provider{Name: "jetson", Type: "openai", BaseURL: "http://x", Enabled: true}

	// nothing known → advertise nothing rather than invent a number
	if got := p.ContextLengthFor(prov, "m"); got != 0 {
		t.Errorf("unknown must be 0, got %d", got)
	}

	// global default applies
	if err := s.SetSetting("default_context_length", "131072"); err != nil {
		t.Fatal(err)
	}
	if got := p.ContextLengthFor(prov, "m"); got != 131072 {
		t.Errorf("default = %d, want 131072", got)
	}

	// what the upstream declared beats the global default
	p.recordContextMeta("jetson", "m", 8192)
	if got := p.ContextLengthFor(prov, "m"); got != 8192 {
		t.Errorf("upstream declaration = %d, want 8192", got)
	}

	// the operator's per-provider override beats everything
	prov.ContextLength = 262144
	if got := p.ContextLengthFor(prov, "m"); got != 262144 {
		t.Errorf("provider override = %d, want 262144", got)
	}
}

func TestAsIntAcceptsStringAndNumber(t *testing.T) {
	// llama-swap sends context as a string ("8192"); other servers send a number
	for _, c := range []struct {
		in   any
		want int
		ok   bool
	}{
		{"8192", 8192, true}, {" 4096 ", 4096, true}, {float64(2048), 2048, true},
		{"", 0, false}, {"abc", 0, false}, {nil, 0, false},
	} {
		got, ok := asInt(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("asInt(%#v) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTokensPerSecExcludesThinkAndProxyTime(t *testing.T) {
	// 100 tokens produced in a 2s window out of a 5s call (1s upstream
	// think-time, 2s generating, 2s cfrproxy post-processing)
	tr := store.Trace{LatencyMS: 5000, TTFBMS: 1000, PostUS: 2000000, CompletionTokens: 100}
	if g := tr.GenMS(); g != 2000 {
		t.Fatalf("GenMS = %d, want 2000", g)
	}
	if tps := tr.TokensPerSec(); tps != 50 {
		t.Fatalf("TokensPerSec = %v, want 50", tps)
	}
	// no completion tokens → refuse to invent a rate
	if tps := (store.Trace{LatencyMS: 1000}).TokensPerSec(); tps != 0 {
		t.Fatalf("want 0 without tokens, got %v", tps)
	}
	// pre-upgrade rows have no TTFB/Post: fall back to total latency
	old := store.Trace{LatencyMS: 4000, CompletionTokens: 200}
	if tps := old.TokensPerSec(); tps != 50 {
		t.Fatalf("legacy row = %v, want 50", tps)
	}
}

// Regression: a non-streamed call has no observable generation window, because
// the upstream withholds headers until the completion is finished. Recording
// TTFB there collapsed gen-time to ~1ms and reported 80000 tok/s.
func TestNonStreamedRateIsNotAbsurd(t *testing.T) {
	// what the fixed code records for a non-stream: TTFB stays 0
	nonStream := store.Trace{LatencyMS: 6175, TTFBMS: 0, PostUS: 0, CompletionTokens: 80}
	tps := nonStream.TokensPerSec()
	if tps < 5 || tps > 40 {
		t.Fatalf("non-streamed rate = %.1f tok/s, want a plausible local-model rate", tps)
	}
	// and the streamed case still subtracts real think-time
	streamed := store.Trace{Stream: true, LatencyMS: 7246, TTFBMS: 2093, PostUS: 0, CompletionTokens: 207}
	if got := streamed.TokensPerSec(); got < 39 || got > 41 {
		t.Fatalf("streamed rate = %.1f, want ~40.2", got)
	}
}

// The first implementation stamped "post" after the relay had already
// finished, so the measured delta was always ~0 — and the raw-passthrough path
// returned before the marker was ever reached. Wrapping the upstream body is
// what makes the number real, so pin the wrapper's behaviour.
func TestLastByteReaderStampsFinalRead(t *testing.T) {
	lb := newLastByteReader(io.NopCloser(strings.NewReader("hello world")))
	if !lb.lastByte().IsZero() {
		t.Fatal("no read yet: timestamp must be zero so callers can tell it was never measured")
	}
	buf := make([]byte, 5)
	if _, err := lb.Read(buf); err != nil {
		t.Fatal(err)
	}
	first := lb.lastByte()
	if first.IsZero() {
		t.Fatal("a successful read must stamp the clock")
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := lb.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !lb.lastByte().After(first) {
		t.Fatal("each byte-producing read must advance the stamp — post-processing is measured from the LAST one")
	}
	// drain to EOF: a zero-byte read must not move the stamp forward, or the
	// idle wait for EOF would be counted as upstream time
	last := lb.lastByte()
	for {
		if _, err := lb.Read(buf); err != nil {
			break
		}
	}
	if lb.lastByte().Before(last) {
		t.Fatal("stamp went backwards")
	}
	if err := lb.Close(); err != nil {
		t.Fatal(err)
	}
}

// pp/tg are llama.cpp's prompt-processing and token-generation rates.
func TestPromptPerSecIsPrefillRate(t *testing.T) {
	// 2000 prompt tokens ingested in 1s of time-to-first-token
	tr := store.Trace{Stream: true, LatencyMS: 5000, TTFBMS: 1000, PromptTokens: 2000, CompletionTokens: 100}
	if pp := tr.PromptPerSec(); pp != 2000 {
		t.Errorf("pp = %v, want 2000 tok/s", pp)
	}

	// cached tokens were never re-processed; counting them would inflate pp by
	// exactly the cache hit ratio
	cached := store.Trace{Stream: true, TTFBMS: 1000, PromptTokens: 2000, CachedTokens: 1500}
	if pp := cached.PromptPerSec(); pp != 500 {
		t.Errorf("pp with cache = %v, want 500 (only the 500 uncached tokens)", pp)
	}

	// a non-streamed call cannot separate prefill from generation — refuse to
	// invent the split rather than report a made-up number
	nonStream := store.Trace{Stream: false, LatencyMS: 5000, TTFBMS: 0, PromptTokens: 2000}
	if pp := nonStream.PromptPerSec(); pp != 0 {
		t.Errorf("non-streamed pp = %v, want 0", pp)
	}

	// fully cached prompt: nothing was processed, so there is no rate
	allCached := store.Trace{Stream: true, TTFBMS: 500, PromptTokens: 900, CachedTokens: 900}
	if pp := allCached.PromptPerSec(); pp != 0 {
		t.Errorf("fully-cached pp = %v, want 0", pp)
	}
}

// usageFromBody had no "responses" case, so a Responses-API passthrough logged
// 0/0/0 for ALL three counters — not just cache. Latent while rawOK blocked
// that path; live the moment anything posts to cfrproxy's inbound
// /v1/responses against a Responses-capable openai provider.
func TestUsageFromBodyHandlesResponsesDialect(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":12000,"output_tokens":345,
	  "input_tokens_details":{"cached_tokens":8192}}}`)

	pt, ct, cached, ok := usageFromBody("responses", body)
	if !ok || pt != 12000 || ct != 345 || cached != 8192 {
		t.Fatalf("responses = (%d,%d,%d,%v), want (12000,345,8192,true)", pt, ct, cached, ok)
	}

	// the regression it guards: the chat-completions branch finds none of
	// those field names and reports nothing at all
	if _, _, _, ok := usageFromBody("openai", body); ok {
		t.Error("precondition: the chat-completions struct must NOT parse a Responses body — " +
			"if it does, this test no longer proves the dialect case is needed")
	}

	// streaming path: usage arrives inside the response.completed SSE event
	line := []byte(`data: {"type":"response.completed","response":{"usage":{` +
		`"input_tokens":900,"output_tokens":40,"input_tokens_details":{"cached_tokens":128}}}}`)
	pt, ct, cached, ok = usageFromStreamLine("responses", line)
	if !ok || pt != 900 || ct != 40 || cached != 128 {
		t.Fatalf("stream line = (%d,%d,%d,%v), want (900,40,128,true)", pt, ct, cached, ok)
	}

	// other dialects must be unaffected
	if pt, ct, cached, ok := usageFromBody("openai",
		[]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":5}}}`)); !ok || pt != 10 || ct != 2 || cached != 5 {
		t.Errorf("chat completions regressed: (%d,%d,%d,%v)", pt, ct, cached, ok)
	}
	if pt, ct, cached, ok := usageFromBody("anthropic",
		[]byte(`{"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":4}}`)); !ok || pt != 7 || ct != 3 || cached != 4 {
		t.Errorf("anthropic regressed: (%d,%d,%d,%v)", pt, ct, cached, ok)
	}
}

// A plan-gated 403 ("your tier cannot call this endpoint") must fail over, not
// hard-fail. Hard-failing turns into a harness retry storm — the shape of
// INCIDENT-001. Vendors that sell API access as a separate plan (Command Code's
// Provider tier among them) return exactly this when a subscription lapses.
func TestUsageExhaustedCoversPlanGating(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"upgrade_required","message":"Provider plan required for API access"}}`,
		`{"error":{"message":"This endpoint requires a paid plan"}}`,
		`{"error":{"message":"API access is not included in your plan"}}`,
		`{"error":{"type":"plan_required"}}`,
		`{"error":{"message":"Subscription required"}}`,
	} {
		if !usageExhausted([]byte(body)) {
			t.Errorf("plan gating must be failover-worthy: %s", body)
		}
	}
	// must NOT swallow ordinary auth failures — those stay hard errors so the
	// operator sees a bad key instead of silent, expensive rerouting
	for _, body := range []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"permission denied for this resource"}}`,
		`{"error":{"message":"authentication failed"}}`,
	} {
		if usageExhausted([]byte(body)) {
			t.Errorf("auth failure must NOT be treated as exhaustion: %s", body)
		}
	}
}

// A model the account cannot currently use arrives as a 400 and hard-failed,
// surfacing downstream as a dead agent. Seen live when a Command Code plan
// change made deepseek-v4-flash intermittently unavailable mid-session.
func TestUsageExhaustedCoversUnsupportedModel(t *testing.T) {
	live := `{"error":{"message":"Model \"claude-command-deepseek-v4-flash\" is not supported on this endpoint.","type":"invalid_request_error","param":"model","code":"unsupported_model"}}`
	if !usageExhausted([]byte(live)) {
		t.Fatal("the captured production payload must be failover-worthy")
	}
	for _, b := range []string{
		`{"error":{"message":"model not available for your account"}}`,
		`{"error":{"code":"unsupported_model"}}`,
	} {
		if !usageExhausted([]byte(b)) {
			t.Errorf("want failover for %s", b)
		}
	}
	// still must not swallow auth problems
	for _, b := range []string{
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"authentication failed"}}`,
	} {
		if usageExhausted([]byte(b)) {
			t.Errorf("auth failure must stay a hard error: %s", b)
		}
	}
}

// The failover banner is the only thing most users see, so a plan-gating
// rejection must not read as "quota exhausted" — that sends them to a billing
// meter instead of an upgrade page.
func TestFailureLabelDistinguishesPlanFromQuota(t *testing.T) {
	plan := "ccbudget: usage cap (HTTP 403) {\"error\":{\"message\":\"Your Go plan doesn't include API access. Upgrade to Provider or higher\",\"code\":\"upgrade_required\"}}"
	if got := failureLabel(plan); got != "plan has no API access" {
		t.Errorf("plan gating labelled %q", got)
	}
	unsup := `command: (HTTP 400) {"error":{"code":"unsupported_model","message":"Model \"x\" is not supported on this endpoint."}}`
	if got := failureLabel(unsup); got != "model unavailable there" {
		t.Errorf("unsupported model labelled %q", got)
	}
	// real quota exhaustion must still say so
	quota := `Qwen: HTTP 429 {"error":{"message":"Your token-plan 1-week quota has been exhausted"}}`
	if got := failureLabel(quota); got != "quota exhausted" {
		t.Errorf("real quota labelled %q", got)
	}
}
