package wire

import "testing"

func TestStreamVisibleText(t *testing.T) {
	tests := []struct {
		name, provider, frame, want string
	}{
		{"openai", "openai", `data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n", "hello"},
		{"openai typed text", "openai", `data: {"choices":[{"delta":{"content":[{"type":"text","text":"hello"},{"type":"reasoning","text":"secret"}]}}]}`, "hello"},
		{"openai reasoning ignored", "openai", `data: {"choices":[{"delta":{"reasoning_content":"secret"}}]}`, ""},
		{"anthropic", "anthropic", `event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, ""},
		{"anthropic data", "anthropic", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, "hello"},
		{"responses", "responses", `data: {"type":"response.output_text.delta","delta":"hello"}`, "hello"},
		{"ollama", "ollama", `{"message":{"role":"assistant","content":"hello"},"done":false}`, "hello"},
		{"commandcode", "commandcode", `{"type":"text-delta","text":"hello"}`, "hello"},
		{"commandcode tool ignored", "commandcode", `{"type":"tool-input-delta","delta":"{\\"x\\":1}"}`, ""},
		{"done", "openai", `data: [DONE]`, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StreamVisibleText(test.provider, []byte(test.frame)); got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}
