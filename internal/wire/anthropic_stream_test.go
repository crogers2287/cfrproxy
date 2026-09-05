package wire

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Text arriving between two argument fragments of the same tool call must not
// split the call into two tool_use blocks (the second with a fresh id and no
// name). The text is held until the tool block closes.
func TestAnthropicStreamKeepsToolCallInOneBlock(t *testing.T) {
	in := make(chan Delta, 16)
	in <- Delta{TC: &TCDelta{Index: 0, ID: "call_1", Name: "write"}}
	in <- Delta{TC: &TCDelta{Index: 0, Args: `{"path":`}}
	in <- Delta{Text: "working"}
	in <- Delta{TC: &TCDelta{Index: 0, Args: `"a.go"}`}}
	in <- Delta{Finish: "tool_calls"}
	close(in)
	rec := httptest.NewRecorder()
	if err := WriteAnthropicStream(rec, "m", in); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if n := strings.Count(out, `"type":"tool_use"`); n != 1 {
		t.Fatalf("want exactly one tool_use block, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `"name":"write"`) || !strings.Contains(out, `"id":"call_1"`) {
		t.Fatalf("tool identity lost:\n%s", out)
	}
	if strings.Index(out, `"text_delta"`) < strings.LastIndex(out, `"input_json_delta"`) {
		t.Fatalf("held text should be emitted after the tool block closes:\n%s", out)
	}
	if !strings.Contains(out, `"text":"working"`) {
		t.Fatalf("held text was dropped:\n%s", out)
	}
}

// A fragment for an index whose block already closed reopens under the same
// id and name rather than inventing a nameless call.
func TestAnthropicStreamReopenedToolCallKeepsIdentity(t *testing.T) {
	in := make(chan Delta, 16)
	in <- Delta{TC: &TCDelta{Index: 0, ID: "call_a", Name: "one"}}
	in <- Delta{TC: &TCDelta{Index: 1, ID: "call_b", Name: "two"}}
	in <- Delta{TC: &TCDelta{Index: 0, Args: `{}`}}
	in <- Delta{Finish: "tool_calls"}
	close(in)
	rec := httptest.NewRecorder()
	if err := WriteAnthropicStream(rec, "m", in); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"name":""`) {
		t.Fatalf("nameless tool_use block emitted:\n%s", out)
	}
	if strings.Count(out, `"id":"call_a"`) != 2 {
		t.Fatalf("reopened block should carry the original id:\n%s", out)
	}
}

func TestAnthropicReaderForwardsThinking(t *testing.T) {
	sse := strings.Join([]string{
		`event: content_block_start`, `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`, ``,
		`event: content_block_delta`, `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"ok"}}`, ``,
		`event: message_delta`, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`, ``,
		`event: message_stop`, `data: {"type":"message_stop"}`, ``,
	}, "\n")
	deltas := make(chan Delta, 16)
	go ReadAnthropicStream(strings.NewReader(sse), deltas)
	rec := httptest.NewRecorder()
	if err := WriteOpenAIStream(rec, "m", deltas); err != nil {
		t.Fatal(err)
	}
	if out := rec.Body.String(); !strings.Contains(out, `"reasoning_content":"hmm"`) {
		t.Fatalf("thinking not forwarded as reasoning_content:\n%s", out)
	}
}

// Reasoning deltas become a thinking block that precedes the text, closed
// with a signature_delta the way the API does — Claude Code then shows
// "Thinking…" instead of "Waiting for API response" for the whole reasoning.
func TestAnthropicStreamForwardsReasoningAsThinking(t *testing.T) {
	in := make(chan Delta, 16)
	in <- Delta{Reasoning: "Let me consider "}
	in <- Delta{Reasoning: "the options."}
	in <- Delta{Text: "The sky is blue."}
	in <- Delta{Finish: "stop"}
	close(in)
	rec := httptest.NewRecorder()
	if err := WriteAnthropicStream(rec, "m", in); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	for _, want := range []string{`"content_block":{"thinking":"","type":"thinking"}`, `"thinking_delta"`, `"thinking":"Let me consider "`, `"signature_delta"`, `"text":"The sky is blue."`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, `"thinking_delta"`) > strings.Index(out, `"text_delta"`) {
		t.Fatalf("thinking must precede text:\n%s", out)
	}
	if strings.Count(out, `"index":0`) < 3 || !strings.Contains(out, `"index":1`) {
		t.Fatalf("thinking should be block 0 and text block 1:\n%s", out)
	}
}
