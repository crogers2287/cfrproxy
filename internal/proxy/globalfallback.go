package proxy

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// maxGlobalHops caps how many global-chain targets one request may try. Each
// failed candidate costs a round trip (plus a retry), so an unbounded chain
// could stall a request for minutes.
const maxGlobalHops = 5

// GlobalFallback is the catch-all failover chain. Where a provider's own
// `fallback` field wires one specific hop, this applies to EVERY request:
// whenever the addressed provider (and its own chain) can't serve — quota or
// subscription usage exhausted, rate limited, overloaded, context overflow, or
// unreachable — the request continues down this ordered list instead of
// failing the harness.
type GlobalFallback struct {
	Enabled bool     `json:"enabled"`
	Targets []string `json:"targets"` // ordered "provider/model" refs
}

func (p *Proxy) GlobalFallbackConfig() GlobalFallback {
	var c GlobalFallback
	if raw := p.Store.Setting("global_fallback"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// appendGlobalFallback extends cands with the configured global chain.
//
// Dedupe is by provider+model, not provider alone: models on one provider can
// have independent quotas (an exhausted Claude opus window while sonnet still
// serves), so "same provider, different model" is a legitimate next hop.
// Share-endpoint requests never get upgraded past their allow-list.
func (p *Proxy) appendGlobalFallback(ctx context.Context, cands []candidate, ep *store.Endpoint) []candidate {
	cfg := p.GlobalFallbackConfig()
	if !cfg.Enabled {
		return cands
	}
	seen := make(map[string]bool, len(cands)+len(cfg.Targets))
	for _, c := range cands {
		seen[pairKey(c.prov.ID, c.model)] = true
	}
	added := 0
	for _, t := range cfg.Targets {
		if added >= maxGlobalHops {
			break
		}
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		fprov, fmodel, err := p.ResolveModel(ctx, t)
		if err != nil || !fprov.Enabled {
			continue
		}
		k := pairKey(fprov.ID, fmodel)
		if seen[k] {
			continue
		}
		// a scoped share endpoint must not be silently routed to a model its
		// owner didn't grant (ForceModel endpoints are already pinned upstream)
		if ep != nil && ep.ForceModel == "" && !p.modelAllowed(*ep, t) {
			continue
		}
		seen[k] = true
		cands = append(cands, candidate{prov: fprov, model: fmodel, failover: true})
		added++
	}
	return cands
}

func pairKey(provID int64, model string) string {
	return strings.ToLower(model) + "@" + strconv.FormatInt(provID, 10)
}

// contextExceeded detects "ran out of context/tokens" rejections. Unlike a
// malformed request, these can succeed on a model with a larger window, so
// they're worth continuing down the fallback chain.
func contextExceeded(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, m := range []string{
		"context length",
		"context_length_exceeded",
		"maximum context",
		"context window",
		"too many tokens",
		"reduce the length",
		"prompt is too long",
		"exceeds the maximum",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// failoverNotices rate-limits the failover banner.
//
// The banner is injected per model call, so without this a harness tool-loop
// stacks a copy into every reply of a turn.
//
// The key is deliberately GLOBAL — just "from -> to" — and not tied to the
// conversation. An earlier version keyed on conversationFingerprint (system +
// first user message) and was useless in practice: Hermes injects the current
// time, memories and system-reminders into the system prompt, so every single
// call hashed to a "new" conversation and every single call re-announced. Any
// key derived from prompt content has that failure mode; the provider pair does
// not.
//
// The TTL means a provider that is still down hours later re-announces itself
// rather than failing over silently forever.
type failoverNotices struct {
	mu sync.Mutex
	m  map[string]time.Time
}

const (
	failoverNoticeTTL = 30 * time.Minute
	failoverNoticeMax = 4096
)

var noticeCache = &failoverNotices{m: map[string]time.Time{}}

// announce reports whether this failover should be shown, recording it when so.
func (f *failoverNotices) announce(key string) bool {
	if key == "" {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.m[key]; ok && time.Since(t) < failoverNoticeTTL {
		return false
	}
	// crude bound, same as stickyRoutes: drop the table rather than grow without
	// limit. The cost of losing it is one extra banner.
	if len(f.m) >= failoverNoticeMax {
		f.m = map[string]time.Time{}
	}
	f.m[key] = time.Now()
	return true
}
