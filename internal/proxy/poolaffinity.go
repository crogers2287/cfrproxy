package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// Prefix-affine pool routing.
//
// REQ-089 gave a logical model several upstream instances and dispatched each
// request to whichever had the fewest in flight. That is the right rule for
// throughput and the WRONG one for a KV cache: every llama.cpp instance owns a
// separate slot cache and a separate KVarN attachment store, both keyed by the
// llama-swap model name. A prefix captured on instance A cannot be restored on
// instance B, so sending turn N+1 of a live conversation to the other card
// forces a cold prefill of the whole prompt — tens of seconds at the 30-90k
// tokens these agents run — to save a few hundred milliseconds of queueing.
//
// So the pool routes by affinity first and load second:
//
//	1. the SAME conversation always returns to the instance that already holds
//	   its KV, for as long as the binding is alive. This never yields to load:
//	   queueing behind one request is cheaper than re-prefilling 60k tokens.
//	2. a new conversation whose STATIC prefix (system + tools) has been served
//	   before prefers that instance — its attachment store is the one that can
//	   restore the prefix — unless that instance is meaningfully deeper in work
//	   than its sibling, in which case the request spreads.
//	3. a prefix nobody has seen is placed by load: the llama-swap /slots view
//	   when one is cached, in-flight count otherwise.
//
// All three are opt-in per pool. The setting stays backwards compatible:
//
//	{"tiel-w6800": ["tiel-coder-q5-w6800", "tiel-b-w6800"]}          // as before, least-busy only
//	{"ornith": {"members": ["ornith-kvx-w6800", "ornith-kvx-w6800-b"]}}  // affinity + probe + sibling failover
//
// A pool declared as a bare array behaves exactly as it did before this file
// existed; the object form turns the new routing on (each flag individually
// overridable).

// poolSpec is one entry of the model_pools setting, in either form.
type poolSpec struct {
	Members []string
	// Affinity routes by conversation/prefix before load.
	Affinity bool
	// Probe consults the upstream's own /slots view for cold placement.
	Probe bool
	// Failover offers the sibling instances as retry candidates.
	Failover bool
}

func cleanMembers(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		if m = strings.TrimSpace(m); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// parsePools reads the model_pools setting. Unparseable entries are dropped
// individually: one bad pool must not disable the others.
func parsePools(raw string) map[string]*poolSpec {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var generic map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &generic) != nil {
		return nil
	}
	out := make(map[string]*poolSpec, len(generic))
	for name, rm := range generic {
		if spec := poolSpecFromJSON(rm); spec != nil {
			out[name] = spec
		}
	}
	return out
}

func poolSpecFromJSON(rm json.RawMessage) *poolSpec {
	var arr []string
	if json.Unmarshal(rm, &arr) == nil {
		// Legacy form. Everything new stays off: these pools were measured with
		// pure least-busy dispatch and keep exactly that.
		return &poolSpec{Members: cleanMembers(arr)}
	}
	var obj struct {
		Members  []string `json:"members"`
		Affinity *bool    `json:"affinity"`
		Probe    *bool    `json:"probe_load"`
		Failover *bool    `json:"failover"`
	}
	if json.Unmarshal(rm, &obj) != nil {
		return nil
	}
	s := &poolSpec{Members: cleanMembers(obj.Members), Affinity: true, Probe: true, Failover: true}
	if obj.Affinity != nil {
		s.Affinity = *obj.Affinity
	}
	if obj.Probe != nil {
		s.Probe = *obj.Probe
	}
	if obj.Failover != nil {
		s.Failover = *obj.Failover
	}
	return s
}

// poolSpecFor returns the pool a logical model fans out to, or nil when the
// name is not pooled. A pool of one is not a pool.
func (p *Proxy) poolSpecFor(model string) *poolSpec {
	spec := parsePools(p.Store.Setting("model_pools"))[model]
	if spec == nil || len(spec.Members) < 2 {
		return nil
	}
	return spec
}

const (
	// A binding must outlive the gaps in a working session: an agent that goes
	// quiet for an hour and comes back still wants its KV. The cost of holding
	// one is a map entry, and the cost of losing one is a full re-prefill.
	poolAffinityTTL = 2 * time.Hour
	// How much deeper a prefix-affine instance may be before a NEW conversation
	// starts on its sibling instead. 1 = "already working": two streams sharing
	// one card halve each other's decode rate, and a conversation that does not
	// exist yet is the cheapest thing in the system to move — it has no KV to
	// lose, only a static prefix that the other instance will capture on its
	// own first pass. A conversation ALREADY bound to an instance is never
	// moved by this, whatever the depth.
	poolYieldDepth = 1
)

// poolAffinity binds conversations and static prefixes to pool members. It is
// deliberately NOT routeCache: that table maps a conversation to an auto-router
// decision, and the same fingerprint would then mean two different things.
var poolAffinity = &stickyRoutes{m: map[string]stickyEntry{}}

// reset drops every binding. Tests only — production wants the table to
// survive for as long as the process does.
func (s *stickyRoutes) reset() {
	s.mu.Lock()
	s.m = map[string]stickyEntry{}
	s.mu.Unlock()
}

// poolConvKey identifies one conversation, using the same stable head
// (system + first user message) the auto-router pins on. Every turn of a
// conversation yields the same key while it grows.
func poolConvKey(req *wire.Request) string {
	fp := conversationFingerprint(req)
	if fp == "" {
		return ""
	}
	return "conv:" + fp
}

// poolPrefixKey identifies the STATIC head — system prompt plus tool schemas —
// which is exactly the span the KV attachment store captures and restores. It
// is built from the same two content hashes the prefix manifest is built from
// (see prefixcache.go), so "the prefix cfrproxy recorded" and "the prefix this
// route is sticky to" are the same object.
func poolPrefixKey(req *wire.Request) string {
	if req == nil || (strings.TrimSpace(req.System) == "" && len(req.Tools) == 0) {
		return ""
	}
	sysSHA, toolsSHA := staticPrefixSHAs(req.System, req.Tools)
	return "prefix:" + sha256hex([]byte(sysSHA + "\x00" + toolsSHA))[:24]
}

// poolChoice is the dispatch decision: which member serves the request, which
// members remain as failover, and how to re-point the bindings if the request
// ends up served somewhere else.
type poolChoice struct {
	member   string
	rest     []string
	convKey  string
	prefKey  string
	why      string
	failover bool
}

func (c poolChoice) active() bool { return c.member != "" }

// rebind moves this conversation's binding to the member that actually served
// it. Without this a dead instance keeps attracting its old conversations for
// the whole TTL, and each of them pays a failover round-trip.
func (c poolChoice) rebind(model string) {
	if model == "" {
		return
	}
	poolAffinity.put(c.convKey, model, "pool")
	poolAffinity.put(c.prefKey, model, "pool")
}

func memberOf(members []string, m string) bool {
	for _, x := range members {
		if x == m {
			return true
		}
	}
	return false
}

func othersThan(members []string, picked string) []string {
	out := make([]string, 0, len(members))
	for _, m := range members {
		if m != picked {
			out = append(out, m)
		}
	}
	return out
}

// routePool picks the member for this request.
func (p *Proxy) routePool(spec *poolSpec, prov store.Provider, req *wire.Request) poolChoice {
	ch := poolChoice{failover: spec.Failover}
	if !spec.Affinity {
		ch.member, ch.why = p.pickPoolMember(spec.Members), "least-busy"
		ch.rest = othersThan(spec.Members, ch.member)
		return ch
	}
	ch.convKey, ch.prefKey = poolConvKey(req), poolPrefixKey(req)

	if m := p.boundMember(ch.convKey, spec.Members); m != "" {
		// Same conversation: its KV lives there. Never yields to load — a
		// queued turn costs seconds, a re-prefill costs tens of them.
		ch.member, ch.why = m, "conversation"
	} else if m := p.boundMember(ch.prefKey, spec.Members); m != "" && !p.poolDeeperThanSiblings(spec.Members, m) {
		// New conversation, this instance's attachment store already holds
		// the static prefix it needs, and it is free to take it.
		ch.member, ch.why = m, "prefix"
	} else {
		ch.member, ch.why = p.pickColdMember(spec, prov)
	}
	// Refresh the conversation binding on every turn so an active session never
	// expires mid-flight. The prefix binding is written only when it is unset
	// (or points at a member that has left the pool): first instance to warm a
	// static prefix keeps ownership of it, which is what makes the attachment
	// store useful — spreading happens per conversation, not per prefix.
	poolAffinity.put(ch.convKey, ch.member, "pool")
	if p.boundMember(ch.prefKey, spec.Members) == "" {
		poolAffinity.put(ch.prefKey, ch.member, "pool")
	}
	ch.rest = othersThan(spec.Members, ch.member)
	return ch
}

// boundMember returns the live binding for a key, or "" when there is none or
// it points at a member that is no longer in the pool.
func (p *Proxy) boundMember(key string, members []string) string {
	m, _, ok := poolAffinity.get(key, poolAffinityTTL)
	if !ok || !memberOf(members, m) {
		return ""
	}
	return m
}

// poolDeeperThanSiblings reports whether m is far enough behind that a NEW
// conversation is better off starting cold on another instance.
func (p *Proxy) poolDeeperThanSiblings(members []string, m string) bool {
	best := p.inflight.get(p.pickPoolMember(members))
	return p.inflight.get(m)-best >= poolYieldDepth
}

// pickColdMember places a prefix nobody has served yet. The upstream's own
// slot view is the better signal — it sees work this proxy did not dispatch —
// but it is never fetched on the request path, so a cold cache degrades to the
// in-flight counter rather than adding latency.
func (p *Proxy) pickColdMember(spec *poolSpec, prov store.Provider) (string, string) {
	if spec.Probe {
		if load, ok := p.poolLoad(prov, spec.Members); ok && load.usable(spec.Members) {
			best, bestScore := "", 0
			for _, m := range spec.Members {
				score := load.score(m)*8 + int(p.inflight.get(m))
				if best == "" || score < bestScore {
					best, bestScore = m, score
				}
			}
			if best != "" {
				return best, "cold/slots"
			}
		}
	}
	return p.pickPoolMember(spec.Members), "cold/inflight"
}

// ---- upstream load probe -------------------------------------------------

// llama-swap answers GET /upstream/<model>/slots with llama.cpp's slot table.
// Two rules make that safe to use:
//
//   - /upstream/<model>/... STARTS the model if it is not loaded. So the probe
//     asks GET /running first (which never loads anything) and only reads slots
//     for models already resident. A member that is not resident is scored as
//     the worst possible placement rather than woken up: swapping a 35B model in
//     costs far more than queueing.
//   - it runs in the background off the request path, behind a short timeout,
//     and every failure degrades to the in-flight counter.
type poolLoadSnapshot struct {
	at    time.Time
	ready map[string]bool
	busy  map[string]int // slots currently processing
	slots map[string]int // slots the instance has
}

// score ranks a member for cold placement: fewer busy slots is better, and a
// model that is not resident loses to any resident one.
func (s *poolLoadSnapshot) score(m string) int {
	if s == nil || !s.ready[m] {
		return 1 << 20
	}
	return s.busy[m]
}

// usable reports whether the snapshot says anything about this pool. A probe
// that reached the server but found no member resident (llama-swap swapped
// them out, or /running failed) carries no placement signal at all, and the
// in-flight counter is the more honest answer.
func (s *poolLoadSnapshot) usable(members []string) bool {
	if s == nil {
		return false
	}
	for _, m := range members {
		if s.ready[m] {
			return true
		}
	}
	return false
}

type poolLoadCache struct {
	mu         sync.Mutex
	entries    map[string]*poolLoadSnapshot
	refreshing map[string]bool
}

const (
	// Refresh a snapshot older than this on the next cold placement...
	poolLoadTTL = 3 * time.Second
	// ...but keep using it until it is this old; beyond that the picture is
	// stale enough that the in-flight counter is the more honest signal.
	poolLoadMaxAge = 20 * time.Second
	poolProbeHTTP  = 900 * time.Millisecond
	poolProbeTotal = 2500 * time.Millisecond
)

// poolLoad returns the cached slot picture for a provider, refreshing it in the
// background when stale. It NEVER blocks the request.
func (p *Proxy) poolLoad(prov store.Provider, members []string) (*poolLoadSnapshot, bool) {
	key := strings.TrimRight(prov.BaseURL, "/")
	p.poolload.mu.Lock()
	if p.poolload.entries == nil {
		p.poolload.entries = map[string]*poolLoadSnapshot{}
		p.poolload.refreshing = map[string]bool{}
	}
	snap := p.poolload.entries[key]
	stale := snap == nil || time.Since(snap.at) > poolLoadTTL
	if stale && !p.poolload.refreshing[key] {
		p.poolload.refreshing[key] = true
		go func() {
			p.refreshPoolLoad(prov, members)
			p.poolload.mu.Lock()
			delete(p.poolload.refreshing, key)
			p.poolload.mu.Unlock()
		}()
	}
	p.poolload.mu.Unlock()
	if snap == nil || time.Since(snap.at) > poolLoadMaxAge {
		return nil, false
	}
	return snap, true
}

// refreshPoolLoad fetches /running and, for resident members only, /slots.
// Exported to the package (not the network) so tests can drive it directly
// instead of racing the background goroutine.
func (p *Proxy) refreshPoolLoad(prov store.Provider, members []string) {
	ctx, cancel := context.WithTimeout(context.Background(), poolProbeTotal)
	defer cancel()
	base := providerRoot(prov)
	snap := &poolLoadSnapshot{at: time.Now(), ready: map[string]bool{},
		busy: map[string]int{}, slots: map[string]int{}}

	running, err := p.probeRunning(ctx, base)
	if err != nil {
		// No running view: record an empty (all-not-ready) snapshot rather than
		// nothing, so score() reports "unknown" and pickColdMember falls back to
		// the in-flight counter instead of guessing.
		p.storePoolLoad(prov, snap)
		return
	}
	for _, m := range members {
		if !running[m] {
			continue
		}
		snap.ready[m] = true
		busy, total, err := p.probeSlots(ctx, base, m)
		if err != nil {
			// Resident but unreadable: keep it eligible, unscored.
			continue
		}
		snap.busy[m], snap.slots[m] = busy, total
	}
	p.storePoolLoad(prov, snap)
}

func (p *Proxy) storePoolLoad(prov store.Provider, snap *poolLoadSnapshot) {
	key := strings.TrimRight(prov.BaseURL, "/")
	p.poolload.mu.Lock()
	if p.poolload.entries == nil {
		p.poolload.entries = map[string]*poolLoadSnapshot{}
		p.poolload.refreshing = map[string]bool{}
	}
	p.poolload.entries[key] = snap
	p.poolload.mu.Unlock()
}

// providerRoot strips a trailing version segment: the llama-swap control
// endpoints (/running, /upstream/...) live at the server root, not under /v1.
func providerRoot(prov store.Provider) string {
	base := strings.TrimRight(prov.BaseURL, "/")
	if endsWithVersion(base) {
		base = base[:strings.LastIndex(base, "/")]
	}
	return base
}

func (p *Proxy) probeGet(ctx context.Context, url string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, poolProbeHTTP)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return b, nil
}

// probeRunning lists the models llama-swap currently holds loaded. Unlike
// /upstream/<model>/..., this never causes a swap.
func (p *Proxy) probeRunning(ctx context.Context, base string) (map[string]bool, error) {
	b, err := p.probeGet(ctx, base+"/running")
	if err != nil {
		return nil, err
	}
	var out struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	ready := map[string]bool{}
	for _, r := range out.Running {
		// "ready" is llama-swap's own word for a process that is up and
		// serving; anything else (starting, stopping) is not a placement target.
		if r.Model != "" && r.State == "ready" {
			ready[r.Model] = true
		}
	}
	return ready, nil
}

// probeSlots reads llama.cpp's slot table for one resident model.
func (p *Proxy) probeSlots(ctx context.Context, base, model string) (busy, total int, err error) {
	b, err := p.probeGet(ctx, base+"/upstream/"+model+"/slots")
	if err != nil {
		return 0, 0, err
	}
	var slots []struct {
		IsProcessing bool `json:"is_processing"`
	}
	if err := json.Unmarshal(b, &slots); err != nil {
		return 0, 0, err
	}
	for _, s := range slots {
		if s.IsProcessing {
			busy++
		}
	}
	return busy, len(slots), nil
}
