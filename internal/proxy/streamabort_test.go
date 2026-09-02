package proxy

import (
	"bufio"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// A client that walks away mid-stream must stop the upstream request and
// leave no goroutines behind. Before the stream writers returned write
// errors, the proxy kept pulling the upstream stream to completion for a
// client that was gone (paid tokens, a held inference slot), and the
// reader/relay goroutines could block forever on a channel nobody drained.
func TestStreamClientDisconnectStopsUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				close(upstreamCancelled)
				return
			case <-time.After(10 * time.Millisecond):
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok%d \"},\"finish_reason\":null}]}\n\n", i)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{Name: "up", Type: "openai", BaseURL: upstream.URL, DefaultModel: "m", Priority: 10, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	front := httptest.NewServer(mux)
	defer front.Close()

	before := runtime.NumGoroutine()
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"up/m","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	got := 0
	for sc.Scan() && got < 3 {
		if strings.HasPrefix(sc.Text(), "data:") {
			got++
		}
	}
	if got < 3 {
		t.Fatalf("expected streamed chunks before disconnecting, got %d", got)
	}
	resp.Body.Close() // the client walks away

	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request was not cancelled after the client disconnected")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}
