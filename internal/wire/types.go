// Package wire defines the normalized request/response schema and the
// translators between it and each provider dialect (openai, anthropic,
// ollama), including stream framing in both directions.
package wire

import "encoding/json"

// Request is the normal form. It is a superset shaped after OpenAI chat
// completions with Anthropic's top-level system prompt pulled out.
type Request struct {
	Model       string
	System      string
	Messages    []Msg
	Tools       []Tool
	MaxTokens   int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Stream      bool
	// reasoning controls, preserved across dialects
	ReasoningEffort string          // openai reasoning_effort
	Thinking        json.RawMessage // anthropic thinking block, verbatim
}

type Msg struct {
	Role       string // user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant messages
	ToolCallID string     // tool result messages
}

type ToolCall struct {
	ID   string
	Name string
	Args string // JSON-encoded arguments
}

type Tool struct {
	Name        string
	Description string
	Params      json.RawMessage // JSON schema
}

type Response struct {
	ID               string
	Model            string
	Content          string
	ToolCalls        []ToolCall
	FinishReason     string // stop | tool_calls | length
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int // cache-read/cached prompt tokens
}

// Delta is one unit of a normalized stream.
type Delta struct {
	Text             string
	TC               *TCDelta
	Finish           string // non-empty on the final delta
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	Err              error
}

// TCDelta is a streamed tool-call fragment.
type TCDelta struct {
	Index int
	ID    string
	Name  string
	Args  string // argument JSON fragment
}

// finish reason mapping helpers
func FinishToAnthropic(f string) string {
	switch f {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func FinishFromAnthropic(f string) string {
	switch f {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}
