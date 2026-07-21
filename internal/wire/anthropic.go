package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- Anthropic /v1/messages dialect ----

type antBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // tool_result payload
}

type antMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []antBlock
}

type antReq struct {
	Model       string          `json:"model"`
	System      json.RawMessage `json:"system,omitempty"` // string or []antBlock
	Messages    []antMsg        `json:"messages"`
	Tools       []antTool       `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop_sequences,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Thinking    json.RawMessage `json:"thinking,omitempty"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func antText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []antBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

func ParseAnthropicRequest(body []byte) (*Request, error) {
	var in antReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad anthropic request: %w", err)
	}
	r := &Request{Model: in.Model, System: antText(in.System), MaxTokens: in.MaxTokens,
		Temperature: in.Temperature, TopP: in.TopP, Stop: in.Stop, Stream: in.Stream, Thinking: in.Thinking}
	for _, m := range in.Messages {
		// Claude Code's Agent SDK injects system-role messages mid-conversation
		// (session hooks, reminders). Fold them into the top-level system —
		// same as the openai parser — so strict chat templates that require
		// system-first (e.g. local models) don't reject the request.
		if m.Role == "system" {
			if txt := antText(m.Content); txt != "" {
				if r.System != "" {
					r.System += "\n\n"
				}
				r.System += txt
			}
			continue
		}
		var blocks []antBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			// plain string content
			r.Messages = append(r.Messages, Msg{Role: m.Role, Content: antText(m.Content)})
			continue
		}
		msg := Msg{Role: m.Role}
		var toolResults []Msg
		for _, bl := range blocks {
			switch bl.Type {
			case "text":
				msg.Content += bl.Text
			case "tool_use":
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: bl.ID, Name: bl.Name, Args: string(bl.Input)})
			case "tool_result":
				toolResults = append(toolResults, Msg{Role: "tool", Content: antText(bl.Content), ToolCallID: bl.ToolUseID})
			}
		}
		// tool_result blocks arrive inside a user message; normal form wants
		// them as standalone tool-role messages before any user text.
		r.Messages = append(r.Messages, toolResults...)
		if msg.Content != "" || len(msg.ToolCalls) > 0 {
			r.Messages = append(r.Messages, msg)
		}
	}
	for _, t := range in.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Params: t.InputSchema})
	}
	return r, nil
}

func BuildAnthropicRequest(r *Request) ([]byte, error) {
	out := antReq{Model: r.Model, MaxTokens: r.MaxTokens, Temperature: r.Temperature,
		TopP: r.TopP, Stop: r.Stop, Stream: r.Stream, Thinking: r.Thinking}
	if len(out.Thinking) == 0 && r.ReasoningEffort != "" {
		// effort tier → anthropic thinking budget
		budget := map[string]int{"low": 2048, "medium": 8192, "high": 16384, "xhigh": 24576}[strings.ToLower(r.ReasoningEffort)]
		if budget > 0 {
			out.Thinking = json.RawMessage(fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, budget))
			if out.MaxTokens <= budget {
				out.MaxTokens = budget + 4096
			}
		}
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096 // required field in the anthropic API
	}
	if r.System != "" {
		b, _ := json.Marshal(r.System)
		out.System = b
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			args, _ := json.Marshal(m.Content)
			blocks := []antBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: args}}
			b, _ := json.Marshal(blocks)
			out.Messages = append(out.Messages, antMsg{Role: "user", Content: b})
		case "assistant":
			var blocks []antBlock
			if m.Content != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Args)
				if !json.Valid(input) || len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, antBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, antBlock{Type: "text", Text: ""})
			}
			b, _ := json.Marshal(blocks)
			out.Messages = append(out.Messages, antMsg{Role: "assistant", Content: b})
		default:
			b, _ := json.Marshal(m.Content)
			out.Messages = append(out.Messages, antMsg{Role: "user", Content: b})
		}
	}
	for _, t := range r.Tools {
		schema := t.Params
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out.Tools = append(out.Tools, antTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return json.Marshal(out)
}

type antResp struct {
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	Content    []antBlock `json:"content"`
	StopReason string     `json:"stop_reason"`
	Usage      struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func ParseAnthropicResponse(body []byte) (*Response, error) {
	var in antResp
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad anthropic response: %w", err)
	}
	if in.Error != nil {
		return nil, fmt.Errorf("provider error: %s", in.Error.Message)
	}
	r := &Response{ID: in.ID, Model: in.Model, FinishReason: FinishFromAnthropic(in.StopReason),
		PromptTokens: in.Usage.InputTokens, CompletionTokens: in.Usage.OutputTokens, CachedTokens: in.Usage.CacheReadInputTokens}
	for _, bl := range in.Content {
		switch bl.Type {
		case "text":
			r.Content += bl.Text
		case "tool_use":
			r.ToolCalls = append(r.ToolCalls, ToolCall{ID: bl.ID, Name: bl.Name, Args: string(bl.Input)})
		}
	}
	return r, nil
}

func BuildAnthropicResponse(r *Response) []byte {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	var content []map[string]any
	if r.Content != "" || len(r.ToolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": r.Content})
	}
	for _, tc := range r.ToolCalls {
		input := json.RawMessage(tc.Args)
		if !json.Valid(input) || len(input) == 0 {
			input = json.RawMessage("{}")
		}
		content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": input})
	}
	out := map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": r.Model,
		"content": content, "stop_reason": FinishToAnthropic(r.FinishReason), "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": r.PromptTokens, "output_tokens": r.CompletionTokens},
	}
	b, _ := json.Marshal(out)
	return b
}

// ReadAnthropicStream parses an Anthropic SSE event stream into normalized deltas.
func ReadAnthropicStream(body io.Reader, out chan<- Delta) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	finish := "stop"
	var pt, ct, cached int
	tcIndex := -1
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens          int `json:"input_tokens"`
					OutputTokens         int `json:"output_tokens"`
					CacheReadInputTokens int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			pt = ev.Message.Usage.InputTokens
			cached = ev.Message.Usage.CacheReadInputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				tcIndex++
				out <- Delta{TC: &TCDelta{Index: tcIndex, ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					out <- Delta{Text: ev.Delta.Text}
				}
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" && tcIndex >= 0 {
					out <- Delta{TC: &TCDelta{Index: tcIndex, Args: ev.Delta.PartialJSON}}
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish = FinishFromAnthropic(ev.Delta.StopReason)
			}
			if ev.Usage.OutputTokens > 0 {
				ct = ev.Usage.OutputTokens
			}
		case "error":
			msg := "stream error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			out <- Delta{Err: fmt.Errorf("%s", msg)}
			return
		}
	}
	if err := sc.Err(); err != nil {
		out <- Delta{Err: err}
		return
	}
	out <- Delta{Finish: finish, PromptTokens: pt, CompletionTokens: ct, CachedTokens: cached}
}

// WriteAnthropicStream frames normalized deltas as Anthropic SSE events.
func WriteAnthropicStream(w http.ResponseWriter, model string, in <-chan Delta) error {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	send := func(event string, payload map[string]any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})

	blockIdx := -1        // current anthropic content block index
	textOpen := false     // is a text block open
	curTC := -1           // normalized tool-call index currently open as a block
	closeBlock := func() {
		if blockIdx >= 0 && (textOpen || curTC >= 0) {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIdx})
			textOpen, curTC = false, -1
		}
	}
	finish := "stop"
	var pt, ct int
	for d := range in {
		if d.Err != nil {
			send("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": d.Err.Error()}})
			return d.Err
		}
		if d.Text != "" {
			if !textOpen {
				closeBlock()
				blockIdx++
				textOpen = true
				send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{"type": "text", "text": ""}})
			}
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIdx,
				"delta": map[string]any{"type": "text_delta", "text": d.Text}})
		}
		if d.TC != nil {
			if d.TC.Index != curTC {
				closeBlock()
				blockIdx++
				curTC = d.TC.Index
				name := d.TC.Name
				tcid := d.TC.ID
				if tcid == "" {
					tcid = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), d.TC.Index)
				}
				send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{"type": "tool_use", "id": tcid, "name": name, "input": map[string]any{}}})
			}
			if d.TC.Args != "" {
				send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": d.TC.Args}})
			}
		}
		if d.Finish != "" {
			finish = d.Finish
			pt, ct = d.PromptTokens, d.CompletionTokens
		}
	}
	closeBlock()
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": FinishToAnthropic(finish), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": pt, "output_tokens": ct}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return nil
}
