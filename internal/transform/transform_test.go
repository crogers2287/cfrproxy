package transform

import (
	"encoding/json"
	"testing"
)

func TestApplyOps(t *testing.T) {
	rules, err := Parse(json.RawMessage(`[
		{"op":"set","path":"temperature","value":0.2},
		{"op":"default","path":"max_tokens","value":4096},
		{"op":"rename","path":"max_tokens","to":"max_completion_tokens"},
		{"op":"delete","path":"stream_options"},
		{"op":"set","path":"options.num_ctx","value":8192}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	in := []byte(`{"model":"m","temperature":0.9,"stream_options":{"include_usage":true}}`)
	out := Apply(in, rules)
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["temperature"] != 0.2 {
		t.Errorf("set failed: %v", doc["temperature"])
	}
	if doc["max_completion_tokens"] != 4096.0 {
		t.Errorf("default+rename failed: %v", doc)
	}
	if _, ok := doc["stream_options"]; ok {
		t.Error("delete failed")
	}
	if opts, ok := doc["options"].(map[string]any); !ok || opts["num_ctx"] != 8192.0 {
		t.Errorf("nested set failed: %v", doc["options"])
	}
}

func TestDefaultKeepsExisting(t *testing.T) {
	rules, _ := Parse(json.RawMessage(`[{"op":"default","path":"max_tokens","value":4096}]`))
	out := Apply([]byte(`{"max_tokens":100}`), rules)
	var doc map[string]any
	json.Unmarshal(out, &doc)
	if doc["max_tokens"] != 100.0 {
		t.Errorf("default overwrote existing value: %v", doc["max_tokens"])
	}
}

func TestParseRejectsBadOps(t *testing.T) {
	if _, err := Parse(json.RawMessage(`[{"op":"explode","path":"x"}]`)); err == nil {
		t.Error("expected error for unknown op")
	}
	if _, err := Parse(json.RawMessage(`[{"op":"set","path":""}]`)); err == nil {
		t.Error("expected error for set without path/value")
	}
}
