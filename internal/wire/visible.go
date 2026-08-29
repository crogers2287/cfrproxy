package wire

import (
	"bytes"
	"encoding/json"
)

// StreamVisibleText extracts only user-visible assistant text from one raw
// provider frame. It intentionally ignores reasoning and tool arguments: the
// integrity observer is calibrated for prose, while tool JSON requires a
// separate schema-aware validator.
//
// This helper lets observation run on cfrproxy's raw fast path without
// translating or otherwise changing the bytes delivered to clients.
func StreamVisibleText(providerType string, frame []byte) string {
	line := bytes.TrimSpace(frame)
	if bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(line[len("data:"):])
	}
	if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) || line[0] != '{' {
		return ""
	}

	switch providerType {
	case "anthropic":
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
			return event.Delta.Text
		}
	case "responses":
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "response.output_text.delta" {
			return event.Delta
		}
	case "ollama":
		var event struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Response string `json:"response"`
		}
		if json.Unmarshal(line, &event) == nil {
			if event.Message.Content != "" {
				return event.Message.Content
			}
			return event.Response
		}
	case "commandcode":
		var event struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "text-delta" {
			return event.Text
		}
	default: // OpenAI chat completions
		var event struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(line, &event) != nil {
			return ""
		}
		var out bytes.Buffer
		for _, choice := range event.Choices {
			var text string
			if json.Unmarshal(choice.Delta.Content, &text) == nil {
				out.WriteString(text)
				continue
			}
			// A few compatible providers stream typed content parts instead of
			// the canonical string. Accept visible text parts while continuing
			// to ignore reasoning and tool payloads.
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(choice.Delta.Content, &parts) == nil {
				for _, part := range parts {
					if part.Type == "text" || part.Type == "output_text" {
						out.WriteString(part.Text)
					}
				}
			}
		}
		return out.String()
	}
	return ""
}
