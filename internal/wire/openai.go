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

// ---- OpenAI chat-completions dialect ----

type oaiMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaiReq struct {
	Model       string          `json:"model"`
	Messages    []oaiMsg        `json:"messages"`
	Tools       []oaiTool       `json:"tools,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	StreamOpts  map[string]any  `json:"stream_options,omitempty"`
	ReasoningEffort string      `json:"reasoning_effort,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// oaiContentText flattens OpenAI content (string or content-part array) to text.
func oaiContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func ParseOpenAIRequest(body []byte) (*Request, error) {
	var in oaiReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad openai request: %w", err)
	}
	r := &Request{Model: in.Model, MaxTokens: in.MaxTokens, Temperature: in.Temperature, TopP: in.TopP, Stream: in.Stream, ReasoningEffort: in.ReasoningEffort}
	if len(in.Stop) > 0 {
		var one string
		if json.Unmarshal(in.Stop, &one) == nil {
			r.Stop = []string{one}
		} else {
			json.Unmarshal(in.Stop, &r.Stop)
		}
	}
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			if r.System != "" {
				r.System += "\n\n"
			}
			r.System += oaiContentText(m.Content)
		default:
			msg := Msg{Role: m.Role, Content: oaiContentText(m.Content), ToolCallID: m.ToolCallID}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
			}
			r.Messages = append(r.Messages, msg)
		}
	}
	for _, t := range in.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, Params: t.Function.Parameters})
	}
	return r, nil
}

func BuildOpenAIRequest(r *Request) ([]byte, error) {
	out := oaiReq{Model: r.Model, MaxTokens: r.MaxTokens, Temperature: r.Temperature, TopP: r.TopP, Stream: r.Stream, ReasoningEffort: r.ReasoningEffort}
	if out.ReasoningEffort == "" && len(r.Thinking) > 0 {
		// anthropic thinking budget → effort tier
		var th struct {
			BudgetTokens int `json:"budget_tokens"`
		}
		json.Unmarshal(r.Thinking, &th)
		switch {
		case th.BudgetTokens <= 0:
		case th.BudgetTokens <= 2048:
			out.ReasoningEffort = "low"
		case th.BudgetTokens <= 8192:
			out.ReasoningEffort = "medium"
		default:
			out.ReasoningEffort = "high"
		}
	}
	if r.Stream {
		out.StreamOpts = map[string]any{"include_usage": true}
	}
	if len(r.Stop) > 0 {
		b, _ := json.Marshal(r.Stop)
		out.Stop = b
	}
	if r.System != "" {
		c, _ := json.Marshal(r.System)
		out.Messages = append(out.Messages, oaiMsg{Role: "system", Content: c})
	}
	for _, m := range r.Messages {
		om := oaiMsg{Role: m.Role, ToolCallID: m.ToolCallID}
		if m.Content != "" || len(m.ToolCalls) == 0 {
			c, _ := json.Marshal(m.Content)
			om.Content = c
		}
		for _, tc := range m.ToolCalls {
			otc := oaiToolCall{ID: tc.ID, Type: "function"}
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Args
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		out.Messages = append(out.Messages, om)
	}
	for _, t := range r.Tools {
		ot := oaiTool{Type: "function"}
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Params
		out.Tools = append(out.Tools, ot)
	}
	return json.Marshal(out)
}

type oaiResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []oaiToolCall   `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func ParseOpenAIResponse(body []byte) (*Response, error) {
	var in oaiResp
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad openai response: %w", err)
	}
	if in.Error != nil {
		return nil, fmt.Errorf("provider error: %s", in.Error.Message)
	}
	r := &Response{ID: in.ID, Model: in.Model, PromptTokens: in.Usage.PromptTokens, CompletionTokens: in.Usage.CompletionTokens, CachedTokens: in.Usage.PromptTokensDetails.CachedTokens, FinishReason: "stop"}
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		r.Content = oaiContentText(c.Message.Content)
		if c.FinishReason != "" {
			r.FinishReason = c.FinishReason
		}
		for _, tc := range c.Message.ToolCalls {
			r.ToolCalls = append(r.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
	}
	return r, nil
}

func BuildOpenAIResponse(r *Response) []byte {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	msg := map[string]any{"role": "assistant", "content": r.Content}
	if len(r.ToolCalls) > 0 {
		var tcs []map[string]any
		for _, tc := range r.ToolCalls {
			tcs = append(tcs, map[string]any{"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Name, "arguments": tc.Args}})
		}
		msg["tool_calls"] = tcs
		msg["content"] = nil
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": r.Model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": r.FinishReason}},
		"usage": map[string]any{"prompt_tokens": r.PromptTokens, "completion_tokens": r.CompletionTokens,
			"total_tokens": r.PromptTokens + r.CompletionTokens},
	}
	b, _ := json.Marshal(out)
	return b
}

// ReadOpenAIStream parses an OpenAI SSE stream into normalized deltas.
func ReadOpenAIStream(body io.Reader, out chan<- Delta) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	finish := ""
	var pt, ct, cached int
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string        `json:"content"`
					ToolCalls []oaiToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			pt, ct, cached = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.PromptTokensDetails.CachedTokens
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				out <- Delta{Text: c.Delta.Content}
			}
			for _, tc := range c.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				out <- Delta{TC: &TCDelta{Index: idx, ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments}}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		out <- Delta{Err: err}
		return
	}
	if finish == "" {
		finish = "stop"
	}
	out <- Delta{Finish: finish, PromptTokens: pt, CompletionTokens: ct, CachedTokens: cached}
}

// WriteOpenAIStream frames normalized deltas as OpenAI SSE chunks.
func WriteOpenAIStream(w http.ResponseWriter, model string, in <-chan Delta) error {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	send := func(payload map[string]any) {
		b, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{payload},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	send(map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil})
	for d := range in {
		if d.Err != nil {
			return d.Err
		}
		if d.Text != "" {
			send(map[string]any{"index": 0, "delta": map[string]any{"content": d.Text}, "finish_reason": nil})
		}
		if d.TC != nil {
			f := map[string]any{}
			if d.TC.Name != "" {
				f["name"] = d.TC.Name
			}
			if d.TC.Args != "" {
				f["arguments"] = d.TC.Args
			}
			tc := map[string]any{"index": d.TC.Index, "type": "function", "function": f}
			if d.TC.ID != "" {
				tc["id"] = d.TC.ID
			}
			send(map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{tc}}, "finish_reason": nil})
		}
		if d.Finish != "" {
			send(map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": d.Finish})
			b, _ := json.Marshal(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{},
				"usage": map[string]any{"prompt_tokens": d.PromptTokens, "completion_tokens": d.CompletionTokens,
					"total_tokens": d.PromptTokens + d.CompletionTokens},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
	return nil
}
