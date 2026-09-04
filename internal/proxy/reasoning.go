package proxy

import (
	"encoding/json"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// Thinking level per route.
//
// A request that carries no reasoning field is not "thinking off": every
// dialect leaves the decision to the model's chat template, and the Qwen3.8
// family template resolves `reasoning_effort|default('xhigh')` — so an agent
// harness that never sets a level runs every turn at the most expensive
// setting (measured against the live Flash-Next server, 2026-09-04). The
// share endpoint and the provider can each carry a level; the share wins.
// By default it fills in only when the client sent nothing, so a harness
// that asks for a level keeps it; "force" overrides the client too.

// reasoningFor resolves the level for a route: share endpoint, then provider.
func reasoningFor(ep *store.Endpoint, prov store.Provider) (level string, force bool) {
	if ep != nil && ep.ReasoningEffort != "" {
		return ep.ReasoningEffort, ep.ReasoningForce
	}
	if prov.ReasoningEffort != "" {
		return prov.ReasoningEffort, prov.ReasoningForce
	}
	return "", false
}

// anthropicBudget mirrors the effort→budget table in wire/anthropic.go.
var anthropicBudget = map[string]int{"low": 2048, "medium": 8192, "high": 16384, "xhigh": 24576}

// applyReasoning sets the thinking level on the FINAL outbound body for the
// given dialect and reports whether it changed anything. The body is
// re-marshalled from a map, like stripBodyParam; key order is irrelevant to
// every upstream.
//
//   - openai:     reasoning_effort (llama.cpp forwards it to the template;
//     OpenAI/vLLM read it natively). "off" cannot be said with
//     that field — llama.cpp ignores "none" — so it becomes
//     chat_template_kwargs.enable_thinking=false, which only a
//     llama.cpp/vLLM-style server understands.
//   - responses:  reasoning.effort ("none" for off).
//   - anthropic:  thinking {enabled, budget_tokens} / {disabled}, budget kept
//     under max_tokens because the API rejects the reverse.
//   - ollama:     think true/false.
func applyReasoning(body []byte, otype, level string, force bool) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body, false
	}
	changed := false
	switch otype {
	case "openai":
		var kw map[string]json.RawMessage
		if raw, ok := m["chat_template_kwargs"]; ok {
			json.Unmarshal(raw, &kw)
		}
		_, hasEff := m["reasoning_effort"]
		_, kwEff := kw["reasoning_effort"]
		_, kwEn := kw["enable_thinking"]
		if (hasEff || kwEff || kwEn) && !force {
			return body, false
		}
		if kw == nil {
			kw = map[string]json.RawMessage{}
		}
		delete(kw, "reasoning_effort")
		delete(kw, "enable_thinking")
		if level == "off" {
			delete(m, "reasoning_effort")
			kw["enable_thinking"] = json.RawMessage("false")
		} else {
			m["reasoning_effort"] = mustJSON(level)
		}
		if len(kw) > 0 {
			m["chat_template_kwargs"] = mustJSON(kw)
		} else {
			delete(m, "chat_template_kwargs")
		}
		changed = true
	case "responses":
		var rs map[string]json.RawMessage
		if raw, ok := m["reasoning"]; ok {
			json.Unmarshal(raw, &rs)
		}
		if _, has := rs["effort"]; has && !force {
			return body, false
		}
		if rs == nil {
			rs = map[string]json.RawMessage{}
		}
		eff := level
		if level == "off" {
			eff = "none"
		}
		rs["effort"] = mustJSON(eff)
		m["reasoning"] = mustJSON(rs)
		changed = true
	case "anthropic":
		if _, has := m["thinking"]; has && !force {
			return body, false
		}
		if level == "off" {
			m["thinking"] = json.RawMessage(`{"type":"disabled"}`)
			changed = true
			break
		}
		budget := anthropicBudget[level]
		if raw, ok := m["max_tokens"]; ok {
			var mt int
			if json.Unmarshal(raw, &mt) == nil && mt > 0 && mt <= budget {
				// the API needs max_tokens > budget_tokens and budget >= 1024
				if mt/2 < 1024 {
					return body, false
				}
				budget = mt / 2
			}
		}
		m["thinking"] = mustJSON(map[string]any{"type": "enabled", "budget_tokens": budget})
		changed = true
	case "ollama":
		if _, has := m["think"]; has && !force {
			return body, false
		}
		m["think"] = mustJSON(level != "off")
		changed = true
	default:
		return body, false
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
