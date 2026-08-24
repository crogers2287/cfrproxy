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

// NamedFusionConfig resolves a custom fusion by name, exactly as
// NamedRouterConfig does for routers. A disabled or incomplete fusion reports
// not-found so the caller falls through to normal routing rather than running a
// half-configured pipeline.
func (p *Proxy) NamedFusionConfig(name string) (FusionConfig, bool) {
	f, ok := p.Store.FusionByName(name)
	if !ok || !f.Enabled || f.Judge == "" || len(f.Participants) == 0 {
		return FusionConfig{}, false
	}
	c := FusionConfig{
		Enabled:      true,
		Participants: f.Participants,
		Judge:        f.Judge,
		MaxTokens:    f.MaxTokens,
	}
	if c.MaxTokens <= 0 {
		// Inherit the default fusion's budget so a named pipeline does not
		// silently generate stub-length drafts.
		c.MaxTokens = p.FusionConfig().MaxTokens
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2000
	}
	return c, true
}

// fusionSpec reports the fusion a model id addresses, if any. Both separators
// are accepted: "fusion:NAME" matches the "auto:NAME" convention, while
// "fusion/NAME" is what a harness produces when it treats fusion as a provider
// mount — the same id reaching us two ways, so both must resolve.
// A bare "fusion" (or "<provider>/fusion") means the unnamed default.
func fusionSpec(model string) (name string, isFusion bool) {
	m := strings.TrimSpace(model)
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		// "cfrproxy-fusion/NAME" or "provider/fusion" both land here
		head, tail := m[:i], m[i+1:]
		if strings.EqualFold(tail, "fusion") {
			return "", true
		}
		if strings.EqualFold(head, "fusion") || strings.EqualFold(head, "cfrproxy-fusion") {
			return tail, true
		}
		m = tail
	}
	if strings.EqualFold(m, "fusion") {
		return "", true
	}
	if len(m) > len("fusion:") && strings.EqualFold(m[:len("fusion:")], "fusion:") {
		return m[len("fusion:"):], true
	}
	return "", false
}

// isFusionMount reports whether a /p/{name} mount addresses the virtual fusion
// provider rather than a configured one.
//
// Fusion pipelines are not backed by a store.Provider — they are a pipeline over
// several providers — but a harness can only add them as a normal OpenAI
// endpoint. Serving them at /p/fusion/v1 lets one be registered as a custom
// provider (e.g. "cfrproxy-fusion") whose catalogue is just the fusions, rather
// than pointing at the global mount and getting all ~140 models.
func isFusionMount(scope string) bool {
	return strings.EqualFold(scope, "fusion") || strings.EqualFold(scope, "cfrproxy-fusion")
}

// fusionModelIDs lists the selectable fusion ids.
//
// bare=true returns the ids as a SCOPED mount serves them ("deep"), matching how
// every other /p/{provider} mount lists bare model ids. That matters because a
// caller joins provider and model with a slash: bare gives "fusion/deep", while
// the prefixed form would give the unusable "fusion/fusion:deep".
//
// bare=false returns the globally addressable ids ("fusion:deep"), which is what
// the top-level catalogue and any router bucket names.
func (p *Proxy) fusionModelIDs(bare bool) []string {
	var ids []string
	if c := p.FusionConfig(); c.Enabled && len(c.Participants) > 0 && c.Judge != "" {
		// "fusion" is correct in both forms: scoped it becomes "fusion/fusion",
		// which resolves to the unnamed default.
		ids = append(ids, "fusion")
	}
	if fs, err := p.Store.Fusions(); err == nil {
		for _, f := range fs {
			if f.Enabled && f.Judge != "" && len(f.Participants) > 0 {
				if bare {
					ids = append(ids, f.Name)
				} else {
					ids = append(ids, "fusion:"+f.Name)
				}
			}
		}
	}
	return ids
}

// FusionConfigFor returns the config a fusion model id addresses.
func (p *Proxy) FusionConfigFor(model string) (FusionConfig, bool) {
	name, ok := fusionSpec(model)
	if !ok {
		return FusionConfig{}, false
	}
	if name == "" {
		c := p.FusionConfig()
		return c, c.Enabled && len(c.Participants) > 0 && c.Judge != ""
	}
	return p.NamedFusionConfig(name)
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
	return p.FusionWith(ctx, req, p.FusionConfig())
}

// FusionWith runs the pipeline against an explicit config, so a named fusion
// ("fusion:NAME") uses its own participants and judge instead of the global
// default.
func (p *Proxy) FusionWith(ctx context.Context, req *wire.Request, cfg FusionConfig) (string, string, bool) {
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
