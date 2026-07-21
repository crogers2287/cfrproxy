package proxy

// Context compression: when a request's estimated tokens exceed the
// threshold, older conversation turns are summarized by a cheap model and
// replaced with one compressed block; the most recent turns stay verbatim.
// Summaries are cached by content hash so an ongoing conversation only pays
// for compression once per prefix. Modeled on the proxy-side compaction
// pattern validated by kompact and LiteLLM's LLMLingua-2 integration; a
// future "llmlingua" strategy can slot in behind the same config.
//
// Settings key "compression":
//
//	{"enabled":true, "threshold_tokens":24000, "keep_recent":8,
//	 "summarizer":"claude/claude-haiku-4-5", "target_words":500}

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

type CompressionConfig struct {
	Enabled         bool   `json:"enabled"`
	ThresholdTokens int    `json:"threshold_tokens"`
	KeepRecent      int    `json:"keep_recent"`
	Summarizer      string `json:"summarizer"`
	TargetWords     int    `json:"target_words"`
}

func (p *Proxy) CompressionConfig() CompressionConfig {
	c := CompressionConfig{ThresholdTokens: 24000, KeepRecent: 8, TargetWords: 500}
	if raw := p.Store.Setting("compression"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	if c.KeepRecent < 2 {
		c.KeepRecent = 2
	}
	if c.ThresholdTokens < 1000 {
		c.ThresholdTokens = 1000
	}
	if c.TargetWords < 100 {
		c.TargetWords = 100
	}
	return c
}

// estTokens: chars/4 heuristic — cheap and good enough for a threshold gate.
func estTokens(req *wire.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Args) + len(tc.Name)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Params) + len(t.Description)
	}
	return n / 4
}

type summaryCache struct {
	mu      sync.Mutex
	entries map[string]summaryEntry
}

type summaryEntry struct {
	text string
	at   time.Time
}

const summaryCacheMax = 200

// Compress mutates req in place when it exceeds the threshold: older turns
// beyond keep_recent are summarized (cached by hash) into one block. Returns
// a trace note ("" = untouched). Fail-open: any summarizer error leaves the
// request unmodified.
func (p *Proxy) Compress(ctx context.Context, req *wire.Request) string {
	cfg := p.CompressionConfig()
	if !cfg.Enabled || cfg.Summarizer == "" {
		return ""
	}
	before := estTokens(req)
	if before < cfg.ThresholdTokens || len(req.Messages) <= cfg.KeepRecent+2 {
		return ""
	}
	// never split a tool-call/result pair: walk the cut point forward past
	// tool-role messages so the kept window starts on a clean turn
	cut := len(req.Messages) - cfg.KeepRecent
	for cut < len(req.Messages) && req.Messages[cut].Role == "tool" {
		cut++
	}
	if cut <= 1 || cut >= len(req.Messages) {
		return ""
	}
	old := req.Messages[:cut]

	var raw strings.Builder
	for _, m := range old {
		fmt.Fprintf(&raw, "[%s] %s\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&raw, "[%s calls %s(%s)]\n", m.Role, tc.Name, snipStr(tc.Args, 200))
		}
	}
	h := sha256.Sum256([]byte(raw.String()))
	key := hex.EncodeToString(h[:16])

	p.summaries.mu.Lock()
	if p.summaries.entries == nil {
		p.summaries.entries = map[string]summaryEntry{}
	}
	cached, ok := p.summaries.entries[key]
	p.summaries.mu.Unlock()

	summary := cached.text
	if !ok {
		prov, model, err := p.ResolveModel(ctx, cfg.Summarizer)
		if err != nil {
			return ""
		}
		hist := raw.String()
		if len(hist) > 200000 {
			hist = hist[len(hist)-200000:] // cap what the summarizer sees
		}
		prompt := fmt.Sprintf(
			"Compress this conversation history into a faithful working summary under %d words. Preserve: decisions made, facts established, file paths, code identifiers, numbers, open questions, and the user's constraints. Drop pleasantries and repetition. Plain text, no markdown. Never add information that is not in the history.\n\nHistory:\n%s",
			cfg.TargetWords, hist)
		creq := &wire.Request{Model: model, MaxTokens: cfg.TargetWords * 3,
			Messages: []wire.Msg{{Role: "user", Content: prompt}}}
		body, err := buildOutbound(prov.Type, creq)
		if err != nil {
			return ""
		}
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		resp, err := p.send(cctx, prov, providerPath(prov.Type), body)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode >= 400 {
			return ""
		}
		norm, err := parseOutboundResponse(prov.Type, rb)
		if err != nil || strings.TrimSpace(norm.Content) == "" {
			return ""
		}
		summary = strings.TrimSpace(norm.Content)
		p.summaries.mu.Lock()
		if len(p.summaries.entries) >= summaryCacheMax {
			p.summaries.entries = map[string]summaryEntry{} // simple reset eviction
		}
		p.summaries.entries[key] = summaryEntry{text: summary, at: time.Now()}
		p.summaries.mu.Unlock()
	}

	kept := make([]wire.Msg, len(req.Messages[cut:]))
	copy(kept, req.Messages[cut:])
	req.Messages = append([]wire.Msg{{Role: "user",
		Content: "[Earlier conversation, compressed by cfrproxy — treat as accurate history]\n" + summary}}, kept...)
	after := estTokens(req)
	cacheTag := ""
	if ok {
		cacheTag = " (cached)"
	}
	return fmt.Sprintf("compressed ~%d→~%d tok%s", before, after, cacheTag)
}

func snipStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
