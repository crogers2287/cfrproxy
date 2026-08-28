package proxy

import (
	"strings"
	"testing"
)

func TestSanitizeEmptySystemQuick(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":""},{"role":"user","content":"hi"}],"model":"m"}`)
	out := sanitizeEmptySystem(in)
	if strings.Contains(string(out), `"system"`) {
		t.Fatalf("empty system not removed: %s", out)
	}
	keep := []byte(`{"messages":[{"role":"system","content":"real"},{"role":"user","content":"hi"}]}`)
	if string(sanitizeEmptySystem(keep)) != string(keep) {
		t.Fatalf("non-empty system must pass through untouched")
	}
	anth := []byte(`{"system":"","messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(string(sanitizeEmptySystem(anth)), `"system":""`) {
		t.Fatalf("empty anthropic system not removed")
	}
}

func TestSanitizeTruncatedToolArgs(t *testing.T) {
	in := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"cat <<EOF\nunterminated"}}]},{"role":"tool","tool_call_id":"c1","content":"x"}]}`)
	out := string(sanitizeEmptySystem(in))
	if !strings.Contains(out, "_truncated_args") {
		t.Fatalf("broken args not repaired: %s", out)
	}
	good := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{\"a\":1}"}}]}]}`)
	if string(sanitizeEmptySystem(good)) != string(good) {
		t.Fatalf("valid args must pass through untouched")
	}
}
