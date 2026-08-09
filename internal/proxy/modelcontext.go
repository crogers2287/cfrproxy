package proxy

// Curated context windows, sourced from provider model cards.
//
// Most upstreams cfrproxy fronts return the bare OpenAI listing shape — id,
// object, created, owned_by — and say nothing about context. CLIProxyAPI,
// ollama.com and the Alibaba/z.ai endpoints all do this. So a harness asking
// cfrproxy "how big is this model's window?" got nothing back and fell through
// to its own guess, which is derived from the model id. That guess is wrong in
// both directions here: ids are frequently renamed by the aggregator
// ("claude-opencode-go-deepseek-v4-flash"), and Hermes' fallback default is
// 256K — under-reporting a 1M model and over-reporting a 128K one.
//
// The failure is quiet and expensive. Hermes sizes compaction as a fraction of
// the window, so a window that reads too large means compaction never fires:
// a session grows until every turn re-prefills a quarter-million uncached
// tokens and each reply takes 48s. Nothing errors; it just gets slower.
//
// Every number below comes from the provider's own published model card, not
// from inference off the id. A model we could not source is deliberately
// ABSENT rather than estimated — advertising a wrong window is worse than
// advertising none, because "none" lets the harness fall back to a documented
// default while a wrong number is silently trusted.

import "strings"

// contextRule maps a distinctive fragment of a model id to that family's
// published context window.
//
// Matching is on substring rather than equality because the same model reaches
// cfrproxy under several ids: "claude-sonnet-5" direct, but
// "claude-opencode-sonnet-5" and "claude-command-sonnet-5" through the
// aggregator mounts. Fragments therefore omit vendor prefixes and carry the
// family token that survives renaming.
type contextRule struct {
	match string
	ctx   int
}

// unsourced marks a model we deliberately advertise nothing for. It exists so
// a narrow "we checked and the vendor does not publish this" entry can outrank
// a broader family rule that would otherwise supply a plausible-looking number.
const unsourced = 0

// modelContextRules is consulted longest-match-first, so a more specific rule
// always beats a broader one that also matches: "gpt-5.6" wins over "gpt-5",
// and "glm-5.2" (1M) wins over "glm-5" (200K). Order in this slice is for
// human reading only.
var modelContextRules = []contextRule{
	// Anthropic — platform.claude.com model cards.
	// Claude 5 family ships 1M as both default and maximum; there is no
	// smaller context variant. Claude 4.x and 3.x remain at 200K.
	{"opus-5", 1_000_000},
	{"sonnet-5", 1_000_000},
	{"fable-5", 1_000_000},
	{"opus-4", 200_000},
	{"sonnet-4", 200_000},
	{"haiku-4-5", 200_000},
	{"3-7-sonnet", 200_000},
	{"3-5-haiku", 200_000},

	// OpenAI — developers.openai.com model pages.
	// The whole 5.5/5.6 generation shares 1,050,000.
	{"gpt-5.6", 1_050_000},
	{"gpt-5.5", 1_050_000},
	{"gpt-5.4", 1_050_000},
	{"gpt-oss-120b", 131_072},
	// The mini tier is a separate model and OpenAI does not publish its
	// window on the 5.4 page. A ctx of 0 records "we looked and could not
	// source it" and, because longest-match wins, stops the broader
	// "gpt-5.4" rule from lending it a number it may not have.
	{"gpt-5.4-mini", unsourced},

	// Google — ai.google.dev. Every Gemini 3.x tier (flash, pro, lite,
	// and the 3.1/3.5/3.6 refreshes) is 1,048,576 in, 65,536 out.
	{"gemini-3", 1_048_576},

	// xAI — docs.x.ai. Note 4.5 is SMALLER than 4.3: the newer model
	// trades window for token efficiency, so id order is not size order.
	{"grok-4.5", 500_000},
	{"grok-4.3", 1_000_000},
	{"grok-4.20", 1_000_000},
	{"grok-build", 256_000},

	// Z.ai — docs.z.ai. 5.2 jumped to 1M; everything before it is 200K,
	// except 4.5 which never left 128K.
	{"glm-5.2", 1_000_000},
	{"glm-5.1", 200_000},
	{"glm-5", 200_000},
	{"glm-4.6", 200_000},
	{"glm-4.5", 128_000},

	// DeepSeek — api-docs.deepseek.com. Pro and Flash share one window.
	{"deepseek-v4", 1_000_000},

	// Alibaba — Model Studio. The 3.5-3.8 max/plus tiers are all 1M.
	{"qwen-3.5", 1_000_000},
	{"qwen-3.6", 1_000_000},
	{"qwen-3.7", 1_000_000},
	{"qwen-3.8", 1_000_000},
	{"qwen3.5", 1_000_000},
	{"qwen3.6", 1_000_000},
	{"qwen3.7", 1_000_000},
	{"qwen3.8", 1_000_000},
}

// catalogContextFor returns the published window for a model id, or 0 when the
// model is not in the catalog. Longest match wins so that a family refresh with
// a different window (grok-4.5 at 500K inside a 1M-era lineup) is not captured
// by the broader rule.
func catalogContextFor(model string) int {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return 0
	}
	best, bestLen := 0, 0
	for _, r := range modelContextRules {
		if len(r.match) > bestLen && strings.Contains(id, r.match) {
			best, bestLen = r.ctx, len(r.match)
		}
	}
	return best
}
