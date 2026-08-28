package proxy

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// (13) a base64 image payload must never reach a trace snippet or a log line.
// Traces exist for debugging; a construction plan or a whiteboard photo parked
// in that table is both a privacy problem and an eviction problem — the image
// would push the actual conversation out of the snippet window.
func TestTraceSnippetRedactsImagePayloads(t *testing.T) {
	// a payload long enough that truncation alone could not be what hides it
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("SENSITIVE-PLAN-BYTES", 40)))
	body := []byte(`{"model":"commandcode/deepseek/deepseek-v4-flash-vision-exp","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is the sheet number?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + secret + `"}}]}]}`)

	got := snip(body)
	if strings.Contains(got, secret) {
		t.Fatal("trace snippet contains the base64 image payload")
	}
	if strings.Contains(got, secret[:64]) {
		t.Fatal("trace snippet contains the head of the base64 payload")
	}
	// the conversation itself must survive: redaction, not blanket truncation
	if !strings.Contains(got, "what is the sheet number?") {
		t.Error("redaction ate the prompt text the trace exists to show")
	}
	if !strings.Contains(got, "base64 chars redacted") {
		t.Error("no marker left; the trace should still say an image was there")
	}

	// jpeg/webp and multiple images are covered by the same path
	multi := []byte(`{"a":"data:image/jpeg;base64,` + secret + `","b":"data:image/webp;base64,` + secret + `"}`)
	if r := string(redactImagePayloads(multi)); strings.Contains(r, secret) {
		t.Error("multi-image body not fully redacted")
	}
	// bodies with no image are returned untouched (fast path)
	plain := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	if string(redactImagePayloads(plain)) != string(plain) {
		t.Error("non-image body was modified")
	}
}

// The redaction must not corrupt an otherwise well-formed body it passes on:
// snip() is display-only, but the regex must not run wild over normal text.
func TestRedactionLeavesNonImageBase64Alone(t *testing.T) {
	body := []byte(`{"note":"the string ;base64, appears here but no data url does"}`)
	if got := string(redactImagePayloads(body)); got != string(body) {
		t.Errorf("body without a data URL was altered:\n got %s", got)
	}
	_ = regexp.MustCompile // keep the import honest if the impl changes
}

// (5) deepseek-v4-flash-vision-exp must be recognized by the proactive vision
// gate, or the router would refuse to send it the very images it can read.
func TestDeepSeekVisionExpIsVisionCapable(t *testing.T) {
	p := New(newDiscoveryStore(t))
	for _, m := range []string{
		"deepseek/deepseek-v4-flash-vision-exp",
		"commandcode/deepseek/deepseek-v4-flash-vision-exp",
		"deepseek-v4-flash-vision-exp",
	} {
		if !p.VisionCapable(m) {
			t.Errorf("%q not recognized as vision-capable", m)
		}
	}
	// and the text-only sibling must NOT be, so images never route to it blind
	if p.VisionCapable("commandcode/deepseek/deepseek-v4-flash") {
		t.Error("deepseek-v4-flash (no vision) was classified as vision-capable")
	}
}
