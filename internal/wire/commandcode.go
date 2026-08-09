// Command Code /alpha/generate dialect.
//
// Command Code (commandcode.ai) gates its OpenAI/Anthropic-compatible
// /provider/v1/* endpoints to Pro/Provider plans; the $1 Go plan can only use
// /alpha/generate — the endpoint the `cmd` CLI itself calls. It speaks a
// custom envelope: a schema-strict "config" block, Vercel AI SDK ModelMessage[]
// for the conversation, JSON-Schema tools, and a newline-delimited JSON
// response (NOT SSE). Everything here is a re-derivation of the reverse-
// engineered protocol (verified against CLI v0.52.1) that tracks the CLI's
// own wire format — expect drift when the CLI moves.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// CommandCodeVersion is the x-command-code-version header the gateway
// validates against the installed CLI. Stale values are rejected, so this
// must track the CLI the operator's key belongs to. It can be overridden per
// provider via the provider's Headers config.
const CommandCodeVersion = "0.52.1"

// alphaConfig is the schema-strict config envelope. Every field is required;
// omitting any returns a 400 that lists the exact missing paths. Values are
// not load-bearing for routing — neutral defaults work fine.
type alphaConfig struct {
	WorkingDir    string `json:"workingDir"`
	Date          string `json:"date"`
	Environment   string `json:"environment"`
	Structure     []any  `json:"structure"`
	IsGitRepo     bool   `json:"isGitRepo"`
	CurrentBranch string `json:"currentBranch"`
	MainBranch    string `json:"mainBranch"`
	GitStatus     string `json:"gitStatus"`
	RecentCommits []any  `json:"recentCommits"`
}

type alphaToolOutput struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// alphaContentPart is one element of a ModelMessage content array.
type alphaContentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolName string           `json:"toolName,omitempty"`
	Input    any              `json:"input,omitempty"`
	Output   *alphaToolOutput `json:"output,omitempty"`
}

type alphaModelMessage struct {
	Role    string             `json:"role"`
	Content []alphaContentPart `json:"content"`
}

type alphaTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type alphaParams struct {
	Model       string               `json:"model"`
	System      string               `json:"system,omitempty"`
	Messages    []alphaModelMessage  `json:"messages"`
	Tools       []alphaTool          `json:"tools,omitempty"`
	MaxTokens   int                  `json:"max_tokens"`
	Temperature *float64             `json:"temperature,omitempty"`
	Stream      bool                 `json:"stream"`
}

type alphaRequest struct {
	Config         alphaConfig `json:"config"`
	Memory         string      `json:"memory"`
	Taste          any         `json:"taste"`
	Skills         any         `json:"skills"`
	PermissionMode string      `json:"permissionMode"`
	Params         alphaParams `json:"params"`
}

// BuildCommandCodeRequest translates the normalized request into the
// /alpha/generate envelope. The upstream only accepts stream:true, so it is
// forced here regardless of the client's preference; cfrproxy buffers the
// NDJSON for non-streaming clients on the way back.
func BuildCommandCodeRequest(r *Request) ([]byte, error) {
	mt := r.MaxTokens
	if mt <= 0 {
		mt = 32000
	}
	req := alphaRequest{
		Config: alphaConfig{
			Date:          time.Now().Format("2006-01-02"),
			Environment:   "production",
			Structure:     []any{},
			RecentCommits: []any{},
		},
		Memory:         "",
		Taste:          nil,
		Skills:         nil,
		PermissionMode: "standard",
		Params: alphaParams{
			Model:       r.Model,
			System:      r.System,
			Messages:    alphaModelMessages(r.Messages),
			Tools:       alphaTools(r.Tools),
			MaxTokens:   mt,
			Temperature: r.Temperature,
			Stream:      true,
		},
	}
	return json.Marshal(req)
}

// alphaModelMessages converts the normalized conversation to Vercel AI SDK
// ModelMessage[]. Assistant tool calls become tool-call parts; tool results
// become their own role:"tool" message with a tool-result part.
func alphaModelMessages(msgs []Msg) []alphaModelMessage {
	// toolName is required on tool-result parts but the normalized form only
	// carries toolCallID — recover the name from the assistant turn that
	// issued the call.
	nameByID := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				nameByID[tc.ID] = tc.Name
			}
		}
	}
	out := make([]alphaModelMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			out = append(out, alphaModelMessage{Role: "tool", Content: []alphaContentPart{{
				Type:       "tool-result",
				ToolCallID: m.ToolCallID,
				ToolName:   nameByID[m.ToolCallID],
				Output:     &alphaToolOutput{Type: "text", Value: m.Content},
			}}})
			continue
		}
		parts := make([]alphaContentPart, 0, 1+len(m.ToolCalls))
		if m.Content != "" {
			parts = append(parts, alphaContentPart{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			var input any
			if err := json.Unmarshal([]byte(tc.Args), &input); err != nil || input == nil {
				input = map[string]any{}
			}
			parts = append(parts, alphaContentPart{Type: "tool-call", ToolCallID: tc.ID, ToolName: tc.Name, Input: input})
		}
		if len(parts) == 0 {
			parts = append(parts, alphaContentPart{Type: "text", Text: ""})
		}
		out = append(out, alphaModelMessage{Role: m.Role, Content: parts})
	}
	return out
}

func alphaTools(tools []Tool) []alphaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]alphaTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Params
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, alphaTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

// ---- response (NDJSON) ----

type alphaUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	CachedInputTokens int `json:"cachedInputTokens"`
}

type alphaEvent struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	Text         string          `json:"text"`
	Delta        string          `json:"delta"`
	Input        json.RawMessage `json:"input"`
	FinishReason string          `json:"finishReason"`
	Usage        *alphaUsage     `json:"usage"`
	TotalUsage   *alphaUsage     `json:"totalUsage"`
	Error        string          `json:"error"`
	Message      string          `json:"message"`
}

func alphaUsageOf(ev *alphaEvent) *alphaUsage {
	if ev.Usage != nil {
		return ev.Usage
	}
	return ev.TotalUsage
}

// alphaFinish normalizes the gateway's finish reasons to the OpenAI spelling.
func alphaFinish(f string) string {
	switch f {
	case "tool-calls":
		return "tool_calls"
	case "":
		return "stop"
	default:
		return f
	}
}

// ReadCommandCodeStream parses the newline-delimited JSON event stream into
// normalized deltas. Tool-call arguments arrive as fragmented JSON deltas,
// keyed by the event id; the redundant final "tool-call" event is only used
// when no deltas were seen for that id (so args never duplicate).
func ReadCommandCodeStream(body io.Reader, out chan<- Delta) {
	defer close(out)
	sc := bufio.NewScanner(body)
	// tool inputs can be large; default scanner cap is way too small
	sc.Buffer(make([]byte, 256*1024), 16*1024*1024)
	toolIdx := map[string]int{}
	toolName := map[string]string{}
	toolSeen := map[string]bool{}
	nextIdx := 0
	var finish string
	var usage *alphaUsage
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev alphaEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "reasoning-delta":
			out <- Delta{Reasoning: ev.Text}
		case "text-delta":
			out <- Delta{Text: ev.Text}
		case "tool-input-start":
			if ev.ID == "" {
				continue
			}
			if _, ok := toolIdx[ev.ID]; !ok {
				toolIdx[ev.ID] = nextIdx
				nextIdx++
			}
			if ev.ToolName != "" {
				toolName[ev.ID] = ev.ToolName
			}
		case "tool-input-delta":
			if ev.ID == "" {
				continue
			}
			idx, ok := toolIdx[ev.ID]
			if !ok {
				idx = nextIdx
				nextIdx++
				toolIdx[ev.ID] = idx
			}
			toolSeen[ev.ID] = true
			out <- Delta{TC: &TCDelta{Index: idx, ID: ev.ID, Name: toolName[ev.ID], Args: ev.Delta}}
		case "tool-call":
			id := ev.ToolCallID
			if id == "" {
				id = ev.ID
			}
			if id != "" && !toolSeen[id] {
				idx, ok := toolIdx[id]
				if !ok {
					idx = nextIdx
					nextIdx++
					toolIdx[id] = idx
				}
				args := string(ev.Input)
				if args == "null" {
					args = "{}"
				}
				out <- Delta{TC: &TCDelta{Index: idx, ID: id, Name: ev.ToolName, Args: args}}
			}
		case "finish-step", "finish":
			if u := alphaUsageOf(&ev); u != nil {
				usage = u
			}
			if ev.FinishReason != "" {
				finish = ev.FinishReason
			}
		case "error":
			msg := ev.Error
			if msg == "" {
				msg = ev.Message
			}
			out <- Delta{Err: errors.New(msg)}
			return
		}
	}
	if err := sc.Err(); err != nil {
		out <- Delta{Err: err}
		return
	}
	pt, ct, cached := 0, 0, 0
	if usage != nil {
		// inputTokens is the TOTAL including cached; the normalized form keeps
		// prompt and cached disjoint.
		pt = usage.InputTokens - usage.CachedInputTokens
		ct = usage.OutputTokens
		cached = usage.CachedInputTokens
	}
	out <- Delta{Finish: alphaFinish(finish), PromptTokens: pt, CompletionTokens: ct, CachedTokens: cached}
}

// ParseCommandCodeResponse buffers a whole NDJSON body (a non-streaming client
// against a gateway that always streams) into a single normalized response.
func ParseCommandCodeResponse(body []byte) (*Response, error) {
	r := &Response{FinishReason: "stop"}
	var text strings.Builder
	argsByID := map[string]string{}
	nameByID := map[string]string{}
	seenID := map[string]bool{}
	order := []string{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev alphaEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "reasoning-delta":
			r.ReasoningContent += ev.Text
		case "text-delta":
			text.WriteString(ev.Text)
		case "tool-input-start":
			if ev.ID == "" {
				continue
			}
			if !seenID[ev.ID] {
				seenID[ev.ID] = true
				order = append(order, ev.ID)
			}
			if ev.ToolName != "" {
				nameByID[ev.ID] = ev.ToolName
			}
		case "tool-input-delta":
			if ev.ID == "" {
				continue
			}
			if !seenID[ev.ID] {
				seenID[ev.ID] = true
				order = append(order, ev.ID)
			}
			argsByID[ev.ID] += ev.Delta
		case "tool-call":
			id := ev.ToolCallID
			if id == "" {
				id = ev.ID
			}
			if !seenID[id] {
				seenID[id] = true
				order = append(order, id)
			}
			argsByID[id] = string(ev.Input)
			if ev.ToolName != "" {
				nameByID[id] = ev.ToolName
			}
		case "finish-step", "finish":
			if u := alphaUsageOf(&ev); u != nil {
				r.PromptTokens = u.InputTokens - u.CachedInputTokens
				r.CompletionTokens = u.OutputTokens
				r.CachedTokens = u.CachedInputTokens
			}
			if ev.FinishReason != "" {
				r.FinishReason = ev.FinishReason
			}
		case "error":
			msg := ev.Error
			if msg == "" {
				msg = ev.Message
			}
			return nil, fmt.Errorf("command code error: %s", msg)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	r.Content = text.String()
	for _, id := range order {
		r.ToolCalls = append(r.ToolCalls, ToolCall{ID: id, Name: nameByID[id], Args: argsByID[id]})
	}
	r.FinishReason = alphaFinish(r.FinishReason)
	return r, nil
}
