package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// affinityPool builds a proxy with one prefix-affine pool and an empty
// binding table, so each test starts from a known routing state.
func affinityPool(t *testing.T, pools string) *Proxy {
	t.Helper()
	poolAffinity.reset()
	return poolStore(t, pools)
}

const twoInstance = `{"ornith":{"members":["ornith-kvx-w6800","ornith-kvx-w6800-b"],"probe_load":false}}`

func agentReq(system, first string) *wire.Request {
	return &wire.Request{
		System:   system,
		Messages: []wire.Msg{{Role: "user", Content: first}},
		Tools:    []wire.Tool{{Name: "read_file", Description: "read a file", Params: json.RawMessage(`{"type":"object"}`)}},
	}
}

func TestPoolSpecForms(t *testing.T) {
	tests := []struct {
		name                      string
		raw                       string
		model                     string
		wantMembers               []string
		affinity, probe, failover bool
	}{
		{
			// The form every existing pool is written in. It must keep the
			// behaviour it was measured with in REQ-089: least-busy only.
			name: "legacy array is least-busy only",
			raw:  `{"tiel-w6800":["tiel-coder-q5-w6800","tiel-b-w6800"]}`, model: "tiel-w6800",
			wantMembers: []string{"tiel-coder-q5-w6800", "tiel-b-w6800"},
		},
		{
			name: "object form turns everything on",
			raw:  `{"ornith":{"members":["a","b"]}}`, model: "ornith",
			wantMembers: []string{"a", "b"}, affinity: true, probe: true, failover: true,
		},
		{
			name: "flags are individually overridable",
			raw:  `{"ornith":{"members":["a","b"],"probe_load":false,"failover":false}}`, model: "ornith",
			wantMembers: []string{"a", "b"}, affinity: true,
		},
		{
			name: "affinity can be switched off explicitly",
			raw:  `{"ornith":{"members":["a","b"],"affinity":false}}`, model: "ornith",
			wantMembers: []string{"a", "b"}, probe: true, failover: true,
		},
		{
			name: "duplicate and blank members collapse",
			raw:  `{"ornith":{"members":["a"," ","a","b"]}}`, model: "ornith",
			wantMembers: []string{"a", "b"}, affinity: true, probe: true, failover: true,
		},
		{
			// One malformed entry must not take the other pools down with it.
			name: "a broken sibling entry is skipped, not fatal",
			raw:  `{"broken":17,"ornith":{"members":["a","b"]}}`, model: "ornith",
			wantMembers: []string{"a", "b"}, affinity: true, probe: true, failover: true,
		},
		{name: "single member is not a pool", raw: `{"ornith":{"members":["a"]}}`, model: "ornith"},
		{name: "unknown model", raw: `{"ornith":{"members":["a","b"]}}`, model: "nope"},
		{name: "malformed setting", raw: `{not json`, model: "ornith"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := poolStore(t, tc.raw).poolSpecFor(tc.model)
			if tc.wantMembers == nil {
				if spec != nil {
					t.Fatalf("want no pool, got %+v", spec)
				}
				return
			}
			if spec == nil {
				t.Fatal("want a pool, got none")
			}
			if strings.Join(spec.Members, ",") != strings.Join(tc.wantMembers, ",") {
				t.Errorf("members = %v, want %v", spec.Members, tc.wantMembers)
			}
			if spec.Affinity != tc.affinity || spec.Probe != tc.probe || spec.Failover != tc.failover {
				t.Errorf("affinity/probe/failover = %v/%v/%v, want %v/%v/%v",
					spec.Affinity, spec.Probe, spec.Failover, tc.affinity, tc.probe, tc.failover)
			}
		})
	}
}

// The whole point of the feature: a conversation must keep landing on the
// instance that already holds its KV, even once that instance is the busier
// one. Re-prefilling 60k tokens costs far more than queueing behind a turn.
func TestConversationStaysOnItsInstance(t *testing.T) {
	p := affinityPool(t, twoInstance)
	spec := p.poolSpecFor("ornith")
	prov := store.Provider{Name: "fred", BaseURL: "http://fred:9069"}
	conv := agentReq("You are a coding agent.\nWorking directory: /srv/app", "fix the parser")

	first := p.routePool(spec, prov, conv)
	if first.member != "ornith-kvx-w6800" {
		t.Fatalf("cold pick = %q, want the first member on an idle pool", first.member)
	}

	// The instance is now working, and deeply: three turns in flight against a
	// sibling with none. Least-busy routing would move the conversation.
	p.inflight.add("ornith-kvx-w6800", 3)

	// Later turns of the SAME conversation: system + first user message are
	// unchanged, the tail has grown.
	conv.Messages = append(conv.Messages, wire.Msg{Role: "assistant", Content: "on it"},
		wire.Msg{Role: "user", Content: "now run the tests"})
	next := p.routePool(spec, prov, conv)
	if next.member != first.member {
		t.Errorf("turn 2 routed to %q, want %q — the KV is on the first instance", next.member, first.member)
	}
	if next.why != "conversation" {
		t.Errorf("reason = %q, want conversation affinity", next.why)
	}
}

// A NEW conversation sharing a static prefix prefers the instance whose
// attachment store already holds that prefix — but yields once that instance
// is queueing, so a busy agent does not monopolise one card.
func TestNewConversationPrefersWarmPrefixThenYields(t *testing.T) {
	p := affinityPool(t, twoInstance)
	spec := p.poolSpecFor("ornith")
	prov := store.Provider{Name: "fred", BaseURL: "http://fred:9069"}
	sys := "You are a coding agent.\nWorking directory: /srv/app"

	warm := p.routePool(spec, prov, agentReq(sys, "session one")).member

	// Same system + tools, different opening message = different conversation.
	second := p.routePool(spec, prov, agentReq(sys, "session two"))
	if second.member != warm {
		t.Errorf("second session went to %q, want %q — same static prefix", second.member, warm)
	}
	if second.why != "prefix" {
		t.Errorf("reason = %q, want prefix affinity", second.why)
	}

	// Now that instance is working: a brand-new conversation is better off on
	// the idle card than sharing decode bandwidth on this one.
	p.inflight.add(warm, poolYieldDepth)
	third := p.routePool(spec, prov, agentReq(sys, "session three"))
	if third.member == warm {
		t.Errorf("third session stayed on the saturated %q; it should spread", warm)
	}

	// ...but an established conversation still does not move.
	back := p.routePool(spec, prov, agentReq(sys, "session one"))
	if back.member != warm {
		t.Errorf("established conversation moved to %q under load, want %q", back.member, warm)
	}
}

// A binding that names an instance no longer in the pool must be ignored
// rather than routing to a model the provider no longer serves.
func TestAffinityToDepartedMemberIsDropped(t *testing.T) {
	p := affinityPool(t, twoInstance)
	spec := p.poolSpecFor("ornith")
	prov := store.Provider{Name: "fred", BaseURL: "http://fred:9069"}
	req := agentReq("You are a coding agent.", "hello")

	poolAffinity.put(poolConvKey(req), "ornith-retired-instance", "pool")
	got := p.routePool(spec, prov, req)
	if !memberOf(spec.Members, got.member) {
		t.Fatalf("routed to %q, which is not in the pool", got.member)
	}
	if got.why == "conversation" {
		t.Error("a stale binding was honoured")
	}
}

// The failover chain is the rest of the pool, in declared order.
func TestPoolChoiceOffersSiblingsForFailover(t *testing.T) {
	p := affinityPool(t, `{"ornith":{"members":["a","b","c"],"probe_load":false}}`)
	spec := p.poolSpecFor("ornith")
	got := p.routePool(spec, store.Provider{Name: "fred"}, agentReq("sys", "hi"))
	if !got.failover {
		t.Fatal("object-form pool should offer sibling failover")
	}
	if strings.Join(got.rest, ",") != "b,c" {
		t.Errorf("rest = %v, want the other two members", got.rest)
	}
	// After the picked instance dies and a sibling serves, the binding follows.
	got.rebind("b")
	again := p.routePool(spec, store.Provider{Name: "fred"}, agentReq("sys", "hi"))
	if again.member != "b" {
		t.Errorf("after rebind the conversation routed to %q, want b", again.member)
	}
}

// Existing pools must be untouched: no affinity, no siblings, pure least-busy.
func TestLegacyPoolBehaviourUnchanged(t *testing.T) {
	p := affinityPool(t, `{"tiel-w6800":["a","b"]}`)
	spec := p.poolSpecFor("tiel-w6800")
	prov := store.Provider{Name: "fred", BaseURL: "http://fred:9069"}
	req := agentReq("You are a coding agent.", "hello")

	first := p.routePool(spec, prov, req)
	if first.member != "a" || first.why != "least-busy" {
		t.Fatalf("first = %q (%s), want a (least-busy)", first.member, first.why)
	}
	if first.failover {
		t.Error("legacy pool must not add sibling candidates")
	}
	// The same conversation moves with load, exactly as before.
	p.inflight.add("a", 1)
	if second := p.routePool(spec, prov, req); second.member != "b" {
		t.Errorf("legacy pool routed to %q, want the least-busy b", second.member)
	}
	if first.convKey != "" || first.prefKey != "" {
		t.Error("legacy pool wrote affinity bindings")
	}
}

// The prefix key must be the static head and nothing else: it survives a
// growing conversation, and it changes when the tools change.
func TestPrefixKeyTracksStaticHeadOnly(t *testing.T) {
	base := agentReq("system text", "first question")
	grown := agentReq("system text", "a completely different opening")
	grown.Messages = append(grown.Messages, wire.Msg{Role: "user", Content: "and more"})
	if poolPrefixKey(base) != poolPrefixKey(grown) {
		t.Error("prefix key changed with the conversation body")
	}
	reordered := agentReq("system text", "first question")
	reordered.Tools = append(reordered.Tools, wire.Tool{Name: "write_file", Params: json.RawMessage(`{}`)})
	if poolPrefixKey(base) == poolPrefixKey(reordered) {
		t.Error("prefix key ignored a change to the tool schemas")
	}
	if poolPrefixKey(&wire.Request{}) != "" {
		t.Error("an empty request should not be pinned")
	}
	// It must agree with the manifest's own notion of a prefix.
	sysSHA, toolsSHA := staticPrefixSHAs(base.System, base.Tools)
	if want := "prefix:" + sha256hex([]byte(sysSHA + "\x00" + toolsSHA))[:24]; poolPrefixKey(base) != want {
		t.Errorf("prefix key = %q, want %q", poolPrefixKey(base), want)
	}
}

// swapServer mimics llama-swap: /running lists resident models, and
// /upstream/<model>/slots reports that model's slot table. A request for a
// model NOT in running is a fault — probing one would swap a 35B model in.
func swapServer(t *testing.T, running map[string]string, busy map[string]int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			Model string `json:"model"`
			State string `json:"state"`
		}
		var out struct {
			Running []row `json:"running"`
		}
		for m, st := range running {
			out.Running = append(out.Running, row{Model: m, State: st})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /upstream/{model}/slots", func(w http.ResponseWriter, r *http.Request) {
		m := r.PathValue("model")
		if running[m] != "ready" {
			t.Errorf("probed /slots for %q, which is not resident — that would force a model load", m)
			http.Error(w, "not loaded", 503)
			return
		}
		type slot struct {
			IsProcessing bool `json:"is_processing"`
		}
		slots := []slot{{}, {}}
		for i := 0; i < busy[m] && i < len(slots); i++ {
			slots[i].IsProcessing = true
		}
		json.NewEncoder(w).Encode(slots)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Cold placement uses the upstream's own slot view when one has been cached:
// it sees work this proxy did not dispatch.
func TestColdPlacementUsesSlotView(t *testing.T) {
	p := affinityPool(t, `{"ornith":{"members":["a","b"]}}`)
	spec := p.poolSpecFor("ornith")
	srv := swapServer(t, map[string]string{"a": "ready", "b": "ready"}, map[string]int{"a": 2, "b": 0})
	prov := store.Provider{Name: "fred", BaseURL: srv.URL}

	// Nothing cached yet: the request must NOT block on the network.
	if m, why := p.pickColdMember(spec, prov); m != "a" || why != "cold/inflight" {
		t.Fatalf("cold-cache pick = %q (%s), want a (cold/inflight)", m, why)
	}
	p.refreshPoolLoad(prov, spec.Members)
	if m, why := p.pickColdMember(spec, prov); m != "b" || why != "cold/slots" {
		t.Errorf("with a's slots full, pick = %q (%s), want b (cold/slots)", m, why)
	}
}

// A member llama-swap is not currently holding loaded is the worst placement,
// not a free one: swapping a 35B model in costs far more than queueing. And
// the probe must never touch /slots for it.
func TestColdPlacementAvoidsUnloadedInstance(t *testing.T) {
	p := affinityPool(t, `{"ornith":{"members":["a","b"]}}`)
	spec := p.poolSpecFor("ornith")
	srv := swapServer(t, map[string]string{"a": "ready"}, map[string]int{"a": 1})
	prov := store.Provider{Name: "fred", BaseURL: srv.URL}

	p.refreshPoolLoad(prov, spec.Members)
	if m, _ := p.pickColdMember(spec, prov); m != "a" {
		t.Errorf("pick = %q, want the resident a over the unloaded b", m)
	}
}

// Every probe failure degrades to the in-flight counter rather than erroring
// or stalling the request.
func TestProbeFailureDegradesToInflight(t *testing.T) {
	p := affinityPool(t, `{"ornith":{"members":["a","b"]}}`)
	spec := p.poolSpecFor("ornith")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", 500)
	}))
	defer dead.Close()
	prov := store.Provider{Name: "fred", BaseURL: dead.URL}

	p.refreshPoolLoad(prov, spec.Members)
	p.inflight.add("a", 1)
	if m, why := p.pickColdMember(spec, prov); m != "b" || why != "cold/inflight" {
		t.Errorf("pick = %q (%s), want b (cold/inflight)", m, why)
	}
}

// The llama-swap control endpoints live at the server root even when the
// provider's base URL was pasted with the SDK's /v1 suffix.
func TestProviderRootStripsVersionSegment(t *testing.T) {
	for base, want := range map[string]string{
		"http://fred:9069":     "http://fred:9069",
		"http://fred:9069/":    "http://fred:9069",
		"http://fred:8093/v1":  "http://fred:8093",
		"http://fred:8093/v1/": "http://fred:8093",
	} {
		if got := providerRoot(store.Provider{BaseURL: base}); got != want {
			t.Errorf("providerRoot(%q) = %q, want %q", base, got, want)
		}
	}
}

// End to end: an instance that is down must cost a retry on its sibling, not
// an error and not a reroute onto some other model. The reply carries no
// failover banner — the weights are identical, nothing about the answer
// changed — but the trace must name the dead instance.
func TestPooledRequestFailsOverToSiblingInstance(t *testing.T) {
	poolAffinity.reset()
	var mu sync.Mutex
	seen := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		mu.Lock()
		seen[req.Model]++
		mu.Unlock()
		if req.Model == "ornith-kvx-w6800" { // ROCm0 instance is wedged
			w.WriteHeader(503)
			w.Write([]byte(`{"error":"slot unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"` + req.Model + `","choices":[{"message":{"role":"assistant","content":"served by b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "fred", Type: "openai", BaseURL: upstream.URL,
		DefaultModel: "ornith", Models: "ornith", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("model_pools", `{"ornith":{"members":["ornith-kvx-w6800","ornith-kvx-w6800-b"],"probe_load":false}}`); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"ornith","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("want 200 via the sibling instance, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served by b") {
		t.Fatalf("answer did not come from the sibling: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "failover") {
		t.Errorf("an intra-pool retry must not inject a banner: %s", rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["ornith-kvx-w6800-b"] == 0 {
		t.Errorf("sibling instance was never tried: %v", seen)
	}
	traces, _ := s.Traces(0, 5)
	if len(traces) == 0 || traces[0].Model != "ornith-kvx-w6800-b" {
		t.Fatalf("trace should be attributed to the instance that served: %+v", traces)
	}
	if !strings.Contains(traces[0].Err, "pool failover") {
		t.Errorf("trace should record the dead instance: %q", traces[0].Err)
	}
	if !strings.Contains(traces[0].Note, "pool→") {
		t.Errorf("trace should say which member was picked: %q", traces[0].Note)
	}
}
