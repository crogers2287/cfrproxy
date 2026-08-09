package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildCommandCodeRequest(t *testing.T) {
	r := &Request{
		Model:     "deepseek/deepseek-v4-pro",
		System:    "be brief",
		MaxTokens: 4096,
		Messages: []Msg{
			{Role: "user", Content: "what's the weather?"},
			{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{{ID: "call_1", Name: "get_weather", Args: `{"location":"Paris"}`}}},
			{Role: "tool", Content: "sunny", ToolCallID: "call_1"},
		},
		Tools: []Tool{{Name: "get_weather", Description: "Get weather", Params: json.RawMessage(`{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}`)}},
	}
	b, err := BuildCommandCodeRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	params := got["params"].(map[string]any)
	if params["stream"] != true {
		t.Errorf("stream must be forced true, got %v", params["stream"])
	}
	if params["model"] != "deepseek/deepseek-v4-pro" {
		t.Errorf("model = %v", params["model"])
	}
	if params["system"] != "be brief" {
		t.Errorf("system = %v", params["system"])
	}
	if params["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v", params["max_tokens"])
	}
	if got["memory"] != "" || got["permissionMode"] != "standard" {
		t.Errorf("memory/permissionMode wrong: %v %v", got["memory"], got["permissionMode"])
	}
	cfg := got["config"].(map[string]any)
	for _, k := range []string{"workingDir", "date", "environment", "structure", "isGitRepo", "currentBranch", "mainBranch", "gitStatus", "recentCommits"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("config.%s missing", k)
		}
	}
	if _, err := time.Parse("2006-01-02", cfg["date"].(string)); err != nil {
		t.Errorf("config.date not YYYY-MM-DD: %v", cfg["date"])
	}
	if cfg["environment"] != "production" {
		t.Errorf("config.environment = %v", cfg["environment"])
	}

	msgs := params["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3", len(msgs))
	}
	// user → text part
	user := msgs[0].(map[string]any)
	if user["role"] != "user" || user["content"].([]any)[0].(map[string]any)["text"] != "what's the weather?" {
		t.Errorf("user message wrong: %v", user)
	}
	// assistant → text + tool-call parts
	asst := msgs[1].(map[string]any)
	parts := asst["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("assistant content len = %d, want 2", len(parts))
	}
	tc := parts[1].(map[string]any)
	if tc["type"] != "tool-call" || tc["toolCallId"] != "call_1" || tc["toolName"] != "get_weather" {
		t.Errorf("tool-call part wrong: %v", tc)
	}
	if tc["input"].(map[string]any)["location"] != "Paris" {
		t.Errorf("tool-call input wrong: %v", tc["input"])
	}
	// tool → tool-result with name recovered from the assistant turn
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" {
		t.Errorf("tool msg role = %v", tool["role"])
	}
	tr := tool["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool-result" || tr["toolCallId"] != "call_1" || tr["toolName"] != "get_weather" {
		t.Errorf("tool-result part wrong: %v", tr)
	}
	if tr["output"].(map[string]any)["value"] != "sunny" {
		t.Errorf("tool-result output wrong: %v", tr["output"])
	}

	tools := params["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d", len(tools))
	}
	toolDef := tools[0].(map[string]any)
	if toolDef["name"] != "get_weather" || toolDef["description"] != "Get weather" {
		t.Errorf("tool def wrong: %v", toolDef)
	}
	if _, ok := toolDef["input_schema"]; !ok {
		t.Errorf("tool input_schema missing: %v", toolDef)
	}
}

func TestParseCommandCodeResponse(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"start"}`,
		`{"type":"start-step","request":{"body":{}},"warnings":[]}`,
		`{"type":"reasoning-start","id":"r0"}`,
		`{"type":"reasoning-delta","id":"r0","text":"Let me "}`,
		`{"type":"reasoning-delta","id":"r0","text":"think"}`,
		`{"type":"reasoning-end","id":"r0"}`,
		`{"type":"text-start","id":"txt-0"}`,
		`{"type":"text-delta","id":"txt-0","text":"Hi"}`,
		`{"type":"text-end","id":"txt-0"}`,
		`{"type":"tool-input-start","id":"call_0","toolName":"get_weather"}`,
		`{"type":"tool-input-delta","id":"call_0","delta":"{\"loc\""}`,
		`{"type":"tool-input-delta","id":"call_0","delta":"ation\":\"Paris\"}"}`,
		`{"type":"tool-input-end","id":"call_0"}`,
		`{"type":"tool-call","toolCallId":"call_0","toolName":"get_weather","input":{"location":"Paris"}}`,
		`{"type":"finish-step","finishReason":"tool-calls","rawFinishReason":"tool_calls","usage":{"inputTokens":100,"outputTokens":20,"cachedInputTokens":40}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	}, "\n")

	r, err := ParseCommandCodeResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "Hi" {
		t.Errorf("Content = %q", r.Content)
	}
	if r.ReasoningContent != "Let me think" {
		t.Errorf("ReasoningContent = %q", r.ReasoningContent)
	}
	if r.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", r.FinishReason)
	}
	if r.PromptTokens != 60 || r.CompletionTokens != 20 || r.CachedTokens != 40 {
		t.Errorf("tokens = %d/%d/%d, want 60/20/40", r.PromptTokens, r.CompletionTokens, r.CachedTokens)
	}
	if len(r.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d", len(r.ToolCalls))
	}
	tc := r.ToolCalls[0]
	if tc.ID != "call_0" || tc.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Args != `{"location":"Paris"}` {
		t.Errorf("tool args = %q", tc.Args)
	}
}

func TestReadCommandCodeStream(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"text-delta","id":"txt-0","text":"Hel"}`,
		`{"type":"text-delta","id":"txt-0","text":"lo"}`,
		`{"type":"tool-input-start","id":"call_0","toolName":"get_weather"}`,
		`{"type":"tool-input-delta","id":"call_0","delta":"{\"l\""}`,
		`{"type":"tool-input-delta","id":"call_0","delta":"oc\":\"NYC\"}"}`,
		`{"type":"finish-step","finishReason":"tool-calls","usage":{"inputTokens":50,"outputTokens":5,"cachedInputTokens":10}}`,
		`{"type":"finish","finishReason":"tool-calls"}`,
	}, "\n")

	got := []Delta{}
	ch := make(chan Delta, 16)
	go ReadCommandCodeStream(strings.NewReader(body), ch)
	for d := range ch {
		got = append(got, d)
	}
	if len(got) != 5 {
		t.Fatalf("deltas = %d, want 5 (2 text, 2 tool, 1 finish)", len(got))
	}
	if got[0].Text != "Hel" || got[1].Text != "lo" {
		t.Errorf("text deltas = %q %q", got[0].Text, got[1].Text)
	}
	if got[2].TC == nil || got[2].TC.Args != `{"l"` {
		t.Errorf("tool delta 1 = %+v", got[2].TC)
	}
	if got[3].TC == nil || got[3].TC.Args != `oc":"NYC"}` {
		t.Errorf("tool delta 2 = %+v", got[3].TC)
	}
	f := got[4]
	if f.Finish != "tool_calls" {
		t.Errorf("finish = %q", f.Finish)
	}
	if f.PromptTokens != 40 || f.CompletionTokens != 5 || f.CachedTokens != 10 {
		t.Errorf("finish tokens = %d/%d/%d, want 40/5/10", f.PromptTokens, f.CompletionTokens, f.CachedTokens)
	}
}
