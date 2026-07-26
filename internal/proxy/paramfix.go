package proxy

// Recovery from provider-rejected body parameters.
//
// In passthrough mode cfrproxy forwards the harness's raw JSON body (only the
// model is rewritten), so provider-specific tuning reaches the provider. The
// cost is that a harness which always sends some parameter will hard-fail
// against a model that refuses it — omp sends "enable_thinking": false, and
// Alibaba's thinking models answer 400 "The value of the enable_thinking
// parameter is restricted to True."
//
// The harness can't know that per model, and the provider won't budge, so
// cfrproxy is the only layer that can fix it: when a 400 names the parameter it
// rejected, drop that one key and retry once. A dropped tuning hint is strictly
// better than a failed request, and the drop is recorded on the trace.

import (
	"encoding/json"
	"regexp"
	"strings"
)

// paramNamePatterns pull the offending key out of an error message. Providers
// word these differently and most leave the structured "param" field null.
var paramNamePatterns = []*regexp.Regexp{
	// Alibaba/Qwen: "The value of the enable_thinking parameter is restricted to True."
	regexp.MustCompile(`(?i)\bthe\s+([a-z_][a-z0-9_]*)\s+parameter\b`),
	// "parameter 'foo' is not supported" / `parameter "foo"`
	regexp.MustCompile(`(?i)\bparameter\s+['"` + "`" + `]([a-z_][a-z0-9_]*)['"` + "`" + `]`),
	// "'foo' is not supported" / "unsupported parameter: foo"
	regexp.MustCompile(`(?i)unsupported\s+parameter:?\s+['"` + "`" + `]?([a-z_][a-z0-9_]*)`),
	// "Unrecognized request argument supplied: foo" (OpenAI)
	regexp.MustCompile(`(?i)unrecognized\s+request\s+argument\s+supplied:?\s+([a-z_][a-z0-9_]*)`),
}

// protectedParams are structural — dropping one would change what was asked
// rather than how it was tuned, so a request naming them is a real error and
// must surface to the harness.
var protectedParams = map[string]bool{
	"model": true, "messages": true, "stream": true, "system": true,
	"tools": true, "tool_choice": true, "prompt": true, "input": true,
}

// rejectedParam returns the top-level body key a 4xx blames, or "".
func rejectedParam(errBody []byte) string {
	var e struct {
		Error struct {
			Param   any    `json:"param"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(errBody, &e)

	// structured field first — OpenAI-compatible providers set it when they can
	if s, ok := e.Error.Param.(string); ok {
		if k := sanitiseParam(s); k != "" {
			return k
		}
	}
	msg := e.Error.Message
	if msg == "" {
		msg = e.Message
	}
	if msg == "" {
		msg = string(errBody)
	}
	for _, re := range paramNamePatterns {
		if m := re.FindStringSubmatch(msg); len(m) > 1 {
			if k := sanitiseParam(m[1]); k != "" {
				return k
			}
		}
	}
	return ""
}

func sanitiseParam(s string) string {
	s = strings.TrimSpace(strings.Trim(s, `"'`+"`"))
	// nested paths like "messages[0].content" are not a top-level key we can drop
	if s == "" || strings.ContainsAny(s, ".[]/ ") || protectedParams[strings.ToLower(s)] {
		return ""
	}
	return s
}

// stripBodyParam removes a top-level key, reporting whether it was there. The
// body is re-marshalled from a generic map, so key order is not preserved —
// irrelevant to every provider, and the alternative is a hand-rolled JSON
// editor for no benefit.
func stripBodyParam(body []byte, key string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, ok := m[key]; !ok {
		return body, false
	}
	delete(m, key)
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}
