package wire

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ---- Ollama /api/chat dialect (NDJSON streaming) ----

type olMsg struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
}

type olToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // object, not string
	} `json:"function"`
}

type olReq struct {
	Model    string         `json:"model"`
	Messages []olMsg        `json:"messages"`
	Tools    []oaiTool      `json:"tools,omitempty"` // ollama uses the openai tool shape
	Stream   *bool          `json:"stream,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

func ParseOllamaRequest(body []byte) (*Request, error) {
	var in olReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad ollama request: %w", err)
	}
	// ollama streams by default; stream:false must be explicit
	stream := in.Stream == nil || *in.Stream
	r := &Request{Model: in.Model, Stream: stream}
	if v, ok := in.Options["temperature"].(float64); ok {
		r.Temperature = &v
	}
	if v, ok := in.Options["top_p"].(float64); ok {
		r.TopP = &v
	}
	if v, ok := in.Options["num_predict"].(float64); ok {
		r.MaxTokens = int(v)
	}
	for _, m := range in.Messages {
		if m.Role == "system" {
			if r.System != "" {
				r.System += "\n\n"
			}
			r.System += m.Content
			continue
		}
		msg := Msg{Role: m.Role, Content: m.Content}
		for i, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID: fmt.Sprintf("call_%d", i), Name: tc.Function.Name, Args: string(tc.Function.Arguments)})
		}
		r.Messages = append(r.Messages, msg)
	}
	for _, t := range in.Tools {
		r.Tools = append(r.Tools, Tool{Name: t.Function.Name, Description: t.Function.Description, Params: t.Function.Parameters})
	}
	return r, nil
}

func BuildOllamaRequest(r *Request) ([]byte, error) {
	out := olReq{Model: r.Model, Stream: &r.Stream}
	opts := map[string]any{}
	if r.Temperature != nil {
		opts["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		opts["top_p"] = *r.TopP
	}
	if r.MaxTokens > 0 {
		opts["num_predict"] = r.MaxTokens
	}
	if len(r.Stop) > 0 {
		opts["stop"] = r.Stop
	}
	if len(opts) > 0 {
		out.Options = opts
	}
	if r.System != "" {
		out.Messages = append(out.Messages, olMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		om := olMsg{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			om.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			var otc olToolCall
			otc.Function.Name = tc.Name
			args := json.RawMessage(tc.Args)
			if !json.Valid(args) || len(args) == 0 {
				args = json.RawMessage("{}")
			}
			otc.Function.Arguments = args
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

type olResp struct {
	Model           string `json:"model"`
	Message         olMsg  `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func ParseOllamaResponse(body []byte) (*Response, error) {
	var in olResp
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad ollama response: %w", err)
	}
	if in.Error != "" {
		return nil, fmt.Errorf("provider error: %s", in.Error)
	}
	r := &Response{Model: in.Model, Content: in.Message.Content, FinishReason: "stop",
		PromptTokens: in.PromptEvalCount, CompletionTokens: in.EvalCount}
	if in.DoneReason == "length" {
		r.FinishReason = "length"
	}
	for i, tc := range in.Message.ToolCalls {
		r.ToolCalls = append(r.ToolCalls, ToolCall{ID: fmt.Sprintf("call_%d", i), Name: tc.Function.Name, Args: string(tc.Function.Arguments)})
	}
	if len(r.ToolCalls) > 0 {
		r.FinishReason = "tool_calls"
	}
	return r, nil
}

func BuildOllamaResponse(r *Response) []byte {
	msg := olMsg{Role: "assistant", Content: r.Content}
	for _, tc := range r.ToolCalls {
		var otc olToolCall
		otc.Function.Name = tc.Name
		args := json.RawMessage(tc.Args)
		if !json.Valid(args) || len(args) == 0 {
			args = json.RawMessage("{}")
		}
		otc.Function.Arguments = args
		msg.ToolCalls = append(msg.ToolCalls, otc)
	}
	done := "stop"
	if r.FinishReason == "length" {
		done = "length"
	}
	out := map[string]any{
		"model": r.Model, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"message": msg, "done": true, "done_reason": done,
		"prompt_eval_count": r.PromptTokens, "eval_count": r.CompletionTokens,
	}
	b, _ := json.Marshal(out)
	return b
}

// ReadOllamaStream parses an Ollama NDJSON stream into normalized deltas.
func ReadOllamaStream(body io.Reader, out chan<- Delta) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	finish := "stop"
	var pt, ct int
	tcIdx := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk olResp
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			out <- Delta{Err: fmt.Errorf("provider error: %s", chunk.Error)}
			return
		}
		if chunk.Message.Content != "" {
			out <- Delta{Text: chunk.Message.Content}
		}
		for _, tc := range chunk.Message.ToolCalls {
			out <- Delta{TC: &TCDelta{Index: tcIdx, ID: fmt.Sprintf("call_%d", tcIdx), Name: tc.Function.Name, Args: string(tc.Function.Arguments)}}
			tcIdx++
			finish = "tool_calls"
		}
		if chunk.Done {
			if chunk.DoneReason == "length" {
				finish = "length"
			}
			pt, ct = chunk.PromptEvalCount, chunk.EvalCount
		}
	}
	if err := sc.Err(); err != nil {
		out <- Delta{Err: err}
		return
	}
	out <- Delta{Finish: finish, PromptTokens: pt, CompletionTokens: ct}
}

// WriteOllamaStream frames normalized deltas as Ollama NDJSON lines.
func WriteOllamaStream(w http.ResponseWriter, model string, in <-chan Delta) error {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	send := func(payload map[string]any) {
		b, _ := json.Marshal(payload)
		w.Write(b)
		w.Write([]byte("\n"))
		if fl != nil {
			fl.Flush()
		}
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	// ollama has no incremental tool-call framing: buffer fragments per index
	// and emit complete calls at the end.
	tcs := map[int]*ToolCall{}
	order := []int{}
	finish := "stop"
	var pt, ct int
	for d := range in {
		if d.Err != nil {
			send(map[string]any{"error": d.Err.Error()})
			return d.Err
		}
		if d.Text != "" {
			send(map[string]any{"model": model, "created_at": now(),
				"message": map[string]any{"role": "assistant", "content": d.Text}, "done": false})
		}
		if d.TC != nil {
			tc, ok := tcs[d.TC.Index]
			if !ok {
				tc = &ToolCall{}
				tcs[d.TC.Index] = tc
				order = append(order, d.TC.Index)
			}
			if d.TC.ID != "" {
				tc.ID = d.TC.ID
			}
			if d.TC.Name != "" {
				tc.Name = d.TC.Name
			}
			tc.Args += d.TC.Args
		}
		if d.Finish != "" {
			finish = d.Finish
			pt, ct = d.PromptTokens, d.CompletionTokens
		}
	}
	final := map[string]any{"model": model, "created_at": now(), "done": true,
		"done_reason": map[string]string{"length": "length"}[finish],
		"prompt_eval_count": pt, "eval_count": ct}
	if final["done_reason"] == "" {
		final["done_reason"] = "stop"
	}
	msg := map[string]any{"role": "assistant", "content": ""}
	if len(order) > 0 {
		var out []olToolCall
		for _, i := range order {
			var otc olToolCall
			otc.Function.Name = tcs[i].Name
			args := json.RawMessage(tcs[i].Args)
			if !json.Valid(args) || len(args) == 0 {
				args = json.RawMessage("{}")
			}
			otc.Function.Arguments = args
			out = append(out, otc)
		}
		msg["tool_calls"] = out
	}
	final["message"] = msg
	send(final)
	return nil
}
