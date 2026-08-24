package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

const testPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

// Msg.Content was a bare string, so every translated request silently dropped
// its pictures. That is what forced the proxy to skip cross-dialect vision
// targets entirely — a translated image request would have asked a blind
// question and gotten a confident wrong answer.
func TestParseOpenAIRequestKeepsImages(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"image_url","image_url":{"url":"` + testPNG + `"}}]}]}`)
	r, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 {
		t.Fatalf("messages = %d", len(r.Messages))
	}
	m := r.Messages[0]
	if m.Content != "what is this" {
		t.Errorf("text = %q", m.Content)
	}
	if len(m.Images) != 1 || m.Images[0] != testPNG {
		t.Fatalf("images = %v, want the data URI preserved", m.Images)
	}
}

// The Responses shape nests the url differently: image_url is a bare string on
// an input_image part, not an object.
func TestParseOpenAIRequestKeepsInputImageShape(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"input_text","text":"hi"},
		{"type":"input_image","image_url":"` + testPNG + `"}]}]}`)
	r, err := ParseOpenAIRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Messages[0].Images; len(got) != 1 || got[0] != testPNG {
		t.Fatalf("images = %v", got)
	}
	if r.Messages[0].Content != "hi" {
		t.Errorf("text = %q", r.Messages[0].Content)
	}
}

// codex serves gpt-5.6-* through the Responses API, so this is the exact
// translation that made luna unusable as the primary vision model.
func TestBuildResponsesRequestEmitsInputImage(t *testing.T) {
	r := &Request{Model: "gpt-5.6-luna", Messages: []Msg{
		{Role: "user", Content: "what is this", Images: []string{testPNG}},
	}}
	out, err := BuildResponsesRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"input_image"`) {
		t.Fatalf("no input_image part emitted: %s", s)
	}
	if !strings.Contains(s, testPNG) {
		t.Fatalf("image data did not survive: %s", s)
	}
	if !strings.Contains(s, `"input_text"`) {
		t.Fatalf("text part lost: %s", s)
	}
}

// Translated openai→openai failover must also keep the picture.
func TestBuildOpenAIRequestReEmitsImages(t *testing.T) {
	r := &Request{Model: "m", Messages: []Msg{
		{Role: "user", Content: "what is this", Images: []string{testPNG}},
	}}
	out, err := BuildOpenAIRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	// Round-trip it back through the parser: the image must still be there.
	back, err := ParseOpenAIRequest(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Messages[0].Images) != 1 || back.Messages[0].Images[0] != testPNG {
		t.Fatalf("image lost on round trip: %v", back.Messages[0].Images)
	}
	if back.Messages[0].Content != "what is this" {
		t.Fatalf("text lost on round trip: %q", back.Messages[0].Content)
	}
}

// A text-only message must keep emitting a plain string, or every existing
// provider suddenly receives a content-part array it did not before.
func TestBuildOpenAIRequestLeavesTextOnlyMessagesAlone(t *testing.T) {
	r := &Request{Model: "m", Messages: []Msg{{Role: "user", Content: "plain"}}}
	out, err := BuildOpenAIRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"content":"plain"`) {
		t.Fatalf("text-only message should stay a bare string: %s", out)
	}
}

// The Responses API requires "output" on every function_call_output item, even
// when the tool returned nothing. With `omitempty` on a plain string an empty
// result dropped the key and the WHOLE request 400d with
// `Missing required parameter: input[N].output` — one silent tool result
// poisoning an otherwise valid conversation.
func TestBuildResponsesRequestAlwaysEmitsToolOutput(t *testing.T) {
	r := &Request{Model: "gpt-5.6-sol", Messages: []Msg{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "f", Args: "{}"}}},
		{Role: "tool", ToolCallID: "call_1", Content: ""}, // tool returned nothing
	}}
	out, err := BuildResponsesRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range body.Input {
		var typ string
		json.Unmarshal(it["type"], &typ)
		if typ != "function_call_output" {
			continue
		}
		found = true
		if _, ok := it["output"]; !ok {
			t.Fatalf(`function_call_output is missing "output": %s`, out)
		}
		var got string
		if err := json.Unmarshal(it["output"], &got); err != nil {
			t.Fatalf("output not a string: %v", err)
		}
		if got != "" {
			t.Fatalf("output = %q, want empty string", got)
		}
	}
	if !found {
		t.Fatalf("no function_call_output emitted: %s", out)
	}
}

// Non-tool items must NOT carry an output key.
func TestBuildResponsesRequestOmitsOutputElsewhere(t *testing.T) {
	r := &Request{Model: "m", Messages: []Msg{{Role: "user", Content: "hi"}}}
	out, err := BuildResponsesRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"output"`) {
		t.Fatalf("plain message should not carry output: %s", out)
	}
}

// A round trip through the parser must survive a nil output (a provider that
// omitted the key) without panicking.
func TestParseResponsesRequestToleratesMissingOutput(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"function_call_output","call_id":"c1"}]}`)
	r, err := ParseResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != "tool" || r.Messages[0].Content != "" {
		t.Fatalf("unexpected parse: %+v", r.Messages)
	}
}
