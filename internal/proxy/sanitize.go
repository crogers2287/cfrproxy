package proxy

import "encoding/json"

// sanitizeEmptySystem removes system messages with empty content from an
// OpenAI-style messages array, and drops an empty top-level "system" field
// (Anthropic style). Some providers hard-reject `{"role":"system","content":""}`
// (DeepSeek: "system message must have content"), and several agent stacks
// emit exactly that when no system prompt is configured (observed 2026-08-27:
// Hermes memory-merge and roundtable consults, five 400s in 12 seconds and a
// tool guardrail trip). The body is returned unchanged unless something was
// actually removed.
func sanitizeEmptySystem(body []byte) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	changed := false

	if raw, ok := doc["system"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s == "" {
			delete(doc, "system")
			changed = true
		}
	}

	if raw, ok := doc["messages"]; ok {
		var msgs []map[string]json.RawMessage
		if json.Unmarshal(raw, &msgs) == nil {
			kept := msgs[:0]
			for _, m := range msgs {
				var role, content string
				json.Unmarshal(m["role"], &role)
				isEmpty := false
				if role == "system" {
					if c, ok := m["content"]; !ok || string(c) == "null" {
						isEmpty = true
					} else if json.Unmarshal(c, &content) == nil && content == "" {
						isEmpty = true
					}
				}
				if isEmpty {
					changed = true
					continue
				}
				// Truncated tool calls (output-limit cuts mid-JSON) come back on
				// the next turn inside history; strict templates then fail the
				// whole request with a 500 JSON parse error, wedging agent
				// loops. Replace unparseable arguments with a valid wrapper
				// that preserves the prefix as context.
				if role == "assistant" {
					if rawTC, ok := m["tool_calls"]; ok {
						var tcs []map[string]json.RawMessage
						if json.Unmarshal(rawTC, &tcs) == nil {
							tcChanged := false
							for _, tc := range tcs {
								var fn map[string]json.RawMessage
								if json.Unmarshal(tc["function"], &fn) != nil {
									continue
								}
								var args string
								if json.Unmarshal(fn["arguments"], &args) != nil {
									continue
								}
								if args == "" || json.Valid([]byte(args)) {
									continue
								}
								if len(args) > 2000 {
									args = args[:2000]
								}
								repaired, _ := json.Marshal(map[string]string{
									"_truncated_args": args,
								})
								fn["arguments"], _ = json.Marshal(string(repaired))
								tc["function"], _ = json.Marshal(fn)
								tcChanged = true
							}
							if tcChanged {
								if b, err := json.Marshal(tcs); err == nil {
									m["tool_calls"] = b
									changed = true
								}
							}
						}
					}
				}
				kept = append(kept, m)
			}
			if changed {
				if b, err := json.Marshal(kept); err == nil {
					doc["messages"] = b
				} else {
					return body
				}
			}
		}
	}

	if !changed {
		return body
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// defaultMaxTokens fills in a generation cap when the caller omitted one.
//
// llama.cpp's n_predict defaults to -1 — generate until the context window is
// full. On a 262K-context local model that is a request which owns a serving
// slot for hours: one observed turn had grown 6.5K -> 13.3K tokens and was
// still going, while every other request queued behind it. The harness that
// sent it never asked for that; it just left max_tokens off, which on a cloud
// API means "a sensible default" and here means "until the context ends".
//
// This is applied to the raw inbound body so the passthrough path (which
// forwards the client's bytes verbatim) is covered too, not just translated
// requests. A caller that specifies its own limit — even a large one — is left
// alone; this only supplies what was missing. 0 disables it.
func defaultMaxTokens(body []byte, n int) []byte {
	if n <= 0 {
		return body
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	// any spelling the caller might have used, across dialects
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if raw, ok := doc[k]; ok && string(raw) != "null" {
			return body
		}
	}
	// ollama carries it as options.num_predict
	if raw, ok := doc["options"]; ok {
		var opts map[string]json.RawMessage
		if json.Unmarshal(raw, &opts) == nil {
			if v, ok := opts["num_predict"]; ok && string(v) != "null" {
				return body
			}
		}
	}
	b, err := json.Marshal(n)
	if err != nil {
		return body
	}
	doc["max_tokens"] = b
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}
