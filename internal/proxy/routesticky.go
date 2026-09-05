package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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
	// persistence (serve only; see EnablePins): the table is written to
	// `path` shortly after every change and read back at startup. Both
	// tables built on this type are what keep a conversation on the model
	// and the instance that hold its KV — losing them on a restart (23 today)
	// re-classified every live conversation and let it hop models.
	path  string
	dirty bool
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
	if len(s.m) >= stickyMaxEntries {
		s.evictOldestLocked()
	}
	s.m[fp] = stickyEntry{model: model, bucket: bucket, at: time.Now()}
	s.dirty = true
}

// ---- persistence ------------------------------------------------------------

type pinFile map[string]struct {
	Model  string    `json:"model"`
	Bucket string    `json:"bucket"`
	At     time.Time `json:"at"`
}

// load reads a table written by flush, dropping entries older than maxAge.
func (s *stickyRoutes) load(path string, maxAge time.Duration) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pf pinFile
	if json.Unmarshal(b, &pf) != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for fp, e := range pf {
		if fp == "" || e.Model == "" || time.Since(e.At) > maxAge {
			continue
		}
		s.m[fp] = stickyEntry{model: e.Model, bucket: e.Bucket, at: e.At}
		n++
	}
	s.path = path
	return n
}

// flush writes the table when it changed. Atomic (temp + rename) so a crash
// mid-write leaves the previous file.
func (s *stickyRoutes) flush() {
	s.mu.Lock()
	if s.path == "" || !s.dirty {
		s.mu.Unlock()
		return
	}
	pf := make(pinFile, len(s.m))
	for fp, e := range s.m {
		pf[fp] = struct {
			Model  string    `json:"model"`
			Bucket string    `json:"bucket"`
			At     time.Time `json:"at"`
		}{e.model, e.bucket, e.at}
	}
	s.dirty = false
	path := s.path
	s.mu.Unlock()
	b, err := json.Marshal(pf)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// EnablePins loads the sticky-route and pool-affinity tables from dataDir and
// keeps them written back every few seconds. Called by `serve` only: tests
// and one-shot CLI commands must not touch the live files.
func EnablePins(dataDir string) (routes, affinity int) {
	routes = routeCache.load(filepath.Join(dataDir, "route-pins.json"), stickyDefaultTTL*4)
	affinity = poolAffinity.load(filepath.Join(dataDir, "pool-affinity.json"), poolAffinityTTL)
	routeCache.path = filepath.Join(dataDir, "route-pins.json")
	poolAffinity.path = filepath.Join(dataDir, "pool-affinity.json")
	go func() {
		for range time.Tick(3 * time.Second) {
			routeCache.flush()
			poolAffinity.flush()
		}
	}()
	return
}

// evictOldestLocked drops the least recently pinned quarter of the table.
// It used to wipe the whole map, which for poolAffinity meant every live
// conversation lost its KV-cache binding at once — a synchronized re-prefill
// storm, the exact cost that table exists to avoid.
func (s *stickyRoutes) evictOldestLocked() {
	keys := make([]string, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return s.m[keys[i]].at.Before(s.m[keys[j]].at) })
	for _, k := range keys[:len(keys)/4] {
		delete(s.m, k)
	}
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
