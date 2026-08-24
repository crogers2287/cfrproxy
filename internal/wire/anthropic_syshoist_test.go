package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// A system message that arrives before any real turn is part of the static
// preamble — hoisting it is correct AND cache-safe, so that behaviour stays.
func TestLeadingSystemMessagesStillHoist(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"system","content":"preamble a"},
			{"role":"system","content":"preamble b"},
			{"role":"user","content":"hi"}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "top\n\npreamble a\n\npreamble b" {
		t.Errorf("leading system not hoisted: %q", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
		t.Errorf("messages = %+v", req.Messages)
	}
}

// THE invariant this change exists for: adding a reminder to turn N must leave
// the system prompt and every earlier message byte-identical, so the upstream
// KV prefix survives. Only bytes at/after the new turn may differ.
func TestReminderDoesNotDisturbCachedPrefix(t *testing.T) {
	without := []byte(`{"model":"m","max_tokens":10,"system":"BIG STATIC PROMPT",
		"messages":[
			{"role":"user","content":"turn one"},
			{"role":"assistant","content":"reply one"},
			{"role":"user","content":"turn two"}
		]}`)
	with := []byte(`{"model":"m","max_tokens":10,"system":"BIG STATIC PROMPT",
		"messages":[
			{"role":"user","content":"turn one"},
			{"role":"assistant","content":"reply one"},
			{"role":"system","content":"<system-reminder>ephemeral</system-reminder>"},
			{"role":"user","content":"turn two"}
		]}`)

	a, err := ParseAnthropicRequest(without)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAnthropicRequest(with)
	if err != nil {
		t.Fatal(err)
	}

	if a.System != b.System {
		t.Fatalf("system prompt changed, whole prefix is lost:\n without=%q\n with   =%q", a.System, b.System)
	}
	if len(a.Messages) != len(b.Messages) {
		t.Fatalf("message count changed: %d vs %d", len(a.Messages), len(b.Messages))
	}
	// every message before the reminder's turn must be untouched
	for i := 0; i < len(a.Messages)-1; i++ {
		x, _ := json.Marshal(a.Messages[i])
		y, _ := json.Marshal(b.Messages[i])
		if string(x) != string(y) {
			t.Errorf("message %d diverged, invalidating the prefix:\n without=%s\n with   =%s", i, x, y)
		}
	}
	last := b.Messages[len(b.Messages)-1]
	if !strings.Contains(last.Content, "ephemeral") || !strings.HasSuffix(last.Content, "turn two") {
		t.Errorf("reminder did not land on the final user turn: %q", last.Content)
	}
}

// A trailing system message has no following user turn; it must still land
// after the prefix rather than in the system block.
func TestTrailingSystemMessageGoesToLastTurn(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"system","content":"late note"}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "top" {
		t.Errorf("trailing system polluted the prefix: %q", req.System)
	}
	if n := len(req.Messages); n != 1 || req.Messages[0].Content != "hi\n\nlate note" {
		t.Errorf("messages = %+v", req.Messages)
	}
}

// A user message carrying only tool_result blocks still has to absorb a pending
// reminder, or the text would be dropped entirely.
func TestReminderSurvivesToolResultOnlyTurn(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]},
			{"role":"system","content":"reminder"},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"data"}]}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "top" {
		t.Errorf("system polluted: %q", req.System)
	}
	var joined []string
	for _, m := range req.Messages {
		joined = append(joined, m.Role+":"+m.Content)
	}
	all := strings.Join(joined, " | ")
	if !strings.Contains(all, "reminder") {
		t.Fatalf("reminder was dropped: %s", all)
	}
	if !strings.Contains(all, "tool:data") {
		t.Errorf("tool result lost: %s", all)
	}
}

// Several reminders between two turns must all survive, in order.
func TestMultipleRemindersMergeInOrder(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"user","content":"one"},
			{"role":"system","content":"first"},
			{"role":"system","content":"second"},
			{"role":"user","content":"two"}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	want := "first\n\nsecond\n\ntwo"
	if got := req.Messages[len(req.Messages)-1].Content; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Whatever we do internally, the OpenAI dialect must never receive a system
// message anywhere but index 0 — that was the original reason for hoisting.
func TestNoSystemRoleEscapesToOpenAI(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":10,"system":"top",
		"messages":[
			{"role":"user","content":"a"},
			{"role":"system","content":"mid"},
			{"role":"assistant","content":"b"},
			{"role":"system","content":"mid2"},
			{"role":"user","content":"c"}
		]}`)
	req, err := ParseAnthropicRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := BuildOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var oai struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &oai); err != nil {
		t.Fatal(err)
	}
	for i, m := range oai.Messages {
		if m.Role == "system" && i != 0 {
			t.Errorf("system message at index %d: %+v", i, oai.Messages)
		}
	}
}
