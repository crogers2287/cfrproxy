package proxy

// Fusion pipeline (à la OpenRouter Fusion): a request addressed to the virtual
// model "fusion" is sent in parallel to several participant models; their
// drafts are then handed to a judge model which synthesizes one final answer.
// The judge call is the request the rest of the data path actually runs, so
// the synthesized answer streams back like any normal completion.
//
// Config in settings key "fusion":
//
//	{"enabled": true,
//	 "participants": ["codex/gpt-5-terra", "gemini/gemini-3-flash", "grok/grok-4"],
//	 "judge": "anthropic/claude-opus",
//	 "max_tokens": 2000}

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

// judgePrompt is the synthesizer's system instruction (optimized via
// prompt-master): reason from the drafts, emit only the finished answer.
const judgePrompt = "You are the lead expert answering the user's request. Several reference drafts from other experts are appended below the user's message. Weigh the drafts against each other and your own judgment: keep what is correct, fix what is wrong, fill what is missing, and merge the strongest ideas. Then write the single best answer to the user's original request, in the form they asked for. Respond as one authoritative voice — the drafts are your private reference, so your reply contains only the finished answer, with no mention of drafts, other models, or your process."

type FusionConfig struct {
	Enabled      bool     `json:"enabled"`
	Participants []string `json:"participants"`
	Judge        string   `json:"judge"`
	MaxTokens    int      `json:"max_tokens"`
}

func (p *Proxy) FusionConfig() FusionConfig {
	c := FusionConfig{MaxTokens: 2000}
	if raw := p.Store.Setting("fusion"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2000
	}
	return c
}

// internalComplete runs one non-streaming completion for a model spec and
// returns its text. Used for fusion participants (and reusable elsewhere).
func (p *Proxy) internalComplete(ctx context.Context, modelSpec string, base *wire.Request, maxTokens int) (string, error) {
	prov, model, err := p.ResolveModel(ctx, modelSpec)
	if err != nil {
		return "", err
	}
	creq := *base
	creq.Model = model
	creq.Stream = false
	creq.MaxTokens = maxTokens
	body, err := buildOutbound(prov.Type, &creq)
	if err != nil {
		return "", err
	}
	resp, err := p.send(ctx, prov, providerPath(prov.Type), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	norm, err := parseOutboundResponse(prov.Type, rb)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(norm.Content), nil
}

// Fusion fans the request out to the participant models, then rewrites req in
// place into a single synthesis call to the judge (drafts appended to the last
// user message, judge instruction prepended to the system). Returns the judge
// model spec and a trace note, or ok=false to fall through to normal routing.
func (p *Proxy) Fusion(ctx context.Context, req *wire.Request) (string, string, bool) {
	cfg := p.FusionConfig()
	if !cfg.Enabled || len(cfg.Participants) == 0 || cfg.Judge == "" {
		return "", "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	drafts := make([]string, len(cfg.Participants))
	var wg sync.WaitGroup
	for i, m := range cfg.Participants {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			if ans, err := p.internalComplete(cctx, m, req, cfg.MaxTokens); err == nil {
				drafts[i] = ans
			}
		}(i, m)
	}
	wg.Wait()

	var b strings.Builder
	n := 0
	for i, d := range drafts {
		if strings.TrimSpace(d) == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n[Draft %d — %s]\n%s\n", n, cfg.Participants[i], d)
	}
	if n == 0 {
		return "", "", false // every participant failed; route normally
	}

	// judge system = judge instruction, plus the user's original system so the
	// judge still honors their constraints
	sys := judgePrompt
	if req.System != "" {
		sys += "\n\n" + req.System
	}
	req.System = sys

	// append the drafts to the last user message (or add one)
	block := "\n\n---\nReference drafts from other experts (your private reference):\n" + b.String()
	appended := false
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			req.Messages[i].Content += block
			appended = true
			break
		}
	}
	if !appended {
		req.Messages = append(req.Messages, wire.Msg{Role: "user", Content: block})
	}

	return cfg.Judge, fmt.Sprintf("fusion(%d)→%s", n, cfg.Judge), true
}
