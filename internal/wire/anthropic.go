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
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`       // tool_result payload
	CacheControl json.RawMessage `json:"cache_control,omitempty"` // prompt-cache breakpoint
}

// cacheEphemeral is an Anthropic prompt-cache breakpoint. Anthropic caches the
// prefix up to and including the marked block, and its hierarchy is
// tools → system → messages, so one breakpoint on the system block covers the
// tool definitions too — the bulk of a stable agent prompt.
var cacheEphemeral = json.RawMessage(`{"type":"ephemeral"}`)

// minCacheableChars is a floor before we bother emitting a breakpoint.
// Anthropic silently ignores caching below ~1024 tokens, so marking a short
// system prompt just adds noise. ~4 chars/token → 4096 chars ≈ 1024 tokens.
const minCacheableChars = 4096

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
	// Claude Code stamps every request of one CLI session (main agent,
	// sub-agents, hooks, web-search sub-requests) with the same session id
	// inside metadata.user_id, a JSON string.
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata,omitempty"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Server tools (web_search_20250305 …) carry a type and no schema; the
	// API executes them itself. Claude Code's WebSearch is one of these.
	Type    string `json:"type,omitempty"`
	MaxUses int    `json:"max_uses,omitempty"`
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
			if bl.Type == "text" && !isBillingHeader(bl.Text) {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// isBillingHeader spots the block Claude Code puts FIRST in its system prompt:
//
//	x-anthropic-billing-header: cc_version=2.1.261.467; cc_entrypoint=cli;
//
// It is metadata for Anthropic's billing, meaningless to any other provider —
// and it carries the CLI version, so it changes with every Claude Code
// update. Because it is the first ~10 tokens of the prefix, keeping it would
// invalidate every prompt cache and every KV-Rosetta artifact for Claude Code
// on the local models each time the CLI updates. Only the translated path is
// affected: an Anthropic-dialect passthrough to an Anthropic-type provider
// forwards the raw bytes and never comes through here.
func isBillingHeader(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), "x-anthropic-billing-header:")
}

func ParseAnthropicRequest(body []byte) (*Request, error) {
	var in antReq
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("bad anthropic request: %w", err)
	}
	r := &Request{Model: in.Model, System: antText(in.System), MaxTokens: in.MaxTokens,
		Temperature: in.Temperature, TopP: in.TopP, Stop: in.Stop, Stream: in.Stream, Thinking: in.Thinking}
	if in.Metadata.UserID != "" {
		var meta struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(in.Metadata.UserID), &meta) == nil && meta.SessionID != "" {
			r.SessionID = meta.SessionID
		}
	}
	// Claude Code's Agent SDK injects system-role messages mid-conversation
	// (session hooks, reminders). They cannot be emitted as system messages in
	// the middle of the list: strict chat templates that require system-first
	// (local models especially) reject that. But folding them into the
	// top-level system — which is what this parser used to do unconditionally —
	// is a prefix-cache catastrophe. The system block renders BEFORE every
	// conversation turn, so appending one new reminder to it changes the token
	// stream ahead of the entire history and forces a full reprefill. Measured
	// on fred: a 25-token volatile field ahead of a 16k prefix took the hit rate
	// from 99.6% to 0.0% and cost 19.8s of redundant prefill.
	//
	// So split by position. A system message that arrives BEFORE any real turn
	// is part of the static preamble: hoisting it is both correct and
	// cache-safe. One that arrives mid-conversation is carried forward and
	// merged into the next user turn, which keeps every byte before it
	// identical and lands the volatile text in a turn that was new anyway.
	seenTurn := false
	var pending []string // mid-conversation system text awaiting the next user turn
	flushInto := func(dst string) string {
		if len(pending) == 0 {
			return dst
		}
		joined := strings.Join(pending, "\n\n")
		pending = nil
		if dst == "" {
			return joined
		}
		return joined + "\n\n" + dst
	}
	for _, m := range in.Messages {
		if m.Role == "system" {
			txt := antText(m.Content)
			if txt == "" {
				continue
			}
			if !seenTurn {
				if r.System != "" {
					r.System += "\n\n"
				}
				r.System += txt
			} else {
				pending = append(pending, txt)
			}
			continue
		}
		seenTurn = true
		var blocks []antBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			// plain string content
			content := antText(m.Content)
			if m.Role == "user" {
				content = flushInto(content)
			}
			r.Messages = append(r.Messages, Msg{Role: m.Role, Content: content})
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
		if msg.Role == "user" {
			msg.Content = flushInto(msg.Content)
		}
		if msg.Content != "" || len(msg.ToolCalls) > 0 {
			r.Messages = append(r.Messages, msg)
		}
	}
	// A trailing system message with no user turn after it has nowhere better
	// to go than the end of the conversation — still after the cached prefix,
	// which is the whole point. Only a request with no turns at all falls back
	// to the system block.
	if len(pending) > 0 {
		trailing := strings.Join(pending, "\n\n")
		pending = nil
		if n := len(r.Messages); n > 0 {
			if c := r.Messages[n-1].Content; c != "" {
				trailing = c + "\n\n" + trailing
			}
			r.Messages[n-1].Content = trailing
		} else if r.System != "" {
			r.System += "\n\n" + trailing
		} else {
			r.System = trailing
		}
	}
	for _, t := range in.Tools {
		if strings.HasPrefix(t.Type, "web_search") {
			// An Anthropic server tool: no provider behind the proxy can run it,
			// so it is not forwarded as a function tool. The proxy emulates it
			// (internal/proxy/websearchtool.go) when the request reaches it.
			r.WebSearch = &WebSearchTool{MaxUses: t.MaxUses}
			continue
		}
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
		// Emit the system prompt as a block array carrying a cache breakpoint
		// so Anthropic caches tools+system instead of re-billing them every
		// turn. Agent harnesses resend a large, byte-identical system prompt on
		// each request, which is exactly what prompt caching is for. Short
		// prompts stay a plain string (Anthropic won't cache them anyway).
		if len(r.System) >= minCacheableChars {
			b, _ := json.Marshal([]antBlock{{Type: "text", Text: r.System, CacheControl: cacheEphemeral}})
			out.System = b
		} else {
			b, _ := json.Marshal(r.System)
			out.System = b
		}
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
	var content []any
	for _, b := range r.Blocks {
		content = append(content, json.RawMessage(b))
	}
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
				Thinking    string `json:"thinking"`
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
			case "thinking_delta":
				// extended-thinking output rides the normalized Reasoning field, so
				// an OpenAI-dialect client sees it as reasoning_content instead of
				// losing it. (Signatures are not carried; thinking is display-only
				// past the proxy.)
				if ev.Delta.Thinking != "" {
					out <- Delta{Reasoning: ev.Delta.Thinking}
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
	var werr error // first failed write to the client; the stream stops there
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	send := func(event string, payload map[string]any) {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil && werr == nil {
			werr = err
		}
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})

	blockIdx := -1     // current anthropic content block index
	textOpen := false  // is a text block open
	thinkOpen := false // is a thinking block open
	curTC := -1        // normalized tool-call index currently open as a block
	// Tool-call identity by normalized index. A tool call's argument
	// fragments are streamed into ONE tool_use block; text that arrives while
	// that block is open is held back and emitted after the block closes,
	// because Anthropic blocks are sequential and a text block in the middle
	// would split the call in two (the second half with a fresh id and no
	// name — a malformed tool call to a Claude-Code-style client).
	type tcIdent struct{ id, name string }
	idents := map[int]tcIdent{}
	var heldText strings.Builder
	closeBlock := func() {
		if blockIdx >= 0 && thinkOpen {
			// the API closes a thinking block with a signature; clients accept
			// an empty one and the proxy drops thinking blocks on the way back in
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIdx,
				"delta": map[string]any{"type": "signature_delta", "signature": ""}})
		}
		if blockIdx >= 0 && (textOpen || thinkOpen || curTC >= 0) {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIdx})
			textOpen, thinkOpen, curTC = false, false, -1
		}
	}
	// Reasoning from a thinking model (deepseek reasoning_content, Anthropic
	// thinking_delta) is forwarded as a thinking block. Dropping it — which
	// this writer used to do — left Claude Code showing "Waiting for API
	// response" for the whole 30-60 s a 150k-context DeepSeek turn thinks.
	emitThinking := func(text string) {
		if text == "" {
			return
		}
		if !thinkOpen {
			closeBlock()
			blockIdx++
			thinkOpen = true
			send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""}})
		}
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text}})
	}
	emitText := func(text string) {
		if text == "" {
			return
		}
		if !textOpen {
			closeBlock()
			blockIdx++
			textOpen = true
			send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIdx,
				"content_block": map[string]any{"type": "text", "text": ""}})
		}
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": text}})
	}
	finish := "stop"
	var pt, ct int
	for d := range in {
		if werr != nil {
			return werr
		}
		if d.Err != nil {
			send("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": d.Err.Error()}})
			return d.Err
		}
		if d.Reasoning != "" && curTC < 0 {
			emitThinking(d.Reasoning)
		}
		if d.Text != "" {
			if curTC >= 0 {
				heldText.WriteString(d.Text)
			} else {
				emitText(d.Text)
			}
		}
		if d.TC != nil {
			id := idents[d.TC.Index]
			if d.TC.ID != "" {
				id.id = d.TC.ID
			}
			if d.TC.Name != "" {
				id.name = d.TC.Name
			}
			if id.id == "" {
				id.id = fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), d.TC.Index)
			}
			idents[d.TC.Index] = id
			if d.TC.Index != curTC {
				closeBlock()
				if heldText.Len() > 0 {
					emitText(heldText.String())
					heldText.Reset()
					closeBlock()
				}
				blockIdx++
				curTC = d.TC.Index
				// A fragment for an index whose block already closed reopens it
				// under the SAME id and name rather than inventing a nameless call.
				send("content_block_start", map[string]any{"type": "content_block_start", "index": blockIdx,
					"content_block": map[string]any{"type": "tool_use", "id": id.id, "name": id.name, "input": map[string]any{}}})
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
	if heldText.Len() > 0 {
		emitText(heldText.String())
		closeBlock()
	}
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": FinishToAnthropic(finish), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": pt, "output_tokens": ct}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return nil
}

// WriteAnthropicOneShotStream emits an already-complete answer as an
// Anthropic SSE stream: pre-built content blocks first (server_tool_use,
// web_search_tool_result …), then the text as one block, then the usage.
// Used by the web-search emulation, whose answer only exists once its tool
// loop has finished.
func WriteAnthropicOneShotStream(w http.ResponseWriter, model string, blocks []json.RawMessage, text string, promptTokens, completionTokens int) error {
	var werr error
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	send := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil && werr == nil {
			werr = err
		}
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": promptTokens, "output_tokens": 0}}})
	idx := 0
	for _, b := range blocks {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": json.RawMessage(b)})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
	for len(text) > 0 {
		n := 2048
		if n > len(text) {
			n = len(text)
		}
		// never split a UTF-8 sequence
		for n < len(text) && n > 0 && text[n]&0xC0 == 0x80 {
			n--
		}
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": text[:n]}})
		text = text[n:]
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": completionTokens}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return werr
}
