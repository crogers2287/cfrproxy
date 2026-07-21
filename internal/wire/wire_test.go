package wire

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthropic request in → normal → openai request out (Claude Code → OpenAI provider)
func TestAnthropicToOpenAI(t *testing.T) {
	in := []byte(`{
		"model":"prov/gpt-x","max_tokens":100,"system":"be terse",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"Tampa"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"72F"}]}
		],
		"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object"}}]
	}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "be terse" || len(req.Messages) != 3 || len(req.Tools) != 1 {
		t.Fatalf("parse: system=%q msgs=%d tools=%d", req.System, len(req.Messages), len(req.Tools))
	}
	if req.Messages[1].ToolCalls[0].Name != "get_weather" {
		t.Fatalf("tool call lost: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "tu1" {
		t.Fatalf("tool result not normalized: %+v", req.Messages[2])
	}
	out, err := BuildOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var oai map[string]any
	json.Unmarshal(out, &oai)
	msgs := oai["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system message missing: %v", msgs[0])
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "tu1" {
		t.Errorf("openai tool result wrong: %v", last)
	}
}

// openai request in → normal → anthropic request out (Codex → Claude provider)
func TestOpenAIToAnthropic(t *testing.T) {
	in := []byte(`{
		"model":"claude/sonnet","messages":[
			{"role":"system","content":"sys"},
			{"role":"user","content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]
	}`)
	req, err := ParseOpenAIRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "sys" || req.Messages[0].Content != "part1 part2" {
		t.Fatalf("parse: %+v", req)
	}
	out, err := BuildAnthropicRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var ant map[string]any
	json.Unmarshal(out, &ant)
	if ant["max_tokens"].(float64) <= 0 {
		t.Error("anthropic max_tokens must be set")
	}
	msgs := ant["messages"].([]any)
	// assistant tool_use then user tool_result
	asst := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if asst["type"] != "tool_use" || asst["name"] != "f" {
		t.Errorf("tool_use block wrong: %v", asst)
	}
	tr := msgs[2].(map[string]any)
	if tr["role"] != "user" {
		t.Errorf("tool_result should be user role: %v", tr)
	}
}

// Claude Code Agent SDK injects mid-conversation system messages; they must
// fold into the top-level system so system-first chat templates accept the
// converted request.
func TestAnthropicMidConversationSystem(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"system","content":"hook output"},
			{"role":"user","content":[{"type":"text","text":"again"}]}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "top\n\nhook output" {
		t.Errorf("system not folded: %q", req.System)
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			t.Fatalf("system role leaked into messages: %+v", req.Messages)
		}
	}
	out, _ := BuildOpenAIRequest(req)
	var oai map[string]any
	json.Unmarshal(out, &oai)
	msgs := oai["messages"].([]any)
	for i, m := range msgs {
		if m.(map[string]any)["role"] == "system" && i != 0 {
			t.Errorf("system message at index %d", i)
		}
	}
}

func TestOllamaRoundtrip(t *testing.T) {
	in := []byte(`{"model":"local/llama3","messages":[{"role":"system","content":"s"},{"role":"user","content":"q"}],"options":{"temperature":0.5},"stream":false}`)
	req, err := ParseOllamaRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.Stream || req.System != "s" || *req.Temperature != 0.5 {
		t.Fatalf("parse: %+v", req)
	}
	// default is streaming when field omitted
	req2, _ := ParseOllamaRequest([]byte(`{"model":"m","messages":[]}`))
	if !req2.Stream {
		t.Error("ollama should default to streaming")
	}
	body, _ := BuildOllamaRequest(req)
	var ol map[string]any
	json.Unmarshal(body, &ol)
	if ol["options"].(map[string]any)["temperature"] != 0.5 {
		t.Errorf("options lost: %v", ol)
	}
}

// stream re-framing: openai SSE chunks → normalized → anthropic events
func TestStreamOpenAIToAnthropic(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
		`data: [DONE]`,
	}, "\n\n")
	deltas := make(chan Delta, 16)
	go ReadOpenAIStream(strings.NewReader(sse), deltas)
	rec := httptest.NewRecorder()
	if err := WriteAnthropicStream(rec, "m", deltas); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", `"text":"Hel"`, `"type":"text_delta"`, `"text":"lo"`, `"stop_reason":"end_turn"`, "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("anthropic stream missing %q in:\n%s", want, out)
		}
	}
}

// stream re-framing: anthropic events → normalized → openai chunks, with tool calls
func TestStreamAnthropicToOpenAI(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`, ``,
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu9","name":"run"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`, ``,
		`event: content_block_stop`, `data: {"type":"content_block_stop","index":0}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n")
	deltas := make(chan Delta, 16)
	go ReadAnthropicStream(strings.NewReader(sse), deltas)
	rec := httptest.NewRecorder()
	if err := WriteOpenAIStream(rec, "m", deltas); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{`"name":"run"`, `{\"x\":`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("openai stream missing %q in:\n%s", want, out)
		}
	}
}

// normalized deltas with tool call → ollama NDJSON buffers args to completion
func TestStreamToOllama(t *testing.T) {
	deltas := make(chan Delta, 8)
	deltas <- Delta{Text: "thinking"}
	deltas <- Delta{TC: &TCDelta{Index: 0, ID: "c1", Name: "f", Args: `{"a"`}}
	deltas <- Delta{TC: &TCDelta{Index: 0, Args: `:2}`}}
	deltas <- Delta{Finish: "tool_calls", PromptTokens: 4, CompletionTokens: 6}
	close(deltas)
	rec := httptest.NewRecorder()
	if err := WriteOllamaStream(rec, "m", deltas); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	var last map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if last["done"] != true {
		t.Fatalf("final line not done: %v", last)
	}
	tcs := last["message"].(map[string]any)["tool_calls"].([]any)
	args := tcs[0].(map[string]any)["function"].(map[string]any)["arguments"].(map[string]any)
	if args["a"] != 2.0 {
		t.Errorf("buffered tool args wrong: %v", args)
	}
}

func TestBuildResponses(t *testing.T) {
	r := &Response{Model: "m", Content: "hi", FinishReason: "stop", PromptTokens: 1, CompletionTokens: 2}
	var oai, ant, ol map[string]any
	json.Unmarshal(BuildOpenAIResponse(r), &oai)
	json.Unmarshal(BuildAnthropicResponse(r), &ant)
	json.Unmarshal(BuildOllamaResponse(r), &ol)
	if oai["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "hi" {
		t.Error("openai response wrong")
	}
	if ant["stop_reason"] != "end_turn" {
		t.Error("anthropic response wrong")
	}
	if ol["message"].(map[string]any)["content"] != "hi" || ol["done"] != true {
		t.Error("ollama response wrong")
	}
}

// reasoning controls survive translation in both directions
func TestReasoningPreserved(t *testing.T) {
	req, err := ParseOpenAIRequest([]byte(`{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || req.ReasoningEffort != "high" {
		t.Fatalf("effort not parsed: %v %+v", err, req)
	}
	out, _ := BuildOpenAIRequest(req)
	if !strings.Contains(string(out), `"reasoning_effort":"high"`) {
		t.Errorf("effort dropped openai->openai: %s", out)
	}
	ant, _ := BuildAnthropicRequest(req)
	if !strings.Contains(string(ant), `"budget_tokens":16384`) {
		t.Errorf("effort not mapped to anthropic thinking: %s", ant)
	}
	// anthropic thinking passthrough + mapping back to effort
	areq, err := ParseAnthropicRequest([]byte(`{"model":"m","max_tokens":30000,"thinking":{"type":"enabled","budget_tokens":8192},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || len(areq.Thinking) == 0 {
		t.Fatalf("thinking not parsed: %v", err)
	}
	ant2, _ := BuildAnthropicRequest(areq)
	if !strings.Contains(string(ant2), `"budget_tokens":8192`) {
		t.Errorf("thinking dropped anthropic->anthropic: %s", ant2)
	}
	oai2, _ := BuildOpenAIRequest(areq)
	if !strings.Contains(string(oai2), `"reasoning_effort":"medium"`) {
		t.Errorf("thinking not mapped to effort: %s", oai2)
	}
}
