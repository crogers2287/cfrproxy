package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
)

// Model pools: one logical model name served by several interchangeable
// upstream instances.
//
// A 35B MoE split across two W6800s with -sm layer reads the same total bytes
// per token as a single card would — the cards take turns rather than working
// together — so the split buys capacity, not speed, and measured 1.7x SLOWER
// under load than one card holding the whole model. Two independent instances,
// one per card, read in parallel and roughly double aggregate throughput while
// each stream keeps full single-card speed. Row split, which would let the
// cards genuinely share the work, is unavailable: the ROCm backend rejects it
// with "device ROCm0 does not support split buffers".
//
// What that costs is a single endpoint: llama-swap runs one process per model
// entry and does not load-balance, so the two instances are two model names.
// This restores the one name — a request for the logical model goes to
// whichever member is least busy, and llama.cpp queues it there if both are
// full.
//
// Configured with the "model_pools" setting, a JSON object of
// logical -> [members]:
//
//	{"tiel-w6800": ["tiel-coder-q5-w6800", "tiel-b-w6800"]}

// inflight counts requests currently dispatched to each upstream model. It is
// the dispatch signal: "least busy" beats round-robin here because turns vary
// enormously in length — one 4000-token answer outlives a dozen short ones,
// and strict alternation would keep feeding the instance still working on it.
type inflightCounter struct {
	mu sync.Mutex
	n  map[string]*int64
}

func (c *inflightCounter) counter(model string) *int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]*int64{}
	}
	v, ok := c.n[model]
	if !ok {
		v = new(int64)
		c.n[model] = v
	}
	return v
}

func (c *inflightCounter) add(model string, d int64) { atomic.AddInt64(c.counter(model), d) }
func (c *inflightCounter) get(model string) int64    { return atomic.LoadInt64(c.counter(model)) }

// poolMembers returns the upstream models a logical name fans out to, or nil
// when the name is not pooled.
func (p *Proxy) poolMembers(model string) []string {
	raw := strings.TrimSpace(p.Store.Setting("model_pools"))
	if raw == "" {
		return nil
	}
	var pools map[string][]string
	if json.Unmarshal([]byte(raw), &pools) != nil {
		return nil
	}
	members := pools[model]
	if len(members) < 2 {
		return nil // a pool of one is just the model itself
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// pickPoolMember returns the least-busy member. Ties go to the earlier member,
// which makes a quiet system deterministic and keeps the first instance warm
// rather than alternating cold prefixes across both.
func (p *Proxy) pickPoolMember(members []string) string {
	best, bestN := members[0], p.inflight.get(members[0])
	for _, m := range members[1:] {
		if n := p.inflight.get(m); n < bestN {
			best, bestN = m, n
		}
	}
	return best
}

// PoolStatus reports in-flight depth per member, for the diagnostic command.
func (p *Proxy) PoolStatus(model string) map[string]int64 {
	members := p.poolMembers(model)
	if members == nil {
		return nil
	}
	out := map[string]int64{}
	for _, m := range members {
		out[m] = p.inflight.get(m)
	}
	return out
}
