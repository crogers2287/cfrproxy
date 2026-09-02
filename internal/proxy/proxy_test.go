package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"https://dialagram.me/router/v1/":                 "https://dialagram.me/router/v1",
		"dialagram.me/router/v1":                          "https://dialagram.me/router/v1",
		"https://dialagram.me/router/v1/chat/completions": "https://dialagram.me/router/v1",
		"https://api.anthropic.com/v1/messages":           "https://api.anthropic.com/v1",
		"fred:9069":                                       "http://fred:9069",
		"localhost:11434":                                 "http://localhost:11434",
		"192.168.1.5:8080/api/chat":                       "http://192.168.1.5:8080",
		"  https://openrouter.ai  ":                       "https://openrouter.ai",
	}
	for in, want := range cases {
		if got := NormalizeBase(in); got != want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func newDiscoveryStore(t *testing.T) *store.Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfrproxy-proxy-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	// The per-provider fallback chain is off by default in production (see
	// providerfallback.go); the legacy tests here were written when it was
	// unconditional, so opt in. TestProviderFallbackGatedBySetting covers the
	// default.
	if err := s.SetSetting("provider_fallback", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	// httptest.NewRequest peers come from 192.0.2.1 (TEST-NET-1), which the
	// data-plane trust model refuses by default. Trust it here, on top of the
	// defaults, so handler tests exercise routing rather than the gate.
	if err := s.SetSetting("trusted_cidrs", "192.0.2.0/24,127.0.0.0/8,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10"); err != nil {
		t.Fatal(err)
	}
	return s
}

// mock router that serves chat only under /api/v1 (openrouter-style)
func TestDiscoverBaseFindsApiV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/chat/completions" {
			w.WriteHeader(401) // exists but wants auth — must still count
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL})
	if base != srv.URL+"/api/v1" {
		t.Errorf("want %s/api/v1, got %s (note %q)", srv.URL, base, note)
	}
	if !strings.Contains(note, "resolved") {
		t.Errorf("note should mention resolution: %q", note)
	}
}

// base already correct → kept, verified
func TestDiscoverBaseKeepsWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(200)
			w.Write([]byte(`{"choices":[]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL})
	if base != srv.URL {
		t.Errorf("base changed unexpectedly: %s", base)
	}
	if !strings.Contains(note, "verified") {
		t.Errorf("note should say verified: %q", note)
	}
}

// nothing responds → keep normalized input, warn
func TestDiscoverBaseNothingResponds(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, note := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL + "/nope"})
	if base != srv.URL+"/nope" {
		t.Errorf("base should stay as entered: %s", base)
	}
	if !strings.Contains(note, "warning") {
		t.Errorf("expected warning note: %q", note)
	}
}

// pasted full endpoint URL → stripped, then verified
func TestDiscoverBaseStripsPastedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/router/v1/chat/completions" {
			w.WriteHeader(400) // exists; bad probe body is fine
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	p := New(newDiscoveryStore(t))
	base, _ := p.DiscoverBase(context.Background(), store.Provider{Type: "openai", BaseURL: srv.URL + "/router/v1/chat/completions"})
	if base != srv.URL+"/router/v1" {
		t.Errorf("want stripped base, got %s", base)
	}
}

// primary 503s → one retry → failover provider answers; trace notes failover
func TestFailover(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" { // ignore model-list scans
			w.WriteHeader(404)
			return
		}
		primaryHits++
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"upstream timeout"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"backup-model","choices":[{"message":{"role":"assistant","content":"saved"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "backup-model", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/backup-model"}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"primary/pm","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200 via failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "saved") {
		t.Fatalf("response not from backup: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "⚠️ failover: primary") || !strings.Contains(rec.Body.String(), "backup-model active") {
		t.Fatalf("failover alert missing/incomplete in visible content: %s", rec.Body.String())
	}
	// the upstream error body must NOT be echoed into the chat — it belongs on
	// the trace, not in front of the user on every tool-loop call
	if strings.Contains(rec.Body.String(), "[cfrproxy]") || strings.Contains(rec.Body.String(), "unavailable —") {
		t.Errorf("verbose failover banner leaked into content: %s", rec.Body.String())
	}
	if primaryHits != 2 {
		t.Errorf("want 2 attempts on primary (1 retry), got %d", primaryHits)
	}
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || traces[0].Provider != "backup" || !strings.Contains(traces[0].Err, "failover from primary") {
		t.Errorf("trace should record failover to backup: %+v", traces)
	}
}

// non-transient errors (auth) must NOT fail over
func TestNoFailoverOn401(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer primary.Close()
	backupHits := 0
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			backupHits++
		}
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "b", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/b"})
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"primary/pm","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 401 {
		t.Errorf("401 should pass through, got %d", rec.Code)
	}
	if backupHits != 0 {
		t.Errorf("backup should not be hit on auth error, got %d hits", backupHits)
	}
}

// scoped /p/{provider}/ mount lists only that provider's models (bare) and
// forces routing to it regardless of the model prefix sent.
func TestScopedProviderMount(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"alpha"},{"id":"beta"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"alpha","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer backend.Close()
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "scoped", Type: "openai", BaseURL: backend.URL, DefaultModel: "alpha", Priority: 10, Enabled: true})
	// a second provider that must NOT receive scoped traffic
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("other provider should not be hit") }))
	defer other.Close()
	s.SaveProvider(&store.Provider{Name: "other", Type: "openai", BaseURL: other.URL, DefaultModel: "z", Priority: 5, Enabled: true})

	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)

	// scoped model list = bare ids from that provider only
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/p/scoped/v1/models", nil))
	if !strings.Contains(rec.Body.String(), `"alpha"`) || strings.Contains(rec.Body.String(), "scoped/alpha") {
		t.Errorf("scoped models should be bare ids: %s", rec.Body.String())
	}

	// scoped chat with a bare model → routed to 'scoped' even though 'other'
	// has higher priority
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/p/scoped/v1/chat/completions",
		strings.NewReader(`{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("scoped chat failed: %d %s", rec.Code, rec.Body.String())
	}
}

// auto router: classifier answer picks the bucket route; failures use default
func TestAutoRoute(t *testing.T) {
	classifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"code"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer classifier.Close()
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "cls", Type: "openai", BaseURL: classifier.URL, DefaultModel: "tiny", Priority: 10, Enabled: true})
	s.SetSetting("auto_router", `{"enabled":true,"classifier":"cls/tiny","routes":{"code":"cls/coder-model","default":"cls/general"}}`)
	p := New(s)
	req := &wire.Request{Messages: []wire.Msg{{Role: "user", Content: "write me a go function"}}}
	target, bucket := p.AutoRoute(context.Background(), req)
	if bucket != "code" || target != "cls/coder-model" {
		t.Errorf("want code route, got bucket=%s target=%s", bucket, target)
	}
	// disabled -> no routing
	s.SetSetting("auto_router", `{"enabled":false}`)
	if tgt, _ := p.AutoRoute(context.Background(), req); tgt != "" {
		t.Errorf("disabled router should return empty, got %s", tgt)
	}
}

func TestEndsWithVersion(t *testing.T) {
	cases := map[string]bool{
		"https://api.z.ai/api/coding/paas/v4": true,
		"https://x/router/v1":                 true,
		"https://api.anthropic.com/v1beta":    true,
		"http://fred:9069":                    false,
		"https://openrouter.ai/api":           false,
		"https://openrouter.ai/api/v1":        true,
	}
	for in, want := range cases {
		if got := endsWithVersion(in); got != want {
			t.Errorf("endsWithVersion(%q)=%v want %v", in, got, want)
		}
	}
}

// A /p/ mount must resolve to the provider whatever the spelling, and must
// never fall through to another provider. Hermes configs carried
// "/p/Qwen%20/v1" (trailing space) and lowercase "/p/qwen/v1"; both missed the
// prefix match in ResolveModel and were answered by a different provider whose
// catalog happened to fuzzy-match the model id.
func TestScopedMountNameIsCanonicalised(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"qwen3.7-max"}]}`))
			return
		}
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from-scoped"}}]}`))
	}))
	defer backend.Close()
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"qwen3.7-max-local"}]}`))
			return
		}
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from-decoy"}}]}`))
	}))
	defer decoy.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "Qwen", Type: "openai", BaseURL: backend.URL, DefaultModel: "qwen3.7-max", Priority: 10, Enabled: true})
	// A second provider that is the *higher-priority* fallback (lower number),
	// mirroring production: when the scoped prefix failed to match, ResolveModel
	// fell all the way through to "highest-priority provider + its default
	// model" and answered from here instead of from Qwen.
	s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: decoy.URL, DefaultModel: "qwen3.7-max-local", Priority: 5, Enabled: true})
	mux := http.NewServeMux()
	New(s).Register(mux)

	for _, mount := range []string{"Qwen", "qwen", "Qwen%20", "QWEN"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/p/"+mount+"/v1/models", nil))
		if !strings.Contains(rec.Body.String(), "qwen3.7-max") || strings.Contains(rec.Body.String(), "qwen3.7-max-local") {
			t.Errorf("/p/%s/v1/models listed the wrong provider: %s", mount, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/p/"+mount+"/v1/chat/completions",
			strings.NewReader(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Errorf("/p/%s chat failed: %d %s", mount, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "from-scoped") {
			t.Errorf("/p/%s chat was answered by the wrong provider: %s", mount, rec.Body.String())
		}
	}

	// an unknown mount must be a loud 404, not a silent reroute
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/p/nosuchprovider/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 404 {
		t.Errorf("unknown /p/ mount should 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// resetFailoverNotices clears the package-level banner-suppression cache so
// tests don't leak state into each other.
func resetFailoverNotices() {
	noticeCache.mu.Lock()
	noticeCache.m = map[string]time.Time{}
	noticeCache.mu.Unlock()
}

// A harness tool-loop makes several model calls per user turn. The failover
// banner must fire once and then stay quiet, instead of stacking a copy into
// every reply — even though the prompt churns between calls.
func TestFailoverBannerRateLimited(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"upstream timeout"}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"backup-model","choices":[{"message":{"role":"assistant","content":"saved"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "backup-model", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/backup-model"})
	mux := http.NewServeMux()
	New(s).Register(mux)
	resetFailoverNotices()

	call := func(body string) string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("want 200 via failover, got %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	// Every call carries a DIFFERENT system prompt, exactly as a live harness
	// does (Hermes injects the current time, memories, system-reminders). This
	// is what defeated the original conversation-fingerprint key.
	turn := func(i int) string {
		return fmt.Sprintf(`{"model":"primary/pm","max_tokens":10,"system":"You are a bot. Current time 19:4%d","messages":[{"role":"user","content":"step %d"}]}`, i, i)
	}
	banners := 0
	for i := 0; i < 6; i++ {
		out := call(turn(i))
		if strings.Contains(out, "failover:") {
			banners++
		} else if i == 0 {
			t.Fatalf("first call must announce the failover: %s", out)
		}
	}
	if banners != 1 {
		t.Errorf("banner should be rate-limited to 1, got %d across 6 prompt-churning calls", banners)
	}
	// the one that fired must be the terse form, not the old verbose one
	resetFailoverNotices()
	if out := call(turn(9)); !strings.Contains(out, "backup-model active") || strings.Contains(out, "[cfrproxy]") {
		t.Errorf("expected terse banner after TTL reset: %s", out)
	}
}

func TestFailureLabel(t *testing.T) {
	cases := map[string]string{
		`Qwen: HTTP 429 {"code":"insufficient_quota","message":"Your token-plan 5-hour quota has been exhausted."}`: "quota exhausted",
		"grok: usage cap (HTTP 402) balance exhausted":                                                              "quota exhausted",
		"codex: context overflow (HTTP 400) too large":                                                              "context overflow",
		"x: HTTP 429 slow down":                       "rate limited",
		"x: HTTP 401 bad key":                         "auth failed",
		"x: HTTP 503 overloaded":                      "upstream error",
		"x: dial tcp 1.2.3.4:443: connection refused": "unreachable",
		"x: something nobody has seen before":         "unavailable",
	}
	for in, want := range cases {
		if got := failureLabel(in); got != want {
			t.Errorf("failureLabel(%.50q) = %q, want %q", in, got, want)
		}
	}
}

// A provider that fails must get a trace row under ITS OWN name. Previously the
// only record lived inside the successful provider's error text, so a provider
// failing 100% of requests showed an empty, healthy-looking panel.
func TestFailedAttemptGetsItsOwnTrace(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota has been exhausted"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"backup-model","choices":[{"message":{"role":"assistant","content":"saved"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "backup-model", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/backup-model"})
	mux := http.NewServeMux()
	New(s).Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"primary/pm","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("want 200 via failover, got %d: %s", rec.Code, rec.Body.String())
	}
	// the banner must name the provider that dropped out AND why
	body := rec.Body.String()
	if !strings.Contains(body, "primary") || !strings.Contains(body, "quota exhausted") {
		t.Errorf("banner should name the failed provider and reason: %s", body)
	}

	traces, _ := s.Traces(0, 20)
	var failRow, okRow *store.Trace
	for i := range traces {
		switch traces[i].Provider {
		case "primary":
			failRow = &traces[i]
		case "backup":
			okRow = &traces[i]
		}
	}
	if failRow == nil {
		t.Fatalf("no trace filed under the FAILED provider 'primary': %+v", traces)
	}
	if failRow.Status != 429 {
		t.Errorf("failed attempt should record its HTTP status, got %d", failRow.Status)
	}
	if !strings.Contains(failRow.Err, "quota") {
		t.Errorf("failed attempt should record the reason, got %q", failRow.Err)
	}
	if okRow == nil || okRow.Status != 200 {
		t.Errorf("successful provider row missing/wrong: %+v", okRow)
	}
}

// An image request rejected with wording visionFailure has never seen must
// STILL reach the vision chain. Enumerating provider phrasings is a losing
// game — this pins the structural rule, not the pattern list.
func TestVisionFallbackOnUnrecognizedImageRejection(t *testing.T) {
	var visionGotImage bool
	var visionBody string
	blind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(400)
		// deliberately says nothing about images, vision, or any known pattern
		w.Write([]byte(`{"error":{"message":"schema violation at messages[0]: variant not in enum"}}`))
	}))
	defer blind.Close()
	seer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		visionBody = string(b)
		visionGotImage = bodyHasImage(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"seer-model","choices":[{"message":{"role":"assistant","content":"a red circle"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer seer.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "seer", Type: "openai", BaseURL: seer.URL, DefaultModel: "seer-model", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "blind", Type: "openai", BaseURL: blind.URL, DefaultModel: "blind-model", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["seer/seer-model"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	imgReq := `{"model":"blind/blind-model","max_tokens":50,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReq)))

	if rec.Code != 200 {
		t.Fatalf("want 200 via vision fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a red circle") {
		t.Fatalf("answer did not come from the vision target: %s", rec.Body.String())
	}
	// the whole point: a vision model that never received the picture would
	// answer confidently and wrongly, which is worse than the original 400
	if !visionGotImage {
		t.Fatalf("vision target received no image part — body was: %s", visionBody)
	}
	if !strings.Contains(visionBody, "iVBORw0KGgoAAAANSUhEUg==") {
		t.Fatalf("image payload did not survive the hop: %s", visionBody)
	}
}

// The image rule must not turn every 4xx into a failover: a text request that
// a provider rejects still fails fast.
func TestTextRequestStillHardFailsOn400(t *testing.T) {
	blind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"schema violation at messages[0]: variant not in enum"}}`))
	}))
	defer blind.Close()
	seer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Error("vision target must not be tried for a text-only request")
		}
		w.WriteHeader(404)
	}))
	defer seer.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "seer", Type: "openai", BaseURL: seer.URL, DefaultModel: "seer-model", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "blind", Type: "openai", BaseURL: blind.URL, DefaultModel: "blind-model", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["seer/seer-model"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"blind/blind-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == 200 {
		t.Fatalf("text 400 must not fail over to the vision chain: %s", rec.Body.String())
	}
}

// The failure mode no error-driven rule can catch: a text-only model handed a
// picture does not fail, it invents an answer and returns 200. The proactive
// gate must route the image away BEFORE that happens.
func TestBlindModelNeverSeesImageRequest(t *testing.T) {
	blindHits := 0
	blind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		blindHits++
		// the hallucination: confident, wrong, and a perfectly healthy 200
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"a pink square and a yellow circle"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer blind.Close()
	var seerGotImage bool
	seer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		seerGotImage = bodyHasImage(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"gemini-3-flash","choices":[{"message":{"role":"assistant","content":"a red circle and a blue rectangle"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer seer.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "gemini", Type: "openai", BaseURL: seer.URL, DefaultModel: "gemini-3-flash", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: blind.URL, DefaultModel: "deepseek-v4-flash", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["gemini/gemini-3-flash"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	imgReq := `{"model":"fred/deepseek-v4-flash","max_tokens":50,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what shapes are these"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReq)))

	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if blindHits != 0 {
		t.Errorf("the blind model was sent an image it cannot see (%d hits)", blindHits)
	}
	if strings.Contains(rec.Body.String(), "pink square") {
		t.Fatalf("hallucinated answer reached the client: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "red circle") {
		t.Fatalf("vision target did not serve the request: %s", rec.Body.String())
	}
	if !seerGotImage {
		t.Error("vision target received no image part")
	}
	// An image response is forwarded verbatim to preserve the picture, so the
	// visible-content banner cannot be injected — the reason has to be on the
	// trace, or the reroute is invisible to the operator.
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || traces[0].Provider != "gemini" {
		t.Fatalf("trace should record the vision target serving: %+v", traces)
	}
	if !strings.Contains(traces[0].Err, "no image support") {
		t.Errorf("trace should say why it rerouted, got %q", traces[0].Err)
	}
}

// A text-only model must still serve TEXT requests directly — the gate keys on
// the request carrying an image, not on the model.
func TestBlindModelStillServesTextDirectly(t *testing.T) {
	blindHits := 0
	blind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		blindHits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer blind.Close()
	seer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Error("vision target must not be used for a text-only request")
		}
		w.WriteHeader(404)
	}))
	defer seer.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "gemini", Type: "openai", BaseURL: seer.URL, DefaultModel: "gemini-3-flash", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: blind.URL, DefaultModel: "deepseek-v4-flash", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["gemini/gemini-3-flash"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"fred/deepseek-v4-flash","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hi there") {
		t.Fatalf("text request should go straight to the model: %d %s", rec.Code, rec.Body.String())
	}
	if blindHits != 1 {
		t.Errorf("want exactly 1 hit on the primary, got %d", blindHits)
	}
}

// A vision-capable primary must be used directly — the gate must not reroute
// every image away from a model that can perfectly well see it.
func TestVisionCapablePrimaryIsNotRerouted(t *testing.T) {
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		primaryHits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"gemini-3-flash","choices":[{"message":{"role":"assistant","content":"served by primary"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer primary.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Error("vision chain must not be used when the primary already sees")
		}
		w.WriteHeader(404)
	}))
	defer other.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "claude", Type: "openai", BaseURL: other.URL, DefaultModel: "claude-opus-5", Priority: 20, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&store.Provider{Name: "gemini", Type: "openai", BaseURL: primary.URL, DefaultModel: "gemini-3-flash", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["claude/claude-opus-5"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	imgReq := `{"model":"gemini/gemini-3-flash","max_tokens":50,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReq)))

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "served by primary") {
		t.Fatalf("vision-capable primary should serve directly: %d %s", rec.Code, rec.Body.String())
	}
	if primaryHits != 1 {
		t.Errorf("want 1 hit on primary, got %d", primaryHits)
	}
}

// The global fallback chain is ordered for text availability and contains
// text-only models. Once a vision chain is in play, an image must never reach
// one of them — otherwise an exhausted vision target silently degrades into a
// confident invention from a model that never saw the picture.
func TestImageNeverReachesBlindGlobalFallback(t *testing.T) {
	blindGlobalHits := 0
	blindGlobal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		blindGlobalHits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"qwen3.8-max-preview","choices":[{"message":{"role":"assistant","content":"a serene mountain lake"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer blindGlobal.Close()
	// the only sighted target, and it is down
	seer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"Individual quota reached"}}`))
	}))
	defer seer.Close()
	blindPrimary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Error("blind primary must not receive an image")
		}
		w.WriteHeader(404)
	}))
	defer blindPrimary.Close()

	s := newDiscoveryStore(t)
	for _, p := range []store.Provider{
		{Name: "gemini", Type: "openai", BaseURL: seer.URL, DefaultModel: "gemini-3-flash", Priority: 20, Enabled: true},
		{Name: "Qwen", Type: "openai", BaseURL: blindGlobal.URL, DefaultModel: "qwen3.8-max-preview", Priority: 30, Enabled: true},
		{Name: "fred", Type: "openai", BaseURL: blindPrimary.URL, DefaultModel: "deepseek-v4-flash", Priority: 10, Enabled: true},
	} {
		pp := p
		if err := s.SaveProvider(&pp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetSetting("vision_fallback", `{"enabled":true,"targets":["gemini/gemini-3-flash"]}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("global_fallback", `{"enabled":true,"targets":["Qwen/qwen3.8-max-preview"]}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	imgReq := `{"model":"fred/deepseek-v4-flash","max_tokens":50,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgReq)))

	if blindGlobalHits != 0 {
		t.Errorf("image reached a blind global-fallback model (%d hits)", blindGlobalHits)
	}
	if strings.Contains(rec.Body.String(), "mountain lake") {
		t.Fatalf("invented answer reached the client: %s", rec.Body.String())
	}
	if rec.Code == 200 {
		t.Fatalf("no sighted model could serve; want an error, got 200: %s", rec.Body.String())
	}
	// the error must name the real problem, not a generic upstream failure
	if !strings.Contains(rec.Body.String(), "no vision-capable model could serve this image") {
		t.Errorf("error should name the cause: %s", rec.Body.String())
	}
}
