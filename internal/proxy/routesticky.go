package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

// Prompt caching is per (provider, model, prefix): a warm cache only helps if
// consecutive turns of the same conversation hit the SAME model. Classifying
// every turn independently means turn 1 can land on grok and turn 2 on claude,
// so each turn pays full price for a cold prefix.
//
// stickyRoutes pins a conversation to the model it was first routed to. Later
// turns reuse that decision — which also skips the classifier call entirely,
// so it's cheaper and lower-latency on top of keeping the cache warm.
type stickyRoutes struct {
	mu sync.Mutex
	m  map[string]stickyEntry
}

type stickyEntry struct {
	model  string
	bucket string
	at     time.Time
}

const (
	stickyDefaultTTL = 30 * time.Minute
	stickyMaxEntries = 4096
)

var routeCache = &stickyRoutes{m: map[string]stickyEntry{}}

func (s *stickyRoutes) get(fp string, ttl time.Duration) (string, string, bool) {
	if fp == "" {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[fp]
	if !ok || time.Since(e.at) > ttl {
		return "", "", false
	}
	return e.model, e.bucket, true
}

func (s *stickyRoutes) put(fp, model, bucket string) {
	if fp == "" || model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// crude bound: a proxy that has seen thousands of conversations drops the
	// whole table rather than growing without limit. Losing stickiness just
	// means the next turn re-classifies.
	if len(s.m) >= stickyMaxEntries {
		s.m = map[string]stickyEntry{}
	}
	s.m[fp] = stickyEntry{model: model, bucket: bucket, at: time.Now()}
}

// conversationFingerprint identifies a conversation by its STABLE head — the
// system prompt plus the first user message. Those don't change as a
// conversation grows, so every turn of one conversation yields the same
// fingerprint while different conversations get different ones.
func conversationFingerprint(req *wire.Request) string {
	if req == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(req.System))
	h.Write([]byte{0})
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content != "" {
			h.Write([]byte(m.Content))
			break
		}
	}
	// a bare request with neither system nor user text isn't worth pinning
	if req.System == "" && len(req.Messages) == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// appendBriefing attaches generated text (an auto-plan briefing) to the END of
// the conversation rather than to the system prompt.
//
// Prompt caches match from the start of the request. The briefing is different
// on every turn, so folding it into `system` would change token 0 and throw
// away the cached prefix — the entire 100k-token agent prompt — on every single
// turn. Appending to the final user message leaves everything before it
// byte-identical, so the bulk of the prefix still hits cache.
func appendBriefing(req *wire.Request, brief string) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && req.Messages[i].Content != "" {
			req.Messages[i].Content += "\n\n" + brief
			return
		}
	}
	// nothing to attach to (e.g. a tool-result-only turn) — fall back to system
	if req.System != "" {
		req.System += "\n\n" + brief
		return
	}
	req.System = brief
}

// sticky reports whether route pinning is on. Absent in the stored JSON means
// on: it's the cache-friendly default and callers who never set it should get
// the cheaper behaviour.
func (c AutoRouterConfig) sticky() bool {
	return c.Sticky == nil || *c.Sticky
}

func (c AutoRouterConfig) stickyTTL() time.Duration {
	if c.StickyTTLMinutes > 0 {
		return time.Duration(c.StickyTTLMinutes) * time.Minute
	}
	return stickyDefaultTTL
}
