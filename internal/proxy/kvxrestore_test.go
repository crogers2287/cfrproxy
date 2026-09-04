package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// kvxStub is an httptest double for kvxd's POST /v1/restore-for-prompt. It
// records every call and answers with whatever the test configured.
type kvxStub struct {
	*httptest.Server
	mu     sync.Mutex
	calls  int
	bodies [][]byte
	answer string
	delay  time.Duration
}

func newKVXStub(t *testing.T, answer string) *kvxStub {
	t.Helper()
	k := &kvxStub{answer: answer}
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != kvxRestorePath {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		k.mu.Lock()
		k.calls++
		k.bodies = append(k.bodies, body)
		delay := k.delay
		k.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(k.answer))
	}))
	t.Cleanup(k.Close)
	return k
}

func (k *kvxStub) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

func (k *kvxStub) last() []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.bodies) == 0 {
		return nil
	}
	return k.bodies[len(k.bodies)-1]
}

// kvxUpstream is a llama-swap stand-in that answers any chat completion and
// keeps the last body it was sent.
type kvxUpstreamStub struct {
	*httptest.Server
	mu   sync.Mutex
	body []byte
}

func (u *kvxUpstreamStub) last() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body
}

func kvxUpstream(t *testing.T) *kvxUpstreamStub {
	t.Helper()
	u := &kvxUpstreamStub{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.body = body
		u.mu.Unlock()
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"` + req.Model + `","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(u.Close)
	return u
}

// kvxProxy wires a provider named provName (serving the two-instance ornith
// affinity pool) in front of upstream, with the kvx_restore setting as given
// ("" = unset, the production default).
func kvxProxy(t *testing.T, provName, upstream, setting string) (*store.Store, *http.ServeMux) {
	t.Helper()
	return kvxProxyPools(t, provName, upstream, setting, twoInstance)
}

// kvxProxyPools is kvxProxy with an explicit model_pools value ("" = none).
func kvxProxyPools(t *testing.T, provName, upstream, setting, pools string) (*store.Store, *http.ServeMux) {
	t.Helper()
	poolAffinity.reset()
	resetFailoverNotices()
	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: provName, Type: "openai", BaseURL: upstream,
		DefaultModel: "ornith", Models: "ornith", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if pools != "" {
		if err := s.SetSetting("model_pools", pools); err != nil {
			t.Fatal(err)
		}
	}
	if setting != "" {
		if err := s.SetSetting("kvx_restore", setting); err != nil {
			t.Fatal(err)
		}
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	return s, mux
}

const kvxAgentBody = `{"model":"ornith","messages":[{"role":"system","content":"You are a coding agent.\nWorking directory: /srv/app"},{"role":"user","content":"%s"}],"tools":[{"type":"function","function":{"name":"read_file","description":"read a file","parameters":{"type":"object"}}}]}`

func kvxSend(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

func kvxLastNote(t *testing.T, s *store.Store) string {
	t.Helper()
	traces, err := s.Traces(0, 1)
	if err != nil || len(traces) == 0 {
		t.Fatalf("no trace: %v", err)
	}
	return traces[0].Note
}

func kvxSetting(url string, extra string) string {
	return `{"enabled":true,"url":"` + url + `"` + extra + `}`
}

const kvxHit = `{"restored":true,"covers_tokens":29601,"slot":1,"prefix":"5000f5c59855","seconds":0.83}`

// Unset setting = zero calls, and the trace carries no kvx note at all.
func TestKVXRestoreDisabledByDefault(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	s, mux := kvxProxy(t, "fred", kvxUpstream(t).URL, "")
	// Even a config that names the stub but leaves enabled unset stays off.
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times with the setting unset", n)
	}
	if note := kvxLastNote(t, s); strings.Contains(note, "kvx→") {
		t.Fatalf("note should not mention kvx: %q", note)
	}
	s.SetSetting("kvx_restore", `{"url":"`+kvx.URL+`"}`)
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "another new conversation", 1))
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times with enabled unset", n)
	}
}

// Called for a NEW conversation (why=cold/* or prefix) with the messages and
// tools exactly as forwarded; NOT called for a later turn of a bound
// conversation (why=conversation).
func TestKVXRestoreOnlyForNewConversations(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	up := kvxUpstream(t)
	s, mux := kvxProxy(t, "fred", up.URL, kvxSetting(kvx.URL, ""))

	// Turn 1 of conversation A: nobody has served this prefix — cold.
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if n := kvx.count(); n != 1 {
		t.Fatalf("cold conversation: kvxd called %d times, want 1", n)
	}
	note := kvxLastNote(t, s)
	if !strings.Contains(note, "pool→ornith-kvx-w6800 (cold/inflight)") {
		t.Fatalf("expected a cold pool pick, note=%q", note)
	}
	if !strings.Contains(note, "kvx→restored 29,601 (slot 1, 0.8s)") {
		t.Fatalf("expected the restore note, note=%q", note)
	}
	var sent struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(kvx.last(), &sent); err != nil {
		t.Fatalf("kvxd body: %v: %s", err, kvx.last())
	}
	if sent.Model != "ornith-kvx-w6800" {
		t.Errorf("kvxd got model %q, want the pool member the request is forwarded to", sent.Model)
	}
	if len(sent.Messages) != 2 || len(sent.Tools) != 1 {
		t.Errorf("kvxd got %d messages / %d tools, want 2 / 1: %s", len(sent.Messages), len(sent.Tools), kvx.last())
	}
	if !strings.Contains(string(sent.Messages[0]), "You are a coding agent.") || !strings.Contains(string(sent.Tools[0]), `"read_file"`) {
		t.Errorf("kvxd body is not the forwarded messages/tools: %s", kvx.last())
	}
	// kvxd must see the messages/tools bytes the upstream was actually sent —
	// not the client's bytes (passthrough re-marshals; translation rebuilds).
	var fwd, got struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	json.Unmarshal(up.last(), &fwd)
	json.Unmarshal(kvx.last(), &got)
	if fwd.Model != got.Model || string(got.Messages) != string(fwd.Messages) || string(got.Tools) != string(fwd.Tools) {
		t.Errorf("kvxd body differs from the forwarded body:\n kvxd     %s\n upstream %s", kvx.last(), up.last())
	}

	// Turn 2 of conversation A: bound, its slot is warm — no restore call.
	turn2 := strings.Replace(kvxAgentBody, "%s", "fix the parser", 1)
	turn2 = strings.Replace(turn2, `}],"tools"`, `},{"role":"assistant","content":"on it"},{"role":"user","content":"now run the tests"}],"tools"`, 1)
	kvxSend(t, mux, turn2)
	if n := kvx.count(); n != 1 {
		t.Fatalf("bound conversation: kvxd called %d times, want still 1", n)
	}
	note = kvxLastNote(t, s)
	if !strings.Contains(note, "(conversation)") {
		t.Fatalf("turn 2 should route by conversation, note=%q", note)
	}
	if strings.Contains(note, "kvx→") {
		t.Fatalf("turn 2 must not carry a kvx note: %q", note)
	}

	// Conversation B: same static prefix, new first turn — routed by prefix
	// affinity, and that is a new conversation, so kvxd is asked.
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "add a --verbose flag", 1))
	if n := kvx.count(); n != 2 {
		t.Fatalf("prefix-routed new conversation: kvxd called %d times, want 2", n)
	}
	note = kvxLastNote(t, s)
	if !strings.Contains(note, "(prefix)") || !strings.Contains(note, "kvx→restored") {
		t.Fatalf("conversation B: want prefix route + restore note, got %q", note)
	}
}

// A cloud provider never triggers a restore, even with the setting on.
func TestKVXRestoreSkipsNonLocalProvider(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	s, mux := kvxProxy(t, "cloud", kvxUpstream(t).URL, kvxSetting(kvx.URL, ""))
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times for a non-local provider", n)
	}
	if note := kvxLastNote(t, s); !strings.Contains(note, "(cold/inflight)") || strings.Contains(note, "kvx→") {
		t.Fatalf("note=%q: cold pool pick expected, no kvx note", note)
	}
}

// Nothing to restore (no system prompt, no tools) = no call.
func TestKVXRestoreSkipsWithoutPrefix(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	_, mux := kvxProxy(t, "fred", kvxUpstream(t).URL, kvxSetting(kvx.URL, ""))
	kvxSend(t, mux, `{"model":"ornith","messages":[{"role":"user","content":"hi"}]}`)
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times for a request with no system prompt or tools", n)
	}
}

// A slow kvxd costs at most timeout_ms and the request still goes upstream.
func TestKVXRestoreTimeoutDoesNotFailRequest(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	kvx.delay = 2 * time.Second
	s, mux := kvxProxy(t, "fred", kvxUpstream(t).URL, kvxSetting(kvx.URL, `,"timeout_ms":80`))
	start := time.Now()
	rec := kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if el := time.Since(start); el > time.Second {
		t.Fatalf("request took %s: the kvxd timeout did not bound the wait", el)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("upstream answer missing: %s", rec.Body.String())
	}
	if n := kvx.count(); n != 1 {
		t.Fatalf("kvxd called %d times, want 1", n)
	}
	if note := kvxLastNote(t, s); !strings.Contains(note, "kvx→timeout") {
		t.Fatalf("note=%q, want kvx→timeout", note)
	}
}

// restored:false is a miss note carrying kvxd's reason.
func TestKVXRestoreMissNote(t *testing.T) {
	kvx := newKVXStub(t, `{"restored":false,"reason":"no attachment matches"}`)
	s, mux := kvxProxy(t, "fred", kvxUpstream(t).URL, kvxSetting(kvx.URL, ""))
	kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if note := kvxLastNote(t, s); !strings.Contains(note, "kvx→miss: no attachment matches") {
		t.Fatalf("note=%q", note)
	}
}

// kvxd not running (connection refused) neither fails nor delays the request.
func TestKVXRestoreUnreachableDoesNotFailRequest(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	s, mux := kvxProxy(t, "fred", kvxUpstream(t).URL, kvxSetting(url, ""))
	start := time.Now()
	rec := kvxSend(t, mux, strings.Replace(kvxAgentBody, "%s", "fix the parser", 1))
	if el := time.Since(start); el > time.Second {
		t.Fatalf("request took %s against a dead kvxd", el)
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("upstream answer missing: %s", rec.Body.String())
	}
	if note := kvxLastNote(t, s); !strings.Contains(note, "kvx→error:") {
		t.Fatalf("note=%q, want kvx→error", note)
	}
}

func TestKVXRestoreConfigDefaults(t *testing.T) {
	var c KVXRestore
	if c.Enabled || c.baseURL() != kvxRestoreDefaultURL || c.timeout() != 3*time.Second || c.provider() != "fred" {
		t.Fatalf("zero config = %+v url=%s timeout=%s provider=%s", c, c.baseURL(), c.timeout(), c.provider())
	}
	c = KVXRestore{Enabled: true, URL: "http://kvx:1/", TimeoutMS: 250, Provider: "local"}
	if c.baseURL() != "http://kvx:1" || c.timeout() != 250*time.Millisecond || c.provider() != "local" {
		t.Fatalf("config = %+v url=%s timeout=%s provider=%s", c, c.baseURL(), c.timeout(), c.provider())
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 29601: "29,601", 1234567: "1,234,567"} {
		if got := commaInt(n); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", n, got, want)
		}
	}
}

// kvxUnpooledBody addresses a model that is in no pool, the way the live
// fleet mostly is (fred/tiel-kvx-w6800 etc.).
func kvxUnpooledBody(first string) string {
	return strings.Replace(strings.Replace(kvxAgentBody, `"model":"ornith"`, `"model":"fred/tiel-kvx-w6800"`, 1), "%s", first, 1)
}

// An UNPOOLED local model: a new conversation restores, its next turn does
// not, another new conversation restores again.
func TestKVXRestoreUnpooledNewConversation(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	up := kvxUpstream(t)
	s, mux := kvxProxyPools(t, "fred", up.URL, kvxSetting(kvx.URL, ""), "")

	kvxSend(t, mux, kvxUnpooledBody("fix the parser"))
	if n := kvx.count(); n != 1 {
		t.Fatalf("new unpooled conversation: kvxd called %d times, want 1", n)
	}
	note := kvxLastNote(t, s)
	if !strings.Contains(note, "kvx→restored 29,601 (slot 1, 0.8s)") || strings.Contains(note, "pool→") {
		t.Fatalf("note=%q: want the restore note and no pool note", note)
	}
	var sent struct {
		Model    string            `json:"model"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	json.Unmarshal(kvx.last(), &sent)
	if sent.Model != "tiel-kvx-w6800" || len(sent.Messages) != 2 || len(sent.Tools) != 1 {
		t.Fatalf("kvxd body: %s", kvx.last())
	}
	traces, _ := s.Traces(0, 1)
	if traces[0].Model != "tiel-kvx-w6800" || traces[0].Provider != "fred" {
		t.Fatalf("trace attributed to %s/%s", traces[0].Provider, traces[0].Model)
	}

	// Turn 2: same system + first user message, longer tail — bound, no call.
	turn2 := strings.Replace(kvxUnpooledBody("fix the parser"), `}],"tools"`, `},{"role":"assistant","content":"on it"},{"role":"user","content":"now run the tests"}],"tools"`, 1)
	kvxSend(t, mux, turn2)
	if n := kvx.count(); n != 1 {
		t.Fatalf("turn 2 of a bound unpooled conversation: kvxd called %d times, want still 1", n)
	}
	if note := kvxLastNote(t, s); strings.Contains(note, "kvx→") {
		t.Fatalf("turn 2 must not carry a kvx note: %q", note)
	}

	// A different conversation on the same model is new again.
	kvxSend(t, mux, kvxUnpooledBody("add a --verbose flag"))
	if n := kvx.count(); n != 2 {
		t.Fatalf("second unpooled conversation: kvxd called %d times, want 2", n)
	}
	if note := kvxLastNote(t, s); !strings.Contains(note, "kvx→restored") {
		t.Fatalf("note=%q", note)
	}
}

// Unpooled on a cloud provider: no call, and the affinity table is untouched.
func TestKVXRestoreUnpooledSkipsNonLocalAndDisabled(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	_, mux := kvxProxyPools(t, "cloud", kvxUpstream(t).URL, kvxSetting(kvx.URL, ""), "")
	body := strings.Replace(kvxUnpooledBody("fix the parser"), "fred/", "cloud/", 1)
	kvxSend(t, mux, body)
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times for an unpooled cloud model", n)
	}
	// Setting off: no call, and no binding is written (behaviour-neutral).
	_, mux = kvxProxyPools(t, "fred", kvxUpstream(t).URL, "", "")
	kvxSend(t, mux, kvxUnpooledBody("fix the parser"))
	if n := kvx.count(); n != 0 {
		t.Fatalf("kvxd called %d times with the setting unset", n)
	}
	if _, _, ok := poolAffinity.get("conv:"+conversationFingerprint(agentReq("You are a coding agent.\nWorking directory: /srv/app", "fix the parser")), poolAffinityTTL); ok {
		t.Fatal("a disabled hook must not write conversation bindings")
	}
}

// A conversation that moves to another unpooled model is new on that model.
func TestKVXUnpooledWhyIsPerModel(t *testing.T) {
	poolAffinity.reset()
	req := agentReq("sys", "hello")
	if w := kvxUnpooledWhy("a", req); w != "new" {
		t.Fatalf("first sighting = %q", w)
	}
	if w := kvxUnpooledWhy("a", req); w != "conversation" {
		t.Fatalf("second sighting = %q", w)
	}
	if w := kvxUnpooledWhy("b", req); w != "new" {
		t.Fatalf("same conversation on another model = %q", w)
	}
	if w := kvxUnpooledWhy("a", &wire.Request{}); w != "" {
		t.Fatalf("unfingerprintable request = %q", w)
	}
}

// The template-affecting fields cfrproxy forwards (reasoning_effort et al.)
// reach kvxd verbatim; generation params do not.
func TestKVXRestoreForwardsTemplateFieldsOnly(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	up := kvxUpstream(t)
	s, mux := kvxProxyPools(t, "fred", up.URL, kvxSetting(kvx.URL, ""), "")

	body := strings.Replace(kvxUnpooledBody("fix the parser"), `"model":"fred/tiel-kvx-w6800",`,
		`"model":"fred/tiel-kvx-w6800","reasoning_effort":"medium","chat_template_kwargs":{"enable_thinking":true},"max_tokens":64,"temperature":0.2,"stream":false,`, 1)
	kvxSend(t, mux, body)
	if n := kvx.count(); n != 1 {
		t.Fatalf("kvxd called %d times, want 1", n)
	}
	if note := kvxLastNote(t, s); !strings.Contains(note, "kvx→restored") {
		t.Fatalf("note=%q", note)
	}
	var fwd, got map[string]json.RawMessage
	if err := json.Unmarshal(up.last(), &fwd); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(kvx.last(), &got); err != nil {
		t.Fatal(err)
	}
	if string(fwd["reasoning_effort"]) != `"medium"` {
		t.Fatalf("upstream did not receive reasoning_effort=medium: %s", up.last())
	}
	for _, k := range []string{"model", "messages", "tools", "reasoning_effort", "chat_template_kwargs"} {
		if string(got[k]) != string(fwd[k]) {
			t.Errorf("%s: kvxd got %s, upstream got %s", k, got[k], fwd[k])
		}
	}
	for _, k := range []string{"max_tokens", "temperature", "stream"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not be forwarded to kvxd: %s", k, kvx.last())
		}
	}
}

func TestKVXRestoreBodyShape(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no messages", `{"model":"m","max_tokens":1}`, ""},
		{"messages only", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":8}`,
			`{"messages":[{"role":"user","content":"hi"}],"model":"tiel"}`},
		{"null tools dropped, thinking kept", `{"messages":[],"tools":null,"thinking":{"type":"enabled"},"enable_thinking":false,"reasoning_format":"none","stream":true}`,
			`{"enable_thinking":false,"messages":[],"model":"tiel","reasoning_format":"none","thinking":{"type":"enabled"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kvxRestoreBody("tiel", []byte(c.in))
			if string(got) != c.want {
				t.Fatalf("got %s\nwant %s", got, c.want)
			}
		})
	}
}

// A restored slot must be the slot the request is served in: llama.cpp would
// otherwise seat it by its own similarity/LRU rule and could prefill cold in the
// other slot right next to a warm restore. A miss pins nothing.
func TestKVXRestorePinsIDSlotOnForwardedBody(t *testing.T) {
	kvx := newKVXStub(t, kvxHit)
	up := kvxUpstream(t)
	_, mux := kvxProxyPools(t, "fred", up.URL, kvxSetting(kvx.URL, ""), "")
	kvxSend(t, mux, kvxUnpooledBody("fix the parser"))
	var fwd map[string]json.RawMessage
	if err := json.Unmarshal(up.last(), &fwd); err != nil {
		t.Fatal(err)
	}
	if string(fwd["id_slot"]) != "1" {
		t.Fatalf("forwarded body should carry id_slot=1 after a restore into slot 1, got %s", up.last())
	}
	miss := newKVXStub(t, `{"restored":false,"reason":"no attachment shares at least 1024 tokens"}`)
	up2 := kvxUpstream(t)
	_, mux2 := kvxProxyPools(t, "fred", up2.URL, kvxSetting(miss.URL, ""), "")
	kvxSend(t, mux2, kvxUnpooledBody("fix the parser"))
	if strings.Contains(string(up2.last()), "id_slot") {
		t.Fatalf("a miss must not pin a slot: %s", up2.last())
	}
}

// A miss on a new conversation must leave a pinned artifact behind: once the
// response has finished, the static head (leading system messages + tools,
// one-token user turn) is posted to /v1/seed — once per head, not per turn.
func TestKVXAutoSeedAfterMiss(t *testing.T) {
	var mu sync.Mutex
	var seeds [][]byte
	kvx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kvxRestorePath:
			w.Write([]byte(`{"restored":false,"reason":"no attachment shares at least 1024 tokens"}`))
		case kvxSeedPath:
			mu.Lock()
			seeds = append(seeds, b)
			mu.Unlock()
			w.Write([]byte(`{"ok":true,"seeded":true,"tokens":9000,"slot":1}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(kvx.Close)
	autoSeedDebounce.mu.Lock()
	autoSeedDebounce.m = map[string]time.Time{}
	autoSeedDebounce.mu.Unlock()
	up := kvxUpstream(t)
	// the fixture's system prompt is short; lower the floor so it qualifies
	_, mux := kvxProxyPools(t, "fred", up.URL, kvxSetting(kvx.URL, `,"auto_seed_min_tokens":1`), "")
	body := kvxUnpooledBody("fix the parser")
	kvxSend(t, mux, body)
	kvxSend(t, mux, strings.Replace(body, "fix the parser", "fix the lexer", 1)) // same head, new conversation
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(seeds)
		mu.Unlock()
		if n >= 1 && time.Now().After(deadline.Add(-2500*time.Millisecond)) || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seeds) != 1 {
		t.Fatalf("want exactly one seed for one head across two conversations, got %d", len(seeds))
	}
	var m map[string]json.RawMessage
	json.Unmarshal(seeds[0], &m)
	var msgs []map[string]any
	json.Unmarshal(m["messages"], &msgs)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" || msgs[1]["content"] != "seed" {
		t.Fatalf("seed must carry the system head + a one-token user turn: %s", m["messages"])
	}
	if string(m["user_turn"]) != `"seed"` || string(m["model"]) != `"tiel-kvx-w6800"` {
		t.Fatalf("seed body: %s", seeds[0])
	}
	// off switch
	autoSeedDebounce.mu.Lock()
	autoSeedDebounce.m = map[string]time.Time{}
	autoSeedDebounce.mu.Unlock()
	_, mux2 := kvxProxyPools(t, "fred", up.URL, kvxSetting(kvx.URL, `,"auto_seed":false,"auto_seed_min_tokens":1`), "")
	kvxSend(t, mux2, body)
	time.Sleep(300 * time.Millisecond)
	if len(seeds) != 1 {
		t.Fatalf("auto_seed:false must not seed, got %d", len(seeds))
	}
}
