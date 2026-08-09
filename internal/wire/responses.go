package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- OpenAI Responses API dialect (/v1/responses) ----
//
// The Responses API is a different shape from chat completions:
//   - request:  `input` (string or item array) + top-level `instructions`
//               (system), flattened `tools`, `reasoning:{effort}`,
//               `max_output_tokens`.
//   - response: `output` array of typed items (message / function_call /
//               reasoning) with `usage:{input_tokens,output_tokens}`.
//   - stream:   typed SSE events (response.output_text.delta,
//               response.function_call_arguments.delta, response.completed …).
//
// This file translates all three directions to/from the normalized wire.Request
// / wire.Response / wire.Delta, mirroring openai.go so the proxy can treat
// "responses" as just another provider dialect.

// ---------- request ----------

type respFuncTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type respReq struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Tools           []respFuncTool  `json:"tools,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Reasoning       map[string]any  `json:"reasoning,omitempty"`
}

// a single input item (message | function_call | function_call_output)
type respInputItem struct {
	Type    string          `json:"type,omitempty"` // message | function_call | function_call_output
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call_output
	Output string `json:"output,omitempty"`
}

type respContentPart struct {
	Type string `json:"type"` // input_text | output_text | input_image
	Text string `json:"text,omitempty"`
	// ImageURL is a data URI or https URL. The Responses API takes it as a
	// bare string on an input_image part, unlike chat completions which nests
	// it in an object.
	ImageURL string `json:"image_url,omitempty"`
}

// respContentText flattens a Responses content field (string or part array).
func respContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []respContentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			// input_text (user) and output_text (assistant) both carry text
			if strings.HasSuffix(p.Type, "_text") {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ParseResponsesRequest parses an inbound /v1/responses request into wire.Request.
func ParseResponsesRequest(body []byte) (*Request, error) {
	var in respReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad responses request: %w", err)
	}
	r := &Request{
		Model:       in.Model,
		System:      in.Instructions,
		MaxTokens:   in.MaxOutputTokens,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if in.Reasoning != nil {
		if eff, ok := in.Reasoning["effort"].(string); ok {
			r.ReasoningEffort = eff
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // only function tools translate to the common form
		}
		r.Tools = append(r.Tools, Tool{Name: t.Name, Description: t.Description, Params: t.Parameters})
	}
	// input: string (single user turn) or array of items
	if len(in.Input) > 0 {
		var s string
		if json.Unmarshal(in.Input, &s) == nil {
			r.Messages = append(r.Messages, Msg{Role: "user", Content: s})
		} else {
			var items []respInputItem
			if err := json.Unmarshal(in.Input, &items); err == nil {
				for _, it := range items {
					switch it.Type {
					case "function_call":
						r.Messages = append(r.Messages, Msg{Role: "assistant",
							ToolCalls: []ToolCall{{ID: it.CallID, Name: it.Name, Args: it.Arguments}}})
					case "function_call_output":
						r.Messages = append(r.Messages, Msg{Role: "tool", ToolCallID: it.CallID, Content: it.Output})
					default: // "message" or bare {role,content}
						role := it.Role
						if role == "" {
							role = "user"
						}
						if role == "system" || role == "developer" {
							if r.System != "" {
								r.System += "\n\n"
							}
							r.System += respContentText(it.Content)
							continue
						}
						r.Messages = append(r.Messages, Msg{Role: role, Content: respContentText(it.Content)})
					}
				}
			}
		}
	}
	return r, nil
}

// BuildResponsesRequest renders a wire.Request as a /v1/responses body.
func BuildResponsesRequest(r *Request) ([]byte, error) {
	out := respReq{
		Model:           r.Model,
		Instructions:    r.System,
		MaxOutputTokens: r.MaxTokens,
		Temperature:     r.Temperature,
		TopP:            r.TopP,
		Stream:          r.Stream,
	}
	effort := r.ReasoningEffort
	if effort == "" && len(r.Thinking) > 0 {
		var th struct {
			BudgetTokens int `json:"budget_tokens"`
		}
		json.Unmarshal(r.Thinking, &th)
		switch {
		case th.BudgetTokens <= 0:
		case th.BudgetTokens <= 2048:
			effort = "low"
		case th.BudgetTokens <= 8192:
			effort = "medium"
		default:
			effort = "high"
		}
	}
	if effort != "" {
		out.Reasoning = map[string]any{"effort": effort}
	}
	for _, t := range r.Tools {
		out.Tools = append(out.Tools, respFuncTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Params})
	}
	var items []respInputItem
	for _, m := range r.Messages {
		switch m.Role {
		case "tool":
			items = append(items, respInputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		case "assistant":
			if m.Content != "" {
				parts, _ := json.Marshal([]respContentPart{{Type: "output_text", Text: m.Content}})
				items = append(items, respInputItem{Type: "message", Role: "assistant", Content: parts})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, respInputItem{Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: tc.Args})
			}
		default: // user
			cps := []respContentPart{{Type: "input_text", Text: m.Content}}
			// Images ride alongside the text so a vision request keeps its
			// picture through translation instead of being silently reduced to
			// a blind question.
			for _, u := range m.Images {
				cps = append(cps, respContentPart{Type: "input_image", ImageURL: u})
			}
			parts, _ := json.Marshal(cps)
			items = append(items, respInputItem{Type: "message", Role: "user", Content: parts})
		}
	}
	b, _ := json.Marshal(items)
	out.Input = b
	return json.Marshal(out)
}

// ---------- response ----------

type respObj struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"` // message | function_call | reasoning
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
		// function_call
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *responsesUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// responsesUsage is the /v1/responses token accounting block. Note the field
// names differ from chat completions (`input_tokens`, not `prompt_tokens`, and
// `input_tokens_details.cached_tokens`, not `prompt_tokens_details…`) — reading
// a Responses payload with the chat-completions struct silently yields zeros.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// UsageFromResponsesBody extracts token usage from a /v1/responses payload
// without parsing the rest of it, for the raw-passthrough path which otherwise
// never decodes the body.
//
// Accepts BOTH shapes: the bare response object (non-streaming) and the SSE
// event envelope that wraps it, where `response.completed` carries the same
// object under "response". A caller handed an SSE line would otherwise find no
// top-level "usage" and record nothing.
func UsageFromResponsesBody(body []byte) (pt, ct, cached int, ok bool) {
	var v struct {
		Usage    *responsesUsage `json:"usage"`
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &v) != nil {
		return 0, 0, 0, false
	}
	u := v.Usage
	if u == nil && v.Response != nil {
		u = v.Response.Usage
	}
	if u == nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return 0, 0, 0, false
	}
	return u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, true
}

// ParseResponsesResponse parses an upstream /v1/responses object into wire.Response.
func ParseResponsesResponse(body []byte) (*Response, error) {
	var in respObj
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad responses response: %w", err)
	}
	if in.Error != nil {
		return nil, fmt.Errorf("provider error: %s", in.Error.Message)
	}
	r := &Response{ID: in.ID, Model: in.Model, FinishReason: "stop"}
	if in.Usage != nil {
		r.PromptTokens = in.Usage.InputTokens
		r.CompletionTokens = in.Usage.OutputTokens
		r.CachedTokens = in.Usage.InputTokensDetails.CachedTokens
	}
	var text, reasoning strings.Builder
	for _, item := range in.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if strings.HasSuffix(c.Type, "_text") {
					text.WriteString(c.Text)
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				reasoning.WriteString(s.Text)
			}
		case "function_call":
			r.ToolCalls = append(r.ToolCalls, ToolCall{ID: item.CallID, Name: item.Name, Args: item.Arguments})
		}
	}
	r.Content = text.String()
	r.ReasoningContent = reasoning.String()
	if len(r.ToolCalls) > 0 {
		r.FinishReason = "tool_calls"
	} else if in.IncompleteDetails != nil && in.IncompleteDetails.Reason == "max_output_tokens" {
		r.FinishReason = "length"
	}
	return r, nil
}

// BuildResponsesResponse renders a wire.Response as a /v1/responses object (for
// clients that call the proxy's inbound /v1/responses endpoint, non-streaming).
func BuildResponsesResponse(r *Response) []byte {
	id := r.ID
	if id == "" {
		id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	var output []map[string]any
	if r.ReasoningContent != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_" + id,
			"summary": []map[string]any{{"type": "summary_text", "text": r.ReasoningContent}},
		})
	}
	if r.Content != "" || len(r.ToolCalls) == 0 {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + id, "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": r.Content}},
		})
	}
	for i, tc := range r.ToolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": fmt.Sprintf("fc_%s_%d", id, i),
			"call_id": tc.ID, "name": tc.Name, "arguments": tc.Args, "status": "completed",
		})
	}
	out := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"model": r.Model, "status": "completed", "output": output,
		"usage": map[string]any{
			"input_tokens": r.PromptTokens, "output_tokens": r.CompletionTokens,
			"total_tokens": r.PromptTokens + r.CompletionTokens,
		},
	}
	b, _ := json.Marshal(out)
	return b
}

// ---------- streaming (upstream → normalized) ----------

// ReadResponsesStream parses a Responses API SSE stream into normalized deltas.
func ReadResponsesStream(body io.Reader, out chan<- Delta) {
	defer close(out)
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	// output_index → tool-call bookkeeping (function_call items)
	toolIdx := map[int]bool{}
	finish := ""
	var pt, ct, cached int
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // ignore `event:` and blank lines; the JSON carries `type`
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type        string `json:"type"`
			Delta       string `json:"delta"`
			OutputIndex int    `json:"output_index"`
			Item        struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
			Response *respObj `json:"response"`
		}
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			if ev.Delta != "" {
				out <- Delta{Text: ev.Delta}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if ev.Delta != "" {
				out <- Delta{Reasoning: ev.Delta}
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				toolIdx[ev.OutputIndex] = true
				finish = "tool_calls"
				out <- Delta{TC: &TCDelta{Index: ev.OutputIndex, ID: ev.Item.CallID, Name: ev.Item.Name}}
			}
		case "response.function_call_arguments.delta":
			if ev.Delta != "" {
				out <- Delta{TC: &TCDelta{Index: ev.OutputIndex, Args: ev.Delta}}
			}
		case "response.completed", "response.incomplete":
			if ev.Response != nil {
				if ev.Response.Usage != nil {
					pt = ev.Response.Usage.InputTokens
					ct = ev.Response.Usage.OutputTokens
					cached = ev.Response.Usage.InputTokensDetails.CachedTokens
				}
				if finish != "tool_calls" && ev.Response.IncompleteDetails != nil &&
					ev.Response.IncompleteDetails.Reason == "max_output_tokens" {
					finish = "length"
				}
			}
		case "response.failed", "error":
			msg := "responses stream error"
			if ev.Response != nil && ev.Response.Error != nil {
				msg = ev.Response.Error.Message
			}
			out <- Delta{Err: fmt.Errorf("%s", msg)}
			return
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

// ---------- streaming (normalized → Responses SSE, inbound endpoint) ----------

// WriteResponsesStream frames normalized deltas as Responses API SSE events for
// clients that called the proxy's inbound /v1/responses endpoint with stream.
func WriteResponsesStream(w http.ResponseWriter, model string, in <-chan Delta) error {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	seq := 0
	send := func(typ string, payload map[string]any) {
		payload["type"] = typ
		payload["sequence_number"] = seq
		seq++
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
		if fl != nil {
			fl.Flush()
		}
	}
	baseResp := func(status string) map[string]any {
		return map[string]any{"id": id, "object": "response", "status": status, "model": model, "output": []any{}}
	}
	send("response.created", map[string]any{"response": baseResp("in_progress")})

	outIdx := 0
	msgOpen := false
	msgIdx := 0
	var fullText strings.Builder
	// tool-call state per normalized index
	type tcState struct {
		outIdx int
		callID string
		name   string
		args   strings.Builder
	}
	tools := map[int]*tcState{}
	var toolOrder []int
	var pt, ct, cached int

	openMsg := func() {
		if msgOpen {
			return
		}
		msgOpen = true
		msgIdx = outIdx
		outIdx++
		send("response.output_item.added", map[string]any{
			"output_index": msgIdx,
			"item":         map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "in_progress", "content": []any{}},
		})
		send("response.content_part.added", map[string]any{
			"output_index": msgIdx, "item_id": "msg_" + id, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": ""},
		})
	}
	closeMsg := func() {
		if !msgOpen {
			return
		}
		send("response.output_text.done", map[string]any{
			"output_index": msgIdx, "item_id": "msg_" + id, "content_index": 0, "text": fullText.String(),
		})
		send("response.content_part.done", map[string]any{
			"output_index": msgIdx, "item_id": "msg_" + id, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": fullText.String()},
		})
		send("response.output_item.done", map[string]any{
			"output_index": msgIdx,
			"item": map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": fullText.String()}}},
		})
		msgOpen = false
	}

	for d := range in {
		if d.Err != nil {
			return d.Err
		}
		if d.Text != "" {
			openMsg()
			fullText.WriteString(d.Text)
			send("response.output_text.delta", map[string]any{
				"output_index": msgIdx, "item_id": "msg_" + id, "content_index": 0, "delta": d.Text,
			})
		}
		if d.TC != nil {
			// a tool call closes any open text message
			closeMsg()
			st := tools[d.TC.Index]
			if st == nil {
				st = &tcState{outIdx: outIdx}
				outIdx++
				tools[d.TC.Index] = st
				toolOrder = append(toolOrder, d.TC.Index)
			}
			if d.TC.ID != "" {
				st.callID = d.TC.ID
			}
			if d.TC.Name != "" {
				st.name = d.TC.Name
			}
			// The upstream emits id+name once (output_item.added), then arg-only
			// deltas; mirror that — open the function_call item on the first
			// delta that carries id/name, stream arguments after.
			if d.TC.Name != "" || d.TC.ID != "" {
				fcID := "fc_" + id + "_" + strconv.Itoa(d.TC.Index)
				send("response.output_item.added", map[string]any{
					"output_index": st.outIdx,
					"item":         map[string]any{"type": "function_call", "id": fcID, "call_id": st.callID, "name": st.name, "arguments": ""},
				})
			}
			if d.TC.Args != "" {
				st.args.WriteString(d.TC.Args)
				send("response.function_call_arguments.delta", map[string]any{
					"output_index": st.outIdx, "item_id": "fc_" + id + "_" + strconv.Itoa(d.TC.Index), "delta": d.TC.Args,
				})
			}
		}
		if d.Finish != "" {
			pt, ct, cached = d.PromptTokens, d.CompletionTokens, d.CachedTokens
			closeMsg()
			// finalize tool-call items
			for _, idx := range toolOrder {
				st := tools[idx]
				fcID := "fc_" + id + "_" + strconv.Itoa(idx)
				send("response.function_call_arguments.done", map[string]any{
					"output_index": st.outIdx, "item_id": fcID, "arguments": st.args.String(),
				})
				send("response.output_item.done", map[string]any{
					"output_index": st.outIdx,
					"item":         map[string]any{"type": "function_call", "id": fcID, "call_id": st.callID, "name": st.name, "arguments": st.args.String(), "status": "completed"},
				})
			}
			// assemble final output for response.completed
			var output []map[string]any
			if fullText.Len() > 0 {
				output = append(output, map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "completed",
					"content": []map[string]any{{"type": "output_text", "text": fullText.String()}}})
			}
			for _, idx := range toolOrder {
				st := tools[idx]
				output = append(output, map[string]any{"type": "function_call", "id": "fc_" + id + "_" + strconv.Itoa(idx),
					"call_id": st.callID, "name": st.name, "arguments": st.args.String(), "status": "completed"})
			}
			final := baseResp("completed")
			final["output"] = output
			final["usage"] = map[string]any{
				"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct,
				"input_tokens_details": map[string]any{"cached_tokens": cached},
			}
			send("response.completed", map[string]any{"response": final})
		}
	}
	return nil
}
