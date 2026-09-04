package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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
	if m != "local/tiel-a" || note != "routine" {
		t.Fatalf("want warm local tiel-a on routine, got %s %q", m, note)
	}
	// second turn of the same conversation is pinned and skips the classifier
	m, note = p.AutoRoute(context.Background(), smallReq("fix the typo in main.go"))
	if m != "local/tiel-a" || note != "routine·sticky" {
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
	if m, note := p.AutoRoute(context.Background(), req); m != "cloud/terra" || note != "routine·sticky" {
		t.Fatalf("turn 3 should stick to cloud, got %s %q", m, note)
	}
}

func TestSmartClassifierPicksTier(t *testing.T) {
	p, _, f := smartFixture(t)
	f.mu.Lock()
	f.answer = "Hard"
	f.mu.Unlock()
	m, note := p.AutoRoute(context.Background(), smallReq("hello"))
	if m != "cloud/fable" || note != "hard" {
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
	if note != "hard" || m != "cloud/fable" {
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
