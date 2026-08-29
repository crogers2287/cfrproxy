package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestObserveModeRecordsCorruptionWithoutChangingRawStream(t *testing.T) {
	clean := "The result is straightforward and the supporting evidence is summarized below. "
	repeated := strings.Repeat("the same phrase is repeating in a tight loop ", 120)
	var upstreamBody strings.Builder
	for _, text := range []string{strings.Repeat(clean, 6), repeated} {
		payload, err := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": text}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		upstreamBody.WriteString("data: ")
		upstreamBody.Write(payload)
		upstreamBody.WriteString("\n\n")
	}
	upstreamBody.WriteString("data: [DONE]\n\n")
	wantBody := upstreamBody.String()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(wantBody))
	}))
	defer backend.Close()

	s := newDiscoveryStore(t)
	provider := store.Provider{
		Name: "observed", Type: "openai", BaseURL: backend.URL, DefaultModel: "loop-model",
		Priority: 10, Enabled: true, IntegrityMode: "observe", IntegrityProfile: "general",
	}
	if err := s.SaveProvider(&provider); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"observed/loop-model","stream":true,"messages":[{"role":"user","content":"continue"}]}`))
	mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != wantBody {
		t.Fatalf("observe mode changed raw stream\ngot:  %q\nwant: %q", got, wantBody)
	}
	traces, err := s.Traces(0, 5)
	if err != nil || len(traces) != 1 {
		t.Fatalf("traces: %v %+v", err, traces)
	}
	trace := traces[0]
	if trace.GuardMode != "observe" || trace.GuardState != "corrupt" || trace.GuardCheckpoints < 2 {
		t.Fatalf("corruption was not observed: %+v", trace)
	}
	if !strings.Contains(trace.GuardData, `"version":1`) || trace.GuardExcerpt == "" {
		t.Fatalf("calibration data missing: %+v", trace)
	}
}
