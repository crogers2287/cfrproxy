package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// fakeSearchWorld: a SearXNG that answers any query, and an upstream whose
// first reply calls web_search and whose second reply answers with text.
func fakeSearchWorld(t *testing.T, modelSearches bool) (searx, up *httptest.Server, calls *int, mu *sync.Mutex) {
	t.Helper()
	mu = &sync.Mutex{}
	n := 0
	calls = &n
	searx = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if r.URL.Query().Get("format") != "json" || q == "" {
			w.WriteHeader(400)
			return
		}
		fmt.Fprintf(w, `{"results":[{"title":"colyseus - npm","url":"https://www.npmjs.com/package/colyseus","content":"0.16.4 · latest"},{"title":"Colyseus","url":"https://colyseus.io/","content":"multiplayer framework"}]}`)
	}))
	t.Cleanup(searx.Close)
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if modelSearches && k == 1 {
			if !strings.Contains(string(b), `"web_search"`) {
				t.Errorf("upstream should be offered the web_search function tool: %s", b)
			}
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"colyseus npm version\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":5}}`))
			return
		}
		if strings.Contains(string(b), "npmjs.com") {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"The latest colyseus release on npm is 0.16.4 (npmjs.com)."},"finish_reason":"stop"}],"usage":{"prompt_tokens":80,"completion_tokens":20}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"I believe it is 0.15."},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":8}}`))
	}))
	t.Cleanup(up.Close)
	return
}

func webSearchRequest(stream bool) string {
	return fmt.Sprintf(`{"model":"local/m","max_tokens":512,"stream":%v,
	  "system":[{"type":"text","text":"You are an assistant for performing a web search tool use"}],
	  "tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
	  "messages":[{"role":"user","content":"Perform a web search for: latest colyseus npm version"}]}`, stream)
}

func TestWebSearchEmulationModelSearches(t *testing.T) {
	searx, up, calls, mu := fakeSearchWorld(t, true)
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: up.URL, DefaultModel: "m", Priority: 10, Enabled: true})
	s.SetSetting("web_search", `{"searx":"`+searx.URL+`"}`)
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(webSearchRequest(false))))
	if rec.Code != 200 {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Content []map[string]any `json:"content"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Content) != 3 || out.Content[0]["type"] != "server_tool_use" || out.Content[1]["type"] != "web_search_tool_result" || out.Content[2]["type"] != "text" {
		t.Fatalf("content blocks: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"url":"https://www.npmjs.com/package/colyseus"`) || !strings.Contains(rec.Body.String(), "0.16.4") {
		t.Fatalf("results/answer missing: %s", rec.Body.String())
	}
	if !strings.Contains(out.Content[2]["text"].(string), "Sources:") {
		t.Fatalf("text should end with a sources list: %s", out.Content[2]["text"])
	}
	mu.Lock()
	n := *calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("model should be called twice (tool call, then answer), got %d", n)
	}
	traces, _ := s.Traces(0, 3)
	if len(traces) == 0 || !strings.HasPrefix(traces[0].Note, "web_search×1") || traces[0].Status != 200 {
		t.Fatalf("trace: %+v", traces)
	}
}

func TestWebSearchEmulationForcesOneSearchAndStreams(t *testing.T) {
	searx, up, _, _ := fakeSearchWorld(t, false) // model answers from memory
	s := newDiscoveryStore(t)
	s.SaveProvider(&store.Provider{Name: "local", Type: "openai", BaseURL: up.URL, DefaultModel: "m", Priority: 10, Enabled: true})
	s.SetSetting("web_search", `{"searx":"`+searx.URL+`"}`)
	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(webSearchRequest(true))))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("HTTP %d %s: %s", rec.Code, rec.Header().Get("Content-Type"), body)
	}
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, `npmjs.com/package/colyseus`, `"text_delta"`, "0.16.4", "message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	// off switch: the request falls through to the normal path (no blocks)
	s.SetSetting("web_search", `{"enabled":false}`)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(webSearchRequest(false))))
	if strings.Contains(rec.Body.String(), "web_search_tool_result") {
		t.Fatalf("disabled emulation must not produce search blocks: %s", rec.Body.String())
	}
}
