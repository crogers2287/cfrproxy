package api

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

// The WebUI's Save button PUTs the whole auto_router block. Everything the
// form edits — and the smart keys it does not — must come back from GET.
func TestAutoRouteSaveKeepsSmartBlock(t *testing.T) {
	dir, _ := os.MkdirTemp("", "cfrproxy-ar")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a := &API{Store: s, Proxy: proxy.New(s)}
	body := `{"enabled":true,"classifier":"x/cls","routes":{"default":"x/def"},"sticky":true,
	  "smart":{"enabled":true,"local_max_tokens":120000,"classify":"heuristic","prefer_warm":false,
	    "tiers":{"routine":["fred/tiel*","fred/ornith"],"hard":["claude/claude-fable-5"]},"vision":["fred/ornith"]}}`
	w := httptest.NewRecorder()
	a.hAutoRouteSet(w, httptest.NewRequest("PUT", "/admin/api/autoroute", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.hAutoRouteGet(w, httptest.NewRequest("GET", "/admin/api/autoroute", nil))
	var got proxy.AutoRouterConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	sm := got.Smart
	if sm == nil || !sm.Enabled || sm.LocalMaxTokens != 120000 || sm.Classify != "heuristic" ||
		sm.PreferWarm == nil || *sm.PreferWarm || len(sm.Tiers["routine"]) != 2 || sm.Tiers["routine"][0] != "fred/tiel*" ||
		len(sm.Tiers["hard"]) != 1 || len(sm.Vision) != 1 {
		t.Fatalf("smart block did not round-trip: %s", w.Body.String())
	}
	if got.Routes["default"] != "x/def" || got.Classifier != "x/cls" {
		t.Fatalf("classic fields lost: %s", w.Body.String())
	}
}
