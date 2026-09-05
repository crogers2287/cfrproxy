package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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

// fakeSwap imitates llama-swap: a listing with per-model meta, /running, and
// /upstream/<m>/slots. Mutable so a test can flip warm/busy between calls.
type fakeSwap struct {
	mu      sync.Mutex
	listing []string
	ctx     map[string]int
	vision  map[string]bool
	running map[string]bool
	busy    map[string]int
	slots   map[string]int
	answer  string // classifier reply
	srv     *httptest.Server
}

func newFakeSwap(t *testing.T) *fakeSwap {
	t.Helper()
	f := &fakeSwap{ctx: map[string]int{}, vision: map[string]bool{}, running: map[string]bool{},
		busy: map[string]int{}, slots: map[string]int{}, answer: "routine"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/running":
			var run []map[string]string
			for m, ok := range f.running {
				if ok {
					run = append(run, map[string]string{"model": m, "state": "ready"})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"running": run})
		case strings.HasPrefix(r.URL.Path, "/upstream/") && strings.HasSuffix(r.URL.Path, "/slots"):
			m := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upstream/"), "/slots")
			var out []map[string]bool
			for i := 0; i < f.slots[m]; i++ {
				out = append(out, map[string]bool{"is_processing": i < f.busy[m]})
			}
			json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/models"):
			var data []map[string]any
			for _, m := range f.listing {
				meta := map[string]any{}
				if c, ok := f.ctx[m]; ok {
					meta["context"] = fmt.Sprint(c)
				}
				if v, ok := f.vision[m]; ok {
					meta["isVision"] = v
				}
				data = append(data, map[string]any{"id": m, "meta": map[string]any{"llamaswap": meta}})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case r.Method == "POST":
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{}}`, f.answer)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fakeCloud lists models and knows nothing about /running.
func fakeCloud(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			var data []map[string]any
			for _, m := range models {
				data = append(data, map[string]any{"id": m})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func smartFixture(t *testing.T) (*Proxy, *store.Store, *fakeSwap) {
	t.Helper()
	t.Setenv("CFRPROXY_ROUTE_LOG", "off")
	routeCache.reset()
	f := newFakeSwap(t)
	f.listing = []string{"tiel-a", "tiel-b", "big"}
	f.ctx["tiel-a"], f.ctx["tiel-b"], f.ctx["big"] = 262144, 262144, 98304
	f.vision["tiel-a"], f.vision["tiel-b"], f.vision["big"] = false, true, true
	f.running["tiel-a"] = true
	f.slots["tiel-a"], f.slots["tiel-b"] = 2, 2
	cloud := fakeCloud(t, "terra", "fable")
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: f.srv.URL, DefaultModel: "tiel-a", Priority: 10, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "cloud", Type: "openai", BaseURL: cloud.URL, DefaultModel: "terra", Priority: 20, Enabled: true})
	s.SetSetting("auto_router", `{"enabled":true,"classifier":"local/tiel-a","routes":{"default":"cloud/terra"},
	  "smart":{"enabled":true,"local_max_tokens":100000,
	    "tiers":{"routine":["local/tiel*","local/big","cloud/terra"],
	             "careful":["local/big","cloud/terra"],
	             "hard":["cloud/fable","cloud/terra"]}}}`)
	return New(s), s, f
}

// tierOf strips the " conv:<id>" tag the smart router appends to its bucket.
func tierOf(note string) string {
	if i := strings.Index(note, " conv:"); i >= 0 {
		return note[:i]
	}
	return note
}

func smallReq(text string) *wire.Request {
	return &wire.Request{Messages: []wire.Msg{{Role: "user", Content: text}}}
}

func findCand(d smartDecision, model string) RouteCandidate {
	for _, c := range d.Candidates {
		if c.Model == model {
			return c
		}
	}
	return RouteCandidate{Verdict: "absent"}
}

func TestSmartRouteLocalFirstWarm(t *testing.T) {
	p, _, _ := smartFixture(t)
	m, note := p.AutoRoute(context.Background(), smallReq("fix the typo in main.go"))
	if m != "local/tiel-a" || tierOf(note) != "routine" || !strings.Contains(note, " conv:") {
		t.Fatalf("want warm local tiel-a on routine, got %s %q", m, note)
	}
	// second turn of the same conversation is pinned and skips the classifier
	m, note = p.AutoRoute(context.Background(), smallReq("fix the typo in main.go"))
	if m != "local/tiel-a" || tierOf(note) != "routine·sticky" {
		t.Fatalf("want sticky pin, got %s %q", m, note)
	}
}

func TestSmartRouteColdIsLastResort(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.running["tiel-a"] = false
	f.mu.Unlock()
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 100}, "")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("every local model is cold → cloud; got %s\n%+v", d.Chosen, d.Candidates)
	}
	if c := findCand(d, "tiel-a"); c.Verdict != "cold" || !c.Local || c.Warm != "cold" {
		t.Fatalf("tiel-a should read cold: %+v", c)
	}
	if c := findCand(d, "terra"); c.Local || c.Warm != "n/a" {
		t.Fatalf("cloud provider must not be local: %+v", c)
	}
}

func TestSmartRouteBusyYieldsToFreeSibling(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.running["tiel-b"] = true
	f.busy["tiel-a"] = 2 // both slots processing
	f.mu.Unlock()
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 100}, "")
	if d.Chosen != "local/tiel-b" {
		t.Fatalf("busy tiel-a should yield to free tiel-b, got %s\n%+v", d.Chosen, d.Candidates)
	}
	if c := findCand(d, "tiel-a"); c.Verdict != "busy" || c.Busy != 2 || c.Slots != 2 {
		t.Fatalf("tiel-a facts: %+v", c)
	}
}

func TestSmartRouteContextEscalatesToCloud(t *testing.T) {
	p, _, _ := smartFixture(t)
	cfg := p.AutoRouterConfig().Smart
	cfg.MaxColdPrefillSeconds = -1 // this test is about the window, not the prefill budget
	// bigger than local_max_tokens but inside the model window
	d := p.smartSelect(context.Background(), cfg, RouteProfile{Tier: tierRoutine, Tokens: 120_000}, "")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("120k tokens must leave the local models, got %s\n%+v", d.Chosen, d.Candidates)
	}
	if c := findCand(d, "tiel-a"); !strings.Contains(c.Verdict, "local_max_tokens") {
		t.Fatalf("want local_max_tokens verdict, got %+v", c)
	}
	// bigger than big's 98k window but under local_max_tokens: big is too small, tiel-a fits
	d = p.smartSelect(context.Background(), cfg, RouteProfile{Tier: tierRoutine, Tokens: 95_000}, "")
	if d.Chosen != "local/tiel-a" {
		t.Fatalf("95k fits tiel-a (262k), got %s", d.Chosen)
	}
	if c := findCand(d, "big"); !strings.HasPrefix(c.Verdict, "too small") {
		t.Fatalf("big (98k) should be too small for 95k+headroom: %+v", c)
	}
}

func TestSmartRouteImageNeedsSightedModel(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.running["big"] = true
	f.mu.Unlock()
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 100, Image: true}, "")
	if d.Chosen != "local/big" {
		t.Fatalf("tiel-a is blind (isVision:false), tiel-b cold → big; got %s\n%+v", d.Chosen, d.Candidates)
	}
	if c := findCand(d, "tiel-a"); c.Verdict != "blind" {
		t.Fatalf("tiel-a verdict: %+v", c)
	}
}

func TestSmartRouteUnhealthyIsLastResort(t *testing.T) {
	p, s, _ := smartFixture(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 30; i++ {
		st := 500
		if i%3 == 0 {
			st = 200
		}
		s.AddTrace(&store.Trace{TS: now, Provider: "local", Model: "tiel-a", Status: st})
	}
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 100}, "")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("tiel-a fails 2/3 → skip to cloud; got %s\n%+v", d.Chosen, d.Candidates)
	}
	if c := findCand(d, "tiel-a"); c.Verdict != "unhealthy" || c.Requests != 30 || c.Failed != 20 {
		t.Fatalf("tiel-a health: %+v", c)
	}
}

func TestSmartRouteStickyRevalidatesGates(t *testing.T) {
	p, _, _ := smartFixture(t)
	req := &wire.Request{System: "you are omp", Messages: []wire.Msg{{Role: "user", Content: "start"}}}
	if m, _ := p.AutoRoute(context.Background(), req); m != "local/tiel-a" {
		t.Fatalf("turn 1: %s", m)
	}
	// same conversation head, but it has grown past the local window
	req.Messages = append(req.Messages, wire.Msg{Role: "assistant", Content: strings.Repeat("x", 500_000)},
		wire.Msg{Role: "user", Content: "continue"})
	m, note := p.AutoRoute(context.Background(), req)
	if m != "cloud/terra" || strings.Contains(note, "sticky") {
		t.Fatalf("pinned model no longer fits → re-route, got %s %q", m, note)
	}
	// and the new pin is the cloud model
	if m, note := p.AutoRoute(context.Background(), req); m != "cloud/terra" || tierOf(note) != "routine·sticky" {
		t.Fatalf("turn 3 should stick to cloud, got %s %q", m, note)
	}
}

func TestSmartClassifierPicksTier(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.answer = "Hard"
	f.mu.Unlock()
	m, note := p.AutoRoute(context.Background(), smallReq("hello"))
	if m != "cloud/fable" || tierOf(note) != "hard" {
		t.Fatalf("classifier said hard → cloud/fable, got %s %q", m, note)
	}
}

func TestSmartHeuristicTier(t *testing.T) {
	p, s, _ := smartFixture(t)
	var cfg AutoRouterConfig
	json.Unmarshal([]byte(s.Setting("auto_router")), &cfg)
	cfg.Smart.Classify = "heuristic"
	b, _ := json.Marshal(cfg)
	s.SetSetting("auto_router", string(b))
	cases := []struct {
		req  *wire.Request
		want string
	}{
		{smallReq("rename this variable"), tierRoutine},
		{smallReq("please refactor the auth module"), tierHard},
		{&wire.Request{Tools: []wire.Tool{{Name: "bash"}}, Messages: func() []wire.Msg {
			var m []wire.Msg
			for i := 0; i < 10; i++ {
				m = append(m, wire.Msg{Role: "user", Content: "go on"})
			}
			return m
		}()}, tierCareful},
	}
	for _, c := range cases {
		pr := profileFacts(c.req)
		if got := heuristicTier(c.req, pr); got != c.want {
			t.Errorf("heuristicTier(%q...) = %s want %s", lastUserText(c.req, 30), got, c.want)
		}
	}
	// the request path honours classify:"heuristic" — the fake classifier says routine, heuristic says hard
	m, note := p.AutoRoute(context.Background(), smallReq("security audit of the login flow"))
	if tierOf(note) != "hard" || m != "cloud/fable" {
		t.Fatalf("heuristic hard → cloud/fable, got %s %q", m, note)
	}
}

func TestSmartTierFallsThroughToConfiguredList(t *testing.T) {
	cfg := &SmartRouterConfig{Enabled: true, Tiers: map[string][]string{"careful": {"x/y"}}}
	if tier, l := cfg.tierList(tierRoutine); tier != tierCareful || len(l) != 1 {
		t.Fatalf("routine with no list → careful, got %s %v", tier, l)
	}
	if tier, _ := cfg.tierList(tierHard); tier != tierCareful {
		t.Fatalf("hard with no list → careful (only list), got %s", tier)
	}
}

func TestSmartExplainShowsVerdicts(t *testing.T) {
	p, _, _ := smartFixture(t)
	res := p.Explain(context.Background(), ExplainRequest{Model: "auto", Tier: "routine", Tokens: 120000, Tools: 3})
	if res.Status != 200 || res.Resolved != "cloud/terra" {
		t.Fatalf("explain: %d %s %s", res.Status, res.Resolved, res.Error)
	}
	txt := res.Text()
	for _, want := range []string{"profile:", "routine (override", "local_max_tokens", "chosen", "local/tiel-a", "cloud/terra", "ctx 262144"} {
		if !strings.Contains(txt, want) {
			t.Errorf("explain text missing %q:\n%s", want, txt)
		}
	}
	// classic (non-smart) explain still lists buckets
	p2, s2 := explainFixture(t)
	s2.SetSetting("auto_router", `{"enabled":true,"classifier":"a/am","routes":{"code":"a/am","default":"b/bm"}}`)
	if txt := p2.Explain(context.Background(), ExplainRequest{Model: "auto"}).Text(); !strings.Contains(txt, "bucket code") {
		t.Fatalf("classic explain regressed:\n%s", txt)
	}
}

func TestSmartRouteFallsBackToDefaultRoute(t *testing.T) {
	p, s, _ := smartFixture(t)
	s.SetSetting("auto_router", `{"enabled":true,"routes":{"default":"cloud/terra"},
	  "smart":{"enabled":true,"tiers":{"routine":["nope/x","local/does-not-exist"]}}}`)
	m, note := p.AutoRoute(context.Background(), smallReq("hi"))
	if m != "cloud/terra" || note != "default" {
		t.Fatalf("nothing qualifies → routes.default, got %s %q", m, note)
	}
}

// A capped account fails every model it is asked for; a tier entry naming a
// model that account has not served lately must inherit that verdict.
func TestSmartRouteAccountWideHealth(t *testing.T) {
	p, s, _ := smartFixture(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 40; i++ {
		s.AddTrace(&store.Trace{TS: now, Provider: "cloud", Model: "fable", Status: 400, Err: "usage cap"})
	}
	// tier names cloud/terra, which has no rows of its own
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 120_000}, "")
	c := findCand(d, "terra")
	if c.Verdict != "chosen (unhealthy, nothing better)" || c.HealthFrom != "provider" || c.Requests != 40 || c.Failed != 40 {
		t.Fatalf("terra should inherit the account's cap failures: %+v", c)
	}
	// a model with enough clean traffic of its own is judged on that alone
	for i := 0; i < 25; i++ {
		s.AddTrace(&store.Trace{TS: now, Provider: "cloud", Model: "terra", Status: 200})
	}
	p.health.at = time.Time{} // drop the 60s cache
	d = p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, RouteProfile{Tier: tierRoutine, Tokens: 120_000}, "")
	if c := findCand(d, "terra"); c.Verdict != "chosen" || c.HealthFrom != "" || c.Requests != 25 {
		t.Fatalf("terra's own clean record should win: %+v", c)
	}
}

// The /p/cfrproxy-auto mount is how a harness picker (Hermes/Telegram) gets to
// see the routers at all: it lists them as bare ids and routes them unscoped.
func TestAutoMountListsAndRoutesRouters(t *testing.T) {
	p, s, _ := smartFixture(t)
	s.SaveRouter(&store.Router{Name: "budget", Enabled: true, Classifier: "local/tiel-a", Planner: "local/tiel-a", Routes: []byte(`{"default":"cloud/terra"}`)})
	ids := p.scopedModelIDs(context.Background(), "cfrproxy-auto", false)
	want := []string{"auto", "auto:budget", "auto-plan:budget"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("auto mount listing = %v want %v", ids, want)
	}
	mux := http.NewServeMux()
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/p/cfrproxy-auto/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Data) != 3 || out.Data[1].ID != "auto:budget" {
		t.Fatalf("scoped listing over HTTP: %+v", out.Data)
	}
	// and a request through the mount reaches the named router: classic
	// buckets, so "default" → cloud/terra (the fake cloud 404s the completion,
	// which is fine — we only need the trace to show where it was routed)
	body := strings.NewReader(`{"model":"auto:budget","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`)
	r2, err := http.Post(srv.URL+"/p/cfrproxy-auto/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || traces[0].Provider != "cloud" || traces[0].Model != "terra" || !strings.Contains(traces[0].Note, "auto→default→cloud/terra") {
		t.Fatalf("auto:budget via the mount should route to cloud/terra; HTTP %d %s; traces=%+v", r2.StatusCode, rb, traces)
	}
}

// A new conversation with a big prompt and no cached prefix on the local
// instance must not sit through a minute of silent prefill when a cloud
// candidate is viable — but the same prompt from a harness whose prefix that
// instance already served is fine, and so is any conversation already pinned.
func TestSmartRouteColdPrefillBudget(t *testing.T) {
	p, _, _ := smartFixture(t)
	poolAffinity.reset()
	cfg := p.AutoRouterConfig().Smart
	req := &wire.Request{System: strings.Repeat("claude code system prompt ", 100), Tools: []wire.Tool{{Name: "bash"}},
		Messages: []wire.Msg{{Role: "user", Content: "hey"}}}
	pr := profileFacts(req)
	pr.Tokens, pr.Tier = 67_000, tierRoutine // 67k at the 1000 tok/s default = 67 s > 30 s
	d := p.smartSelect(context.Background(), cfg, pr, "")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("67k cold on tiel-a should escalate, got %s\n%+v", d.Chosen, d.Candidates)
	}
	c := findCand(d, "tiel-a")
	if !strings.HasPrefix(c.Verdict, "cold prefill ~67s") || c.ColdPrefillS < 60 || c.PrefixKnown {
		t.Fatalf("tiel-a verdict: %+v", c)
	}
	// a faster measured rate changes the estimate
	notePrefillRate("tiel-a", 5000)
	d = p.smartSelect(context.Background(), cfg, pr, "")
	if d.Chosen != "local/tiel-a" || findCand(d, "tiel-a").ColdPrefillS > 20 {
		t.Fatalf("at 5000 tok/s 67k is 13 s and fits: %s %+v", d.Chosen, findCand(d, "tiel-a"))
	}
	notePrefillRate("tiel-a", 1) // and back to hopeless
	notePrefillRate("tiel-a", 1)
	notePrefillRate("tiel-a", 1)
	notePrefillRate("tiel-a", 1)
	notePrefillRate("tiel-a", 1)
	// the prefix is known on tiel-a → viable regardless of size
	poolAffinity.put(poolPrefixKey(req), "tiel-a", "test")
	d = p.smartSelect(context.Background(), cfg, pr, "")
	if d.Chosen != "local/tiel-a" || !findCand(d, "tiel-a").PrefixKnown {
		t.Fatalf("known prefix should be judged cached: %s %+v", d.Chosen, findCand(d, "tiel-a"))
	}
	// budget off → always viable
	poolAffinity.reset()
	cfg.MaxColdPrefillSeconds = -1
	if d := p.smartSelect(context.Background(), cfg, pr, ""); d.Chosen != "local/tiel-a" {
		t.Fatalf("budget off: %s", d.Chosen)
	}
	// through the request path the winner's prefix is remembered
	cfg.MaxColdPrefillSeconds = 0
	poolAffinity.reset()
	prefillRates.mu.Lock()
	delete(prefillRates.m, "tiel-a")
	prefillRates.mu.Unlock()
	small := &wire.Request{System: "omp", Messages: []wire.Msg{{Role: "user", Content: "small"}}}
	if m, _ := p.AutoRoute(context.Background(), small); m != "local/tiel-a" {
		t.Fatalf("small new conversation: %s", m)
	}
	if !prefixKnownOn(small, []string{"tiel-a"}) {
		t.Fatal("winner's static prefix should be bound after routing")
	}
}

// Seeding renders the body the proxy would forward (OpenAI dialect, provider
// thinking default, one-token user turn) and posts it to kvxd's /v1/seed; the
// dry-run probe uses the same render and turns a "cold prefill" verdict into
// "prefix cached" when an artifact covers the prompt.
func TestKVXSeedAndDryRunProbe(t *testing.T) {
	var seedBodies, probeBodies [][]byte
	var mu sync.Mutex
	kvx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/seed":
			seedBodies = append(seedBodies, b)
			fmt.Fprint(w, `{"ok":true,"seeded":true,"tokens":7613,"slot":1,"admitted":"abc","stages":{"prefill":1.1},"seconds":7.3}`)
		case "/v1/restore-for-prompt":
			probeBodies = append(probeBodies, b)
			fmt.Fprint(w, `{"ok":true,"restored":false,"would_restore":true,"covers_tokens":60000,"shared_tokens":60000}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(kvx.Close)
	p, s, _ := smartFixture(t)
	poolAffinity.reset()
	prov, _ := s.ProviderByName("local")
	prov.ReasoningEffort = "medium"
	s.SaveProvider(&prov)
	s.SetSetting("kvx_restore", `{"enabled":true,"url":"`+kvx.URL+`","provider":"local"}`)

	// seed from a recorded prefix (with the old glued billing header, which must go)
	sp := SeedPrefix{Client: "claude-code", Fingerprint: "deadbeefcafe0000", system: billingHead.ReplaceAllString(
		"x-anthropic-billing-header: cc_version=2.1.259.467; cc_entrypoint=cli;You are Claude Code.", ""),
		tools: []wire.Tool{{Name: "bash", Description: "run", Params: json.RawMessage(`{"type":"object"}`)}}}
	if sp.system != "You are Claude Code." {
		t.Fatalf("billing head not stripped: %q", sp.system)
	}
	res, err := p.KVXSeed(context.Background(), "local/tiel-a", []SeedPrefix{sp}, "")
	if err != nil || len(res) != 1 || !res[0].Seeded || res[0].Tokens != 7613 || res[0].Slot != 1 {
		t.Fatalf("seed: %v %+v", err, res)
	}
	var body map[string]json.RawMessage
	json.Unmarshal(seedBodies[0], &body)
	if string(body["model"]) != `"tiel-a"` || string(body["reasoning_effort"]) != `"medium"` || string(body["user_turn"]) != `"seed"` {
		t.Fatalf("seed body: %s", seedBodies[0])
	}
	var msgs []map[string]any
	json.Unmarshal(body["messages"], &msgs)
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[0]["content"] != "You are Claude Code." || msgs[1]["content"] != "seed" {
		t.Fatalf("seed messages: %s", body["messages"])
	}
	if !strings.Contains(string(body["tools"]), `"bash"`) {
		t.Fatalf("tools missing: %s", body["tools"])
	}
	for _, k := range []string{"max_tokens", "stream", "temperature"} {
		if _, ok := body[k]; ok {
			t.Fatalf("generation param %s must not reach kvxd", k)
		}
	}

	// the probe: 67k new conversation, naive estimate 67 s → over budget → ask kvxd
	req := &wire.Request{System: "You are Claude Code.", Tools: sp.tools, Messages: []wire.Msg{{Role: "user", Content: "hey"}}}
	pr := profileFacts(req)
	pr.Tokens, pr.Tier = 67_000, tierRoutine
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, pr, "")
	c := findCand(d, "tiel-a")
	if d.Chosen != "local/tiel-a" || !c.PrefixKnown || c.KVXShared != 60000 || c.ColdPrefillS > 10 {
		t.Fatalf("kvx says 60k covered → local should win: %s %+v", d.Chosen, c)
	}
	if len(probeBodies) == 0 || !strings.Contains(string(probeBodies[0]), `"dry_run":true`) {
		t.Fatalf("probe must be a dry run: %d %s", len(probeBodies), probeBodies)
	}
	// a conversation pinned to a cloud model re-validates only that model: no probes
	mu.Lock()
	probeBodies = nil
	mu.Unlock()
	d = p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, pr, "cloud/terra")
	if d.Chosen != "cloud/terra" || len(d.Candidates) != 1 || d.Candidates[0].Verdict != "chosen (pinned)" {
		t.Fatalf("pinned fast path: %s %+v", d.Chosen, d.Candidates)
	}
	mu.Lock()
	np := len(probeBodies)
	mu.Unlock()
	if np != 0 {
		t.Fatalf("a pinned cloud conversation must not probe local candidates, got %d probes", np)
	}
	// a pinned local model whose window no longer fits is re-routed (and never probed)
	pr.Tokens = 300_000
	d = p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, pr, "local/tiel-a")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("outgrown pin should re-route: %s", d.Chosen)
	}
	mu.Lock()
	np = len(probeBodies)
	mu.Unlock()
	if np != 0 {
		t.Fatalf("no probe for a model the prompt cannot fit, got %d", np)
	}
	// a pool is refused as a seed target with a helpful message
	s.SetSetting("model_pools", `{"tp":["tiel-a","tiel-b"],"tiel-a":["tiel-a","tiel-b"]}`)
	if _, err := p.KVXSeed(context.Background(), "local/tp", []SeedPrefix{sp}, ""); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("pool seed should be refused: %v", err)
	}
	// a member that is also a pool key (fred's ornith-kvx-w6800) is a runtime and seeds fine
	if _, err := p.KVXSeed(context.Background(), "local/tiel-a", []SeedPrefix{sp}, ""); err != nil {
		t.Fatalf("member-as-pool-key should seed: %v", err)
	}
}

func TestRouteTrajectoriesGroupByConversation(t *testing.T) {
	p, s, _ := smartFixture(t)
	now := time.Now().UnixMilli()
	add := func(off int64, model, note string, prompt, cached int, status int) {
		s.AddTrace(&store.Trace{TS: now + off, Provider: "local", Model: model, Inbound: "anthropic", Status: status,
			LatencyMS: 1000 + off, PromptTokens: prompt, CachedTokens: cached, Note: note})
	}
	add(0, "tiel-a", "auto→routine→local/tiel-a pool→x kvx→restored 60,000 (slot 1, 2.7s) conv:aaaa1111", 67000, 60000, 200)
	add(1, "tiel-a", "auto→routine·sticky→local/tiel-a conv:aaaa1111", 67100, 67000, 200)
	add(2, "terra", "auto→routine→cloud/terra conv:aaaa1111", 130000, 0, 200) // outgrew local
	add(3, "tiel-a", "auto→careful→local/tiel-a conv:bbbb2222", 8000, 0, 500)
	add(4, "tiel-a", "pool→tiel-a (conversation)", 8000, 7000, 200) // not auto-routed: ignored
	ts, err := p.RouteTrajectories(100, 10)
	if err != nil || len(ts) != 2 {
		t.Fatalf("%v %+v", err, ts)
	}
	b, a := ts[0], ts[1] // newest activity first: bbbb2222 (off 3) then aaaa1111 (off 2)
	if b.Conv != "bbbb2222" || b.Errors != 1 || b.Tier != "careful" || a.Conv != "aaaa1111" {
		t.Fatalf("order/errors: %+v %+v", b, a)
	}
	if a.Turns != 3 || a.Escalations != 1 || len(a.Hops) != 2 || a.Hops[0].Model != "local/tiel-a" || a.Hops[0].Turns != 2 || a.Hops[1].Model != "cloud/terra" {
		t.Fatalf("hops: %+v", a)
	}
	if a.KVX != "kvx→restored" || a.CacheHitPct < 45 || a.CacheHitPct > 50 {
		t.Fatalf("kvx/cache: %+v", a)
	}
	if txt := TrajectoriesText(ts); !strings.Contains(txt, "local/tiel-a×2 → cloud/terra") {
		t.Fatalf("text:\n%s", txt)
	}
}

// Hooks and search sub-requests are policy-routed to routine regardless of
// what the classifier would say, and a stale pin under another tier is dropped.
// A big new conversation that goes to cloud only because local was over the
// cold-prefill budget gets its head seeded on the warm local runtime anyway.
func TestSmartRouteSeedsColdLosers(t *testing.T) {
	var mu sync.Mutex
	var seeds []string
	kvx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case kvxSeedPath:
			mu.Lock()
			seeds = append(seeds, string(b))
			mu.Unlock()
			fmt.Fprint(w, `{"ok":true,"seeded":true,"tokens":60000,"slot":1}`)
		default: // dry-run probe: nothing held
			fmt.Fprint(w, `{"ok":true,"restored":false,"would_restore":false}`)
		}
	}))
	t.Cleanup(kvx.Close)
	p, s, _ := smartFixture(t)
	poolAffinity.reset()
	autoSeedDebounce.mu.Lock()
	autoSeedDebounce.m = map[string]time.Time{}
	autoSeedDebounce.mu.Unlock()
	s.SetSetting("kvx_restore", `{"enabled":true,"url":"`+kvx.URL+`","provider":"local","auto_seed_min_tokens":1}`)
	req := &wire.Request{System: strings.Repeat("You are a security monitor. ", 2000), Tools: []wire.Tool{{Name: "bash"}},
		Messages: []wire.Msg{{Role: "user", Content: "review"}}}
	pr := profileFacts(req)
	pr.Tokens, pr.Tier = 67_000, tierRoutine
	d := p.smartSelect(context.Background(), p.AutoRouterConfig().Smart, pr, "")
	if d.Chosen != "cloud/terra" {
		t.Fatalf("expected cloud on cold prefill, got %s", d.Chosen)
	}
	p.seedColdLosers(req, d)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seeds)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seeds) != 1 || !strings.Contains(seeds[0], `"model":"tiel-a"`) || !strings.Contains(seeds[0], `"user_turn":"seed"`) {
		t.Fatalf("want one seed on the warm local loser tiel-a, got %d: %.200s", len(seeds), strings.Join(seeds, "|"))
	}
}

func TestSmartRouteRoleTierOverride(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.answer = "hard" // the classifier would send this to cloud/fable
	f.mu.Unlock()
	hook := &wire.Request{System: "You are a security monitor for autonomous AI coding agents. Review the transcript.",
		Messages: []wire.Msg{{Role: "user", Content: "review this"}}}
	m, note := p.AutoRoute(context.Background(), hook)
	if m != "local/tiel-a" || tierOf(note) != "routine" {
		t.Fatalf("hook must be routine → local, got %s %q", m, note)
	}
	// a pin left over from a "hard" grade is replaced on the next turn
	routeCache.put(conversationFingerprint(hook), "cloud/fable", tierHard)
	m, note = p.AutoRoute(context.Background(), hook)
	if m != "local/tiel-a" || strings.Contains(note, "sticky") {
		t.Fatalf("stale hard pin should be dropped for a hook, got %s %q", m, note)
	}
	// the main agent is still graded by the classifier
	main := &wire.Request{System: "You are Claude Code, Anthropic's official CLI for Claude.", Messages: []wire.Msg{{Role: "user", Content: "hi"}}}
	if m, note := p.AutoRoute(context.Background(), main); m != "cloud/fable" || tierOf(note) != "hard" {
		t.Fatalf("main agent should follow the classifier: %s %q", m, note)
	}
}

// A fork sub-agent carries the main agent's system prompt; it is told apart
// by starting a new conversation while the main one is still active. After
// the main goes quiet, a new conversation is the main agent again (compaction).
func TestSessionRoleDetectsForks(t *testing.T) {
	sessionMains.mu.Lock()
	sessionMains.m = map[string]sessionMain{}
	sessionMains.mu.Unlock()
	mk := func(first string) *wire.Request {
		return &wire.Request{SessionID: "sess-1", System: "You are Claude Code, Anthropic's official CLI for Claude.",
			Messages: []wire.Msg{{Role: "user", Content: first}}}
	}
	main := mk("<system-reminder>context</system-reminder>\nbuild M0")
	fork := mk("<system-reminder>context</system-reminder>\nyou are a fork: explore the repo")
	if r := sessionRole(main, "main"); r != "main" {
		t.Fatalf("first conversation is main, got %s", r)
	}
	if r := sessionRole(fork, "main"); r != "sub" {
		t.Fatalf("concurrent new conversation is a fork, got %s", r)
	}
	if r := sessionRole(main, "main"); r != "main" {
		t.Fatalf("main stays main, got %s", r)
	}
	sessionMains.mu.Lock()
	sessionMains.m["sess-1"] = sessionMain{fp: conversationFingerprint(main), last: time.Now().Add(-5 * time.Minute)}
	sessionMains.mu.Unlock()
	if r := sessionRole(fork, "main"); r != "main" {
		t.Fatalf("after the main went quiet a new conversation is the compacted main, got %s", r)
	}
	if r := sessionRole(&wire.Request{System: "You are a security monitor for autonomous AI", SessionID: "sess-1"}, "hook"); r != "hook" {
		t.Fatalf("non-main kinds pass through, got %s", r)
	}
}

// A routine turn that asks for deep thinking is capped to low; a hard turn
// keeps what it asked for; a request that asks for less is left alone.
func TestSmartRouteCapsEffortByTier(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.answer = "routine"
	f.mu.Unlock()
	req := &wire.Request{Messages: []wire.Msg{{Role: "user", Content: "rename a variable"}},
		Thinking: json.RawMessage(`{"type":"enabled","budget_tokens":16000}`)}
	_, note := p.AutoRoute(context.Background(), req)
	if req.ReasoningEffort != "low" || req.Thinking != nil || !strings.Contains(note, "effort=low") {
		t.Fatalf("routine should cap high thinking to low: effort=%q thinking=%s note=%q", req.ReasoningEffort, req.Thinking, note)
	}
	low := &wire.Request{Messages: []wire.Msg{{Role: "user", Content: "rename a variable again"}}, ReasoningEffort: "off"}
	_, note = p.AutoRoute(context.Background(), low)
	if low.ReasoningEffort != "off" || strings.Contains(note, "effort=") {
		t.Fatalf("a lower request is untouched: %q %q", low.ReasoningEffort, note)
	}
	f.mu.Lock()
	f.answer = "hard"
	f.mu.Unlock()
	hard := &wire.Request{Messages: []wire.Msg{{Role: "user", Content: "prove the lemma"}}, Thinking: json.RawMessage(`{"type":"enabled","budget_tokens":16000}`)}
	_, note = p.AutoRoute(context.Background(), hard)
	if hard.Thinking == nil || strings.Contains(note, "effort=") {
		t.Fatalf("hard keeps its thinking budget: %s %q", hard.Thinking, note)
	}
}

// The provider's prompt count from the previous turn overrides the byte
// estimate, so a conversation that grew past local_max_tokens re-routes even
// though its bytes/4 estimate still looks small.
func TestSmartRouteUsesActualPromptTokens(t *testing.T) {
	p, _, _ := smartFixture(t)
	req := &wire.Request{System: "sys", Messages: []wire.Msg{{Role: "user", Content: "small"}}}
	m, note := p.AutoRoute(context.Background(), req)
	if m != "local/tiel-a" {
		t.Fatalf("turn 1: %s", m)
	}
	fp := conversationFingerprint(req)
	noteConvTokens(convTagOf(note), 117_000) // what the provider billed for that turn
	if knownConvTokens(fp) != 117_000 {
		t.Fatalf("conv tokens not recorded: %d", knownConvTokens(fp))
	}
	m, note = p.AutoRoute(context.Background(), req)
	if m != "cloud/terra" || strings.Contains(note, "sticky") {
		t.Fatalf("117k actual tokens exceed local_max_tokens 100k → re-route, got %s %q", m, note)
	}
}
