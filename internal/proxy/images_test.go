package proxy

import (
	"encoding/json"
	"testing"
)

// The only field cfrproxy rewrites is the model. Everything else is
// provider-specific surface (size, quality, aspect_ratio, vendor extensions)
// and must survive untouched, or adding a new image parameter would mean
// editing this proxy.
func TestSetJSONModelPreservesEveryOtherField(t *testing.T) {
	in := []byte(`{
		"model": "grok-imagine-image",
		"prompt": "a cat",
		"n": 2,
		"size": "1024x1024",
		"response_format": "b64_json",
		"aspect_ratio": "16:9",
		"vendor_ext": {"seed": 42, "nested": ["a", "b"]}
	}`)
	out, err := setJSONModel(in, "grok/grok-imagine-image")
	if err != nil {
		t.Fatalf("setJSONModel: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["model"] != "grok/grok-imagine-image" {
		t.Fatalf("model = %v, want rewritten", got["model"])
	}
	if got["prompt"] != "a cat" || got["size"] != "1024x1024" || got["aspect_ratio"] != "16:9" {
		t.Fatalf("scalar field lost or altered: %v", got)
	}
	if got["n"].(float64) != 2 {
		t.Fatalf("n = %v, want 2", got["n"])
	}
	ext, ok := got["vendor_ext"].(map[string]any)
	if !ok || ext["seed"].(float64) != 42 {
		t.Fatalf("nested vendor extension lost: %v", got["vendor_ext"])
	}
	if arr, ok := ext["nested"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("nested array lost: %v", ext["nested"])
	}
}

// A request with no model at all still has to come back with one, otherwise a
// scoped mount (/p/grok) could forward a body the provider rejects.
func TestSetJSONModelAddsMissingModel(t *testing.T) {
	out, err := setJSONModel([]byte(`{"prompt":"x"}`), "grok/grok-imagine-image")
	if err != nil {
		t.Fatalf("setJSONModel: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "grok/grok-imagine-image" {
		t.Fatalf("model = %v, want injected", got["model"])
	}
	if got["prompt"] != "x" {
		t.Fatalf("prompt lost: %v", got)
	}
}

// A body we cannot parse is the provider's to reject with its own error
// message. Rewriting must not turn that into a cfrproxy 400 or, worse, replace
// the caller's payload with something invented here.
func TestSetJSONModelPassesThroughUnparseableBody(t *testing.T) {
	in := []byte(`not json at all`)
	out, err := setJSONModel(in, "grok/grok-imagine-image")
	if err != nil {
		t.Fatalf("unparseable body should not error, got %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("body was altered: %q", out)
	}
}

// An empty JSON object is parseable, so the model should be injected rather
// than passed through.
func TestSetJSONModelHandlesEmptyObject(t *testing.T) {
	out, err := setJSONModel([]byte(`{}`), "codex/gpt-image-2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "codex/gpt-image-2" {
		t.Fatalf("model = %v, want injected", got["model"])
	}
}
