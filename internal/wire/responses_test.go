package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// outbound: wire.Request -> Responses request body shape
func TestBuildResponsesRequest(t *testing.T) {
	r := &Request{
		Model:           "gpt-5",
		System:          "be terse",
		Messages:        []Msg{{Role: "user", Content: "hi"}},
		MaxTokens:       100,
		ReasoningEffort: "high",
		Tools:           []Tool{{Name: "get_time", Description: "clock", Params: json.RawMessage(`{"type":"object"}`)}},
	}
	b, err := BuildResponsesRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["instructions"] != "be terse" {
		t.Errorf("system not mapped to instructions: %s", b)
	}
	if m["max_output_tokens"].(float64) != 100 {
		t.Errorf("max_output_tokens wrong: %s", b)
	}
	if reasoning, _ := m["reasoning"].(map[string]any); reasoning["effort"] != "high" {
		t.Errorf("reasoning effort not set: %s", b)
	}
	// tools flattened (name at top level, not nested under function)
	tools := m["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "get_time" {
		t.Errorf("tool not flattened: %s", b)
	}
	// input is an item array with a user message carrying input_text
	if !strings.Contains(string(b), `"input_text"`) || !strings.Contains(string(b), `"hi"`) {
		t.Errorf("input item malformed: %s", b)
	}
}

// outbound: Responses response object -> wire.Response
func TestParseResponsesResponse(t *testing.T) {
	body := `{"id":"resp_1","model":"gpt-5","status":"completed",
	  "output":[
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi!"}]},
	    {"type":"function_call","call_id":"call_9","name":"get_time","arguments":"{}"}
	  ],
	  "usage":{"input_tokens":5,"output_tokens":7,"input_tokens_details":{"cached_tokens":2}}}`
	r, err := ParseResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "Hi!" {
		t.Errorf("content wrong: %q", r.Content)
	}
	if r.ReasoningContent != "thinking" {
		t.Errorf("reasoning wrong: %q", r.ReasoningContent)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].ID != "call_9" || r.ToolCalls[0].Name != "get_time" {
		t.Errorf("tool call wrong: %+v", r.ToolCalls)
	}
	if r.FinishReason != "tool_calls" {
		t.Errorf("finish should be tool_calls: %q", r.FinishReason)
	}
	if r.PromptTokens != 5 || r.CompletionTokens != 7 || r.CachedTokens != 2 {
		t.Errorf("usage wrong: %d/%d/%d", r.PromptTokens, r.CompletionTokens, r.CachedTokens)
	}
}

// inbound: Responses request (string + array input) -> wire.Request
func TestParseResponsesRequest(t *testing.T) {
	// string input form
	r, err := ParseResponsesRequest([]byte(`{"model":"gpt-5","instructions":"sys","input":"hello","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.System != "sys" || len(r.Messages) != 1 || r.Messages[0].Content != "hello" || !r.Stream {
		t.Fatalf("string input parse wrong: %+v", r)
	}
	// array input form with a tool result
	r2, err := ParseResponsesRequest([]byte(`{"model":"gpt-5","input":[
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
	  {"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"42"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.Messages) != 3 {
		t.Fatalf("array parse wrong count: %+v", r2.Messages)
	}
	if r2.Messages[1].ToolCalls[0].ID != "c1" {
		t.Errorf("function_call not parsed: %+v", r2.Messages[1])
	}
	if r2.Messages[2].Role != "tool" || r2.Messages[2].Content != "42" {
		t.Errorf("function_call_output not parsed: %+v", r2.Messages[2])
	}
}

// outbound stream: Responses SSE -> normalized deltas
func TestReadResponsesStream(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r"}}`,
		`data: {"type":"response.output_text.delta","delta":"Hel"}`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"f"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"a\":1}"}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":4}}}`,
		"",
	}, "\n\n")
	out := make(chan Delta, 32)
	go ReadResponsesStream(strings.NewReader(sse), out)
	var text, args, finish string
	var tcID, tcName string
	var pt, ct int
	for d := range out {
		if d.Err != nil {
			t.Fatal(d.Err)
		}
		text += d.Text
		if d.TC != nil {
			if d.TC.ID != "" {
				tcID = d.TC.ID
			}
			if d.TC.Name != "" {
				tcName = d.TC.Name
			}
			args += d.TC.Args
		}
		if d.Finish != "" {
			finish, pt, ct = d.Finish, d.PromptTokens, d.CompletionTokens
		}
	}
	if text != "Hello" {
		t.Errorf("text wrong: %q", text)
	}
	if tcID != "c1" || tcName != "f" || args != `{"a":1}` {
		t.Errorf("tool call stream wrong: id=%q name=%q args=%q", tcID, tcName, args)
	}
	if finish != "tool_calls" || pt != 3 || ct != 4 {
		t.Errorf("finish/usage wrong: %q %d %d", finish, pt, ct)
	}
}

// The Responses API names token fields differently from chat completions, so a
// payload read with the chat-completions struct yields a silent 0/0/0. This is
// the shape the raw-passthrough path depends on.
func TestUsageFromResponsesBody(t *testing.T) {
	// non-streaming: usage at the top level
	bare := `{"id":"resp_1","model":"gpt-5.6-luna","usage":{
	  "input_tokens":12000,"output_tokens":345,
	  "input_tokens_details":{"cached_tokens":8192}}}`
	pt, ct, cached, ok := UsageFromResponsesBody([]byte(bare))
	if !ok || pt != 12000 || ct != 345 || cached != 8192 {
		t.Fatalf("bare object = (%d,%d,%d,%v), want (12000,345,8192,true)", pt, ct, cached, ok)
	}

	// streaming: the same object nested inside the response.completed event —
	// an extractor that only looks at the top level records nothing here
	event := `{"type":"response.completed","response":{"id":"resp_1","usage":{
	  "input_tokens":900,"output_tokens":40,
	  "input_tokens_details":{"cached_tokens":128}}}}`
	pt, ct, cached, ok = UsageFromResponsesBody([]byte(event))
	if !ok || pt != 900 || ct != 40 || cached != 128 {
		t.Fatalf("SSE event = (%d,%d,%d,%v), want (900,40,128,true)", pt, ct, cached, ok)
	}

	// events without usage (deltas) must not report a bogus zero-usage hit
	for _, s := range []string{
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"id":"r"}}`,
		`{"usage":{"input_tokens":0,"output_tokens":0}}`,
		`not json`,
	} {
		if _, _, _, ok := UsageFromResponsesBody([]byte(s)); ok {
			t.Errorf("want ok=false for %s", s)
		}
	}

	// a chat-completions body must NOT be mistaken for a Responses one
	chat := `{"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	if _, _, _, ok := UsageFromResponsesBody([]byte(chat)); ok {
		t.Error("chat-completions usage must not satisfy the Responses extractor")
	}
}
