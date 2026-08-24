package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

func bigLog(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i == n/2 {
			b.WriteString("2026-08-23 10:00:00 ERROR something exploded\n")
			continue
		}
		fmt.Fprintf(&b, "2026-08-23 10:00:%02d INFO routine chatter iteration %d\n", i%60, i)
	}
	return b.String()
}

func toolMsg(content string) wire.Msg { return wire.Msg{Role: "tool", Content: content} }

// The cache-safety property: compressing the same text must always give the
// same bytes, no matter what else is in the request. If this ever fails, the
// prefix changes turn-over-turn and prefix caching collapses.
func TestCavemanDeterministic(t *testing.T) {
	src := bigLog(400)
	first := cavemanCompressOne(src)
	for i := 0; i < 50; i++ {
		if got := cavemanCompressOne(src); got != first {
			t.Fatalf("non-deterministic output on run %d", i)
		}
	}
}

// Compressing an already-compressed message must not change it again, or a
// message would drift every turn it survives in the window.
func TestCavemanIdempotent(t *testing.T) {
	for name, src := range map[string]string{
		"logs": bigLog(300),
		"json": `{"items":[` + strings.Repeat(`{"a":"`+strings.Repeat("x", 900)+`"},`, 40) + `{"a":"z"}]}`,
		"text": strings.Repeat("plain prose line\n", 300),
	} {
		t.Run(name, func(t *testing.T) {
			once := cavemanCompressOne(src)
			twice := cavemanCompressOne(once)
			if once != twice {
				t.Fatalf("not idempotent: %d -> %d bytes", len(once), len(twice))
			}
		})
	}
}

// Growth would be a silent regression: we would pay more tokens for the
// privilege of mangling the payload.
func TestCavemanNeverGrows(t *testing.T) {
	for _, src := range []string{
		bigLog(200),
		strings.Repeat("a", 9000),
		`{"k":[1,2,3]}`,
		"short",
		"",
	} {
		if got := cavemanCompressOne(src); len(got) > len(src)+120 {
			t.Fatalf("grew %d -> %d", len(src), len(got))
		}
	}
}

// The whole point of the log detector: errors must survive.
func TestCavemanKeepsErrors(t *testing.T) {
	got := cavemanCompressOne(bigLog(400))
	if !strings.Contains(got, "ERROR something exploded") {
		t.Fatal("error line was dropped")
	}
	if len(got) >= len(bigLog(400)) {
		t.Fatal("no reduction on a log payload")
	}
}

func TestCavemanJSONStaysValid(t *testing.T) {
	src := `{"rows":[` + strings.Repeat(`{"name":"`+strings.Repeat("q", 800)+`"},`, 30) + `{"name":"last"}]}`
	got := cavemanCompressOne(src)
	if !json.Valid([]byte(got)) {
		t.Fatalf("compressed JSON is not valid JSON: %.120s", got)
	}
	if len(got) >= len(src) {
		t.Fatalf("no reduction: %d -> %d", len(src), len(got))
	}
}

// System prompts and tool schemas are the cache prefix. They must be untouched.
func TestCavemanLeavesPrefixAlone(t *testing.T) {
	sys := strings.Repeat("SYSTEM PROMPT LINE\n", 500)
	req := &wire.Request{
		System: sys,
		Tools:  []wire.Tool{{Name: "t", Description: strings.Repeat("d", 5000)}},
		Messages: []wire.Msg{
			{Role: "user", Content: strings.Repeat("user text\n", 500)},
			toolMsg(bigLog(300)),
			toolMsg(bigLog(300)),
			toolMsg(bigLog(300)),
			toolMsg(bigLog(300)),
		},
	}
	userBefore := req.Messages[0].Content
	st := CavemanCompress(req, false)
	if req.System != sys {
		t.Fatal("System was modified")
	}
	if len(req.Tools[0].Description) != 5000 {
		t.Fatal("tool schema was modified")
	}
	if req.Messages[0].Content != userBefore {
		t.Fatal("user message was modified")
	}
	if st.Msgs == 0 {
		t.Fatal("expected some tool messages to be compressed")
	}
}

// The newest results are what the model is reasoning about right now.
func TestCavemanKeepsRecentToolResultsVerbatim(t *testing.T) {
	newest := bigLog(300)
	req := &wire.Request{Messages: []wire.Msg{
		toolMsg(bigLog(300)),
		toolMsg(bigLog(300)),
		toolMsg(bigLog(300)),
		toolMsg(newest),
	}}
	CavemanCompress(req, false)
	n := len(req.Messages)
	for i := n - cavemanKeepRecent; i < n; i++ {
		if strings.Contains(req.Messages[i].Content, "caveman:") {
			t.Fatalf("message %d (within keep-recent window) was compressed", i)
		}
	}
	if !strings.Contains(req.Messages[0].Content, "caveman:") {
		t.Fatal("oldest tool result should have been compressed")
	}
}

// Small payloads are already cheap; touching them risks losing the answer.
func TestCavemanSkipsSmall(t *testing.T) {
	small := strings.Repeat("x", cavemanMinBytes-1)
	req := &wire.Request{Messages: []wire.Msg{
		toolMsg(small), toolMsg(small), toolMsg(small), toolMsg(small),
	}}
	st := CavemanCompress(req, false)
	if st.Msgs != 0 {
		t.Fatalf("compressed %d small messages", st.Msgs)
	}
}

// Off by default: a request through a provider with the flag clear must be
// byte-identical. (The flag is checked by the caller; this pins the contract
// that CavemanCompress is the ONLY thing that mutates, and reports honestly.)
func TestCavemanStatsAccounting(t *testing.T) {
	req := &wire.Request{Messages: []wire.Msg{
		toolMsg(bigLog(400)), toolMsg(bigLog(400)), toolMsg(bigLog(400)),
	}}
	st := CavemanCompress(req, false)
	if st.Msgs != 1 {
		t.Fatalf("expected 1 compressed (3 tools - 2 kept), got %d", st.Msgs)
	}
	if st.Before <= st.After || st.Saved() == 0 {
		t.Fatalf("bad accounting: before=%d after=%d", st.Before, st.After)
	}
	if st.After != len(req.Messages[0].Content) {
		t.Fatalf("after=%d does not match actual content %d", st.After, len(req.Messages[0].Content))
	}
}

func TestDetectKind(t *testing.T) {
	cases := map[string]cavemanKind{
		`{"a":1}`:  kindJSON,
		`[1,2,3]`:  kindJSON,
		bigLog(50): kindLogs,
		"diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n": kindDiff,
		"just some prose\nmore prose\n":             kindText,
	}
	for in, want := range cases {
		if got := detectKind(in); got != want {
			t.Errorf("detectKind(%.30q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseCavemanMode(t *testing.T) {
	cases := map[string]struct {
		mode CavemanMode
		set  bool
	}{
		"off": {CMOff, true}, "0": {CMOff, true}, "false": {CMOff, true},
		"in": {CMIn, true}, "request": {CMIn, true},
		"out": {CMOut, true}, "response": {CMOut, true},
		"both": {CMBoth, true}, "1": {CMBoth, true}, "true": {CMBoth, true}, "ON": {CMBoth, true},
		"":         {CMOff, false},
		"nonsense": {CMOff, false}, // a typo must never silently compress
	}
	for in, want := range cases {
		got, set := ParseCavemanMode(in)
		if got != want.mode || set != want.set {
			t.Errorf("ParseCavemanMode(%q) = (%v,%v), want (%v,%v)", in, got, set, want.mode, want.set)
		}
	}
}

func TestCavemanModePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		reqMode  CavemanMode
		reqSet   bool
		ep, prov bool
		want     CavemanMode
	}{
		{"nothing set", CMOff, false, false, false, CMOff},
		{"provider on -> in only", CMOff, false, false, true, CMIn},
		{"endpoint on -> in only", CMOff, false, true, false, CMIn},
		{"request wins over provider", CMBoth, true, false, true, CMBoth},
		{"request can disable provider", CMOff, true, false, true, CMOff},
		{"request out with nothing else", CMOut, true, false, false, CMOut},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CavemanModeFor(c.reqMode, c.reqSet, c.ep, c.prov); got != c.want {
				t.Fatalf("= %v, want %v", got, c.want)
			}
		})
	}
}

// A standing checkbox must never turn on OUTPUT compression: that changes what
// the caller receives and has to be asked for explicitly.
func TestStandingConfigNeverCompressesOutput(t *testing.T) {
	for _, c := range []struct{ ep, prov bool }{{true, false}, {false, true}, {true, true}} {
		m := CavemanModeFor(CMOff, false, c.ep, c.prov)
		if m.CompressOut() {
			t.Fatalf("ep=%v prov=%v produced output compression (%v)", c.ep, c.prov, m)
		}
	}
}

func TestCavemanCompressResponse(t *testing.T) {
	long := bigLog(400)
	r := &wire.Response{Content: long}
	st := CavemanCompressResponse(r)
	if st.Msgs != 1 || st.Saved() == 0 {
		t.Fatalf("no compression: %+v", st)
	}
	if !strings.Contains(r.Content, "ERROR something exploded") {
		t.Fatal("error line lost from response")
	}
	// small replies untouched
	small := &wire.Response{Content: "short answer"}
	if got := CavemanCompressResponse(small); got.Msgs != 0 || small.Content != "short answer" {
		t.Fatal("small response was modified")
	}
	// tool calls must survive verbatim — they get executed, not read
	tc := &wire.Response{Content: long, ToolCalls: []wire.ToolCall{{ID: "1", Name: "f", Args: `{"a":1}`}}}
	CavemanCompressResponse(tc)
	if tc.ToolCalls[0].Args != `{"a":1}` {
		t.Fatal("tool call arguments were modified")
	}
}

// Standing config must stay conservative: tool results only, so an ongoing
// conversation's cached prefix is not disturbed.
func TestCavemanStandingPolicyLeavesUserMessagesAlone(t *testing.T) {
	big := bigLog(400)
	req := &wire.Request{Messages: []wire.Msg{
		{Role: "user", Content: big},
		{Role: "user", Content: "what happened?"},
	}}
	st := CavemanCompress(req, false)
	if st.Msgs != 0 || req.Messages[0].Content != big {
		t.Fatal("standing policy compressed a user message")
	}
}

// An explicit per-request flag means "shrink what I am sending" — including a
// big payload pasted in as a user message. This is the /caveman skill's path.
func TestCavemanExplicitCompressesUserMessages(t *testing.T) {
	big := bigLog(400)
	question := "One sentence: what error is in this log?"
	req := &wire.Request{Messages: []wire.Msg{
		{Role: "user", Content: big},
		{Role: "user", Content: question},
	}}
	st := CavemanCompress(req, true)
	if st.Msgs != 1 {
		t.Fatalf("expected the payload message compressed, got %d", st.Msgs)
	}
	if len(req.Messages[0].Content) >= len(big) {
		t.Fatal("payload was not shrunk")
	}
	if !strings.Contains(req.Messages[0].Content, "ERROR something exploded") {
		t.Fatal("error line lost from payload")
	}
	if req.Messages[1].Content != question {
		t.Fatal("the instruction itself was modified")
	}
}

// Whatever the policy, the final message carries the instruction and must
// survive verbatim — otherwise we save tokens by destroying the question.
func TestCavemanNeverCompressesLastMessage(t *testing.T) {
	big := bigLog(400)
	for _, explicit := range []bool{false, true} {
		req := &wire.Request{Messages: []wire.Msg{
			{Role: "user", Content: "setup"},
			{Role: "user", Content: big}, // last message, huge
		}}
		CavemanCompress(req, explicit)
		if req.Messages[1].Content != big {
			t.Fatalf("explicit=%v compressed the last message", explicit)
		}
	}
}

// System prompt stays untouched even under the explicit policy.
func TestCavemanExplicitStillSpareSystem(t *testing.T) {
	sys := strings.Repeat("SYSTEM\n", 2000)
	req := &wire.Request{System: sys, Messages: []wire.Msg{
		{Role: "user", Content: bigLog(400)},
		{Role: "user", Content: "go"},
	}}
	CavemanCompress(req, true)
	if req.System != sys {
		t.Fatal("explicit policy modified System")
	}
}
