package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSearxURLPrecedence(t *testing.T) {
	t.Setenv("SEARXNG_ENDPOINT", "http://from-env:9090/")
	if got := searxURL("http://configured:1234/"); got != "http://configured:1234" {
		t.Errorf("explicit config should win and lose its trailing slash: %q", got)
	}
	if got := searxURL("  "); got != "http://from-env:9090" {
		t.Errorf("should fall back to env: %q", got)
	}
	t.Setenv("SEARXNG_ENDPOINT", "")
	if got := searxURL(""); got != "http://127.0.0.1:9090" {
		t.Errorf("should fall back to the local default: %q", got)
	}
}

func TestRunWebSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format=json not requested: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"results":[
		 {"title":"First","url":"https://a.example/1","content":"alpha snippet"},
		 {"title":"Second","url":"https://b.example/2","content":"beta snippet"},
		 {"title":"Third","url":"https://c.example/3","content":"gamma snippet"}]}`))
	}))
	defer srv.Close()

	out, err := runWebSearch(srv.URL, "some query", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "First") || !strings.Contains(out, "https://a.example/1") {
		t.Errorf("result 1 missing: %s", out)
	}
	// maxResults must actually cap
	if strings.Contains(out, "Third") {
		t.Errorf("maxResults=2 not honoured: %s", out)
	}
}

func TestRunWebSearchEmptyAndBadJSON(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[]}`))
	}))
	defer empty.Close()
	if out, err := runWebSearch(empty.URL, "q", 5); err != nil || !strings.Contains(out, "No results") {
		t.Errorf("empty results should be a normal answer, got %q / %v", out, err)
	}

	// SearXNG serves HTML when the JSON format isn't enabled — the error must
	// say so rather than surfacing a parse failure
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!doctype html><html><body>nope</body></html>`))
	}))
	defer html.Close()
	_, err := runWebSearch(html.URL, "q", 5)
	if err == nil || !strings.Contains(err.Error(), "format=json") {
		t.Errorf("HTML response should hint at the settings.yml fix, got %v", err)
	}
}

// A tool failure must be handed back to the model as text, never abort the
// panelist — a panel that loses a member because a search 500'd is worse than
// one whose member answers from memory and says so.
func TestToolFailureDoesNotKillPanelist(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	dead.Close() // closed: connection refused

	var sawToolResult string
	calls := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []map[string]any `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"x\"}"}}]}}],"usage":{}}`))
			return
		}
		for _, m := range in.Messages {
			if m["role"] == "tool" {
				sawToolResult, _ = m["content"].(string)
			}
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"answered anyway"}}],"usage":{}}`))
	}))
	defer proxy.Close()

	out, err := chatWithTools(proxy.URL, "m", "sys", "user", "", 100, 20*time.Second, &usageAcc{}, dead.URL, nil)
	if err != nil {
		t.Fatalf("panelist aborted on a tool failure: %v", err)
	}
	if out != "answered anyway" {
		t.Errorf("unexpected answer: %q", out)
	}
	if !strings.Contains(sawToolResult, "Search failed") {
		t.Errorf("model should have been told the search failed, got %q", sawToolResult)
	}
}

// The loop must stop offering tools once the budget is spent, so a model that
// keeps searching is forced to conclude instead of stalling the panel.
func TestToolLoopIsBounded(t *testing.T) {
	calls, lastHadTools := 0, true
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		_, hasTools := in["tools"]
		lastHadTools = hasTools
		calls++
		w.Header().Set("Content-Type", "application/json")
		if hasTools {
			// always ask for another search
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t%d","type":"function","function":{"name":"web_search","arguments":"{\"query\":\"q\"}"}}]}}],"usage":{}}`, calls)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"forced to conclude"}}],"usage":{}}`))
	}))
	defer proxy.Close()
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[{"title":"t","url":"u","content":"c"}]}`))
	}))
	defer searx.Close()

	out, err := chatWithTools(proxy.URL, "m", "sys", "user", "", 100, 30*time.Second, &usageAcc{}, searx.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "forced to conclude" {
		t.Errorf("loop did not terminate cleanly: %q", out)
	}
	if lastHadTools {
		t.Error("final call should have been made without tools")
	}
	if calls != maxToolRounds+1 {
		t.Errorf("want %d calls (%d tool rounds + 1 forced answer), got %d", maxToolRounds+1, maxToolRounds, calls)
	}
}

func TestSearchLogDedupAndBound(t *testing.T) {
	l := &searchLog{}
	for i := 0; i < 100; i++ {
		l.add(fmt.Sprintf("q%d", i))
	}
	if got := len(l.list()); got != 40 {
		t.Errorf("searchLog should cap at 40, got %d", got)
	}
	var nilLog *searchLog
	nilLog.add("safe") // must not panic
	if nilLog.list() != nil {
		t.Error("nil searchLog should list nothing")
	}
}
