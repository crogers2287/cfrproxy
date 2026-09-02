package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// The per-provider `fallback` hop is gated by the provider_fallback setting
// and is OFF by default: a provider's own chain is invisible in the admin UI,
// and on 2026-07-31 it quietly billed DeepSeek for every local request while
// llama-swap was down. The setting existed in the DB but nothing read it.
func TestProviderFallbackGatedBySetting(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(503)
		w.Write([]byte(`{"error":"down"}`))
	}))
	defer primary.Close()
	backupHits := 0
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		backupHits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"b","choices":[{"message":{"role":"assistant","content":"saved"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer backup.Close()

	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "backup", Type: "openai", BaseURL: backup.URL, DefaultModel: "b", Priority: 20, Enabled: true})
	s.SaveProvider(&store.Provider{Name: "primary", Type: "openai", BaseURL: primary.URL, DefaultModel: "pm", Priority: 10, Enabled: true, Fallback: "backup/b"})
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	resetFailoverNotices()
	body := `{"model":"primary/pm","messages":[{"role":"user","content":"hi"}]}`

	// default (unset) → the provider's own fallback is NOT walked
	s.SetSetting("provider_fallback", "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code == 200 || strings.Contains(rec.Body.String(), "saved") {
		t.Fatalf("per-provider chain ran while provider_fallback is unset: %d %s", rec.Code, rec.Body.String())
	}
	if backupHits != 0 {
		t.Fatalf("backup was contacted %d times with the chain disabled", backupHits)
	}

	// explicitly enabled → the chain walks to backup
	s.SetSetting("provider_fallback", `{"enabled":true}`)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "saved") {
		t.Fatalf("expected failover to backup with provider_fallback enabled: %d %s", rec.Code, rec.Body.String())
	}
}
