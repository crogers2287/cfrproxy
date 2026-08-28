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
//
// The image fields follow the AI SDK v5 ImagePart the gateway validates
// against: {"type":"image","image":<data-url|base64|https-url>,
// "mediaType":"image/png"}. See alphaImagePart for the live verification.
type alphaContentPart struct {
	Type       string           `json:"type"`
	Text       string           `json:"text,omitempty"`
	Image      string           `json:"image,omitempty"`
	MediaType  string           `json:"mediaType,omitempty"`
	ToolCallID string           `json:"toolCallId,omitempty"`
	ToolName   string           `json:"toolName,omitempty"`
	Input      any              `json:"input,omitempty"`
	Output     *alphaToolOutput `json:"output,omitempty"`
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

// alphaToolChoice is the tool-choice union the gateway accepts:
// {"type":"auto"} | {"type":"any"} | {"type":"tool","name":"<tool>"}.
// Note the field is "name", not the AI SDK's "toolName".
type alphaToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type alphaParams struct {
	Model       string              `json:"model"`
	System      string              `json:"system,omitempty"`
	Messages    []alphaModelMessage `json:"messages"`
	Tools       []alphaTool         `json:"tools,omitempty"`
	ToolChoice  *alphaToolChoice    `json:"tool_choice,omitempty"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stream      bool                `json:"stream"`
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
	msgs, err := alphaModelMessages(r.Messages)
	if err != nil {
		return nil, err
	}
	// Anti-hallucination guard. A vision request that reaches the model with
	// its pictures missing does not fail loudly — the model answers the text
	// alone, fluently and wrongly. Count what the normalized request carried
	// against what the envelope actually says, and refuse to send if a single
	// image went missing anywhere in translation.
	if want := countRequestImages(r.Messages); want > 0 {
		if got := countAlphaImageParts(msgs); got != want {
			return nil, fmt.Errorf("command code alpha: request carries %d image(s) but the envelope has %d image part(s); refusing to send a vision request blind", want, got)
		}
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
			Messages:    msgs,
			Tools:       alphaTools(r.Tools),
			ToolChoice:  alphaToolChoiceOf(r.ToolChoice, r.Tools),
			MaxTokens:   mt,
			Temperature: r.Temperature,
			Stream:      true,
		},
	}
	return json.Marshal(req)
}

func countRequestImages(msgs []Msg) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Images)
	}
	return n
}

func countAlphaImageParts(msgs []alphaModelMessage) int {
	n := 0
	for _, m := range msgs {
		for _, p := range m.Content {
			if p.Type == "image" {
				n++
			}
		}
	}
	return n
}

// alphaModelMessages converts the normalized conversation to Vercel AI SDK
// ModelMessage[]. Assistant tool calls become tool-call parts; tool results
// become their own role:"tool" message with a tool-result part. Images become
// image parts on the user turn that carried them.
//
// It returns an error rather than dropping an image it cannot represent: a
// silently text-only vision request is answered confidently and wrongly, which
// is worse than a failed one.
func alphaModelMessages(msgs []Msg) ([]alphaModelMessage, error) {
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
		parts := make([]alphaContentPart, 0, 1+len(m.Images)+len(m.ToolCalls))
		if m.Content != "" {
			parts = append(parts, alphaContentPart{Type: "text", Text: m.Content})
		}
		// Text first, then images in their original order. The normalized form
		// keeps a message's text as one string and its images as a list, so a
		// client's exact text/image interleaving is already flattened before
		// this point; this reproduces what the normal form actually holds.
		if len(m.Images) > 0 {
			if m.Role != "user" {
				// Verified live: only user turns may carry image parts — an
				// image on an assistant turn is rejected upstream with
				// "The messages do not match the ModelMessage[] schema".
				return nil, fmt.Errorf("command code alpha: %d image(s) on a %q message cannot be represented (alpha accepts images on user messages only)", len(m.Images), m.Role)
			}
			for i, img := range m.Images {
				part, err := alphaImagePart(img)
				if err != nil {
					return nil, fmt.Errorf("command code alpha: image %d of %d on the user message is unusable: %w", i+1, len(m.Images), err)
				}
				parts = append(parts, part)
			}
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
	return out, nil
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
	// error is a bare string on some events and an object
	// ({"type","code","message","statusCode"}) on others — notably the
	// upstream refusals that matter most. Decoding it as a string made the
	// whole line fail to unmarshal, and the reader skips lines it cannot
	// parse: a hard upstream error (e.g. cmd_zdr_no_providers) was therefore
	// swallowed and returned to the client as an empty, successful answer.
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// alphaErrorText pulls a human message out of either error spelling. It never
// returns "" for an error event, so an upstream refusal can never be mistaken
// for an empty-but-successful turn.
func alphaErrorText(ev *alphaEvent) string {
	var s string
	if len(ev.Error) > 0 && json.Unmarshal(ev.Error, &s) == nil && s != "" {
		return s
	}
	var o struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(ev.Error) > 0 && json.Unmarshal(ev.Error, &o) == nil {
		switch {
		case o.Message != "" && o.Code != "":
			return o.Code + ": " + o.Message
		case o.Message != "":
			return o.Message
		case o.Code != "":
			return o.Code
		case o.Type != "":
			return o.Type
		}
	}
	if ev.Message != "" {
		return ev.Message
	}
	if len(ev.Error) > 0 {
		return string(ev.Error)
	}
	return "unknown upstream error"
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
				args := alphaToolInputJSON(ev.Input)
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
			out <- Delta{Err: errors.New(alphaErrorText(&ev))}
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
	deltaSeen := map[string]bool{}
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
			deltaSeen[ev.ID] = true
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
			// The trailing tool-call event repeats what the deltas already
			// said, and it is the repair path when the deltas arrive
			// fragmented mid-token. But it is not always the better copy:
			// when the model is cut off at max_tokens mid-arguments, this
			// event's "input" is a JSON *string* of the partial text, and
			// overwriting with that hands the client double-encoded arguments
			// — json.loads() then yields a string where the schema promised
			// an object. So take whichever copy actually parses.
			cand := alphaToolInputJSON(ev.Input)
			switch {
			case !deltaSeen[id]:
				argsByID[id] = cand // nothing streamed; this is all there is
			case json.Valid([]byte(argsByID[id])):
				// streamed arguments are already well-formed: keep them
			case json.Valid([]byte(cand)):
				argsByID[id] = cand // repair fragmented/corrupt deltas
			default:
				// both are broken (a truncated call): keep the streamed text
				// rather than a double-encoded string of it
			}
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
			return nil, fmt.Errorf("command code error: %s", alphaErrorText(&ev))
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

// ---- images ----
//
// The alpha image schema, verified live against
// POST https://api.commandcode.ai/alpha/generate on 2026-08-28 with
// deepseek/deepseek-v4-flash-vision-exp (each probe asked the model to name the
// shape and colour in a generated PNG, so a 200 alone never counted as a pass):
//
//	{"type":"image","image":"data:image/png;base64,<b64>","mediaType":"image/png"}
//	                                              -> 200, "Red circle."   ACCEPTED
//	{"type":"image","image":"<bare base64>","mediaType":"image/png"}
//	                                              -> 200, "A red circle." ACCEPTED
//	{"type":"image","image":"data:...."}  (no mediaType)
//	                                              -> 200, "A red circle." ACCEPTED
//	{"type":"image","image":"https://…"}          -> schema OK, but the gateway
//	    answers with an upstream error event: "Failed to download image from
//	    https://…" (reproduced on two unrelated public hosts), so a remote URL
//	    validates and then fails to render. Passed through unchanged — the
//	    failure is loud, and inlining it here would mean cfrproxy fetching
//	    arbitrary client-supplied URLs.
//	{"type":"image","url":"data:…"}               -> 400 zod validation error
//	{"type":"image_url","image_url":{"url":…}}    -> 400 zod validation error
//	{"type":"file","data":…,"mediaType":…}        -> 400 zod validation error
//	image part on an assistant message            -> upstream "The messages do
//	    not match the ModelMessage[] schema"
//
// This matches the AI SDK v5 imagePartSchema the gateway validates with
// (z.object({type:"image", image: dataContent|URL, mediaType: string.optional()}),
// user messages only), which is corroborated by the tool-call/tool-result parts
// already in this file being the v5 spelling ("input", "output":{type,value}).

// alphaImagePart converts one normalized image reference into its alpha
// content part. Nothing derived from the payload is placed in the returned
// error: a base64 blob in an error string would land in trace snippets and
// logs, which is exactly where image data must never appear.
func alphaImagePart(img string) (alphaContentPart, error) {
	s := strings.TrimSpace(img)
	switch {
	case s == "":
		return alphaContentPart{}, errors.New("empty image reference")
	case strings.HasPrefix(s, "data:"):
		mediaType, err := validateImageDataURL(s)
		if err != nil {
			return alphaContentPart{}, err
		}
		return alphaContentPart{Type: "image", Image: s, MediaType: mediaType}, nil
	case strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "http://"):
		// media type is unknown until the fetch, and alpha treats it as
		// optional, so it is left off rather than guessed from the extension
		return alphaContentPart{Type: "image", Image: s}, nil
	default:
		// Deliberately not a base64 sniff: a bare payload with no media type
		// is indistinguishable from a mangled URL, and guessing wrong sends a
		// blind request. Callers hand us data URLs or http(s) URLs.
		return alphaContentPart{}, errors.New("unsupported image reference: expected a data: URL or an http(s) URL")
	}
}

// dataURLHeaderMax bounds the scan for the "data:...," header so a malformed
// multi-megabyte string is never walked end to end looking for a comma.
const dataURLHeaderMax = 256

// validateImageDataURL checks a data URL is a well-formed base64 image and
// returns its media type. It validates the payload's alphabet and length in
// place rather than decoding it, so a 10 MB screenshot costs no second copy.
func validateImageDataURL(s string) (string, error) {
	head := s
	if len(head) > dataURLHeaderMax {
		head = head[:dataURLHeaderMax]
	}
	comma := strings.IndexByte(head, ',')
	if comma < 0 {
		return "", errors.New("malformed data URL: no ',' separating header from payload")
	}
	header := s[len("data:"):comma] // e.g. "image/png;base64"
	if !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return "", errors.New("malformed data URL: only ;base64 payloads are supported")
	}
	mediaType := strings.TrimSpace(header[:len(header)-len(";base64")])
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i]) // drop charset etc.
	}
	mediaType = strings.ToLower(mediaType)
	if !strings.HasPrefix(mediaType, "image/") || mediaType == "image/" {
		return "", fmt.Errorf("malformed data URL: media type %q is not an image type", mediaType)
	}
	payload := s[comma+1:]
	if payload == "" {
		return "", errors.New("malformed data URL: empty payload")
	}
	n := 0
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '-', c == '_': // std and url-safe alphabets
			n++
		case c == '=':
			// padding, only valid at the tail
			for j := i; j < len(payload); j++ {
				if payload[j] != '=' {
					return "", errors.New("malformed data URL: base64 padding in the middle of the payload")
				}
			}
			i = len(payload)
		case c == '\n', c == '\r', c == ' ', c == '\t': // tolerated wrapping
		default:
			return "", errors.New("malformed data URL: payload is not valid base64")
		}
	}
	if n == 0 {
		return "", errors.New("malformed data URL: empty payload")
	}
	if n%4 == 1 {
		return "", errors.New("malformed data URL: truncated base64 payload")
	}
	return mediaType, nil
}

// alphaToolChoiceOf maps the client's OpenAI tool_choice onto the union alpha
// accepts. Verified live: params.tool_choice must be an object and its type
// must be one of "auto", "any" or "tool" (with "name"); a bare string, or
// {"type":"required"}, is a 400. OpenAI's "none" has no alpha spelling, so it
// is dropped rather than invented — and with no tools declared there is
// nothing to choose, so the field is omitted entirely.
func alphaToolChoiceOf(raw json.RawMessage, tools []Tool) *alphaToolChoice {
	if len(raw) == 0 || len(tools) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return &alphaToolChoice{Type: "auto"}
		case "required", "any":
			return &alphaToolChoice{Type: "any"}
		default: // "none" and anything unrecognized
			return nil
		}
	}
	var o struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	name := o.Function.Name
	if name == "" {
		name = o.Name
	}
	if name != "" {
		return &alphaToolChoice{Type: "tool", Name: name}
	}
	switch strings.ToLower(strings.TrimSpace(o.Type)) {
	case "any", "required":
		return &alphaToolChoice{Type: "any"}
	case "auto":
		return &alphaToolChoice{Type: "auto"}
	}
	return nil
}

// alphaToolInputJSON renders a tool-call "input" as the arguments JSON an
// OpenAI client expects. A truncated call can arrive as a JSON string holding
// partial JSON; one level of that is unwrapped so the client never receives
// double-encoded arguments.
func alphaToolInputJSON(in json.RawMessage) string {
	s := strings.TrimSpace(string(in))
	if s == "" || s == "null" {
		return "{}"
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if json.Unmarshal(in, &unquoted) == nil {
			if unquoted == "" {
				return "{}"
			}
			return unquoted
		}
	}
	return s
}
