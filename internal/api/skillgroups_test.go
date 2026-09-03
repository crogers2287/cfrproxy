package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

// Groups round-trip through the admin API and show up in a target's
// effective list and catalog preview.
func TestSkillGroupsAPIRoundTrip(t *testing.T) {
	dir, _ := os.MkdirTemp("", "cfrproxy-api-groups")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := filepath.Join(dir, "skills")
	os.MkdirAll(filepath.Join(root, "ha-mcp"), 0o755)
	os.WriteFile(filepath.Join(root, "ha-mcp", "SKILL.md"), []byte("---\nname: ha-mcp\ndescription: HA\n---\nbody\n"), 0o644)
	s.SaveSkillRoot(&store.SkillRoot{Path: root, Enabled: true})
	s.ScanSkills(4)
	ep := &store.Endpoint{Name: "team", APIKey: "k", Enabled: true}
	s.SaveEndpoint(ep)
	s.SetSetting("admin_user", "admin")
	a := &API{Store: s, Proxy: proxy.New(s)}
	a.SetPassword("pw")
	mux := http.NewServeMux()
	a.Register(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth("admin", "pw")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	rec := do("POST", "/admin/api/skill-groups", `{"name":"homelab","description":"d","members":["ha-mcp","ghost"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"missing":["ghost"]`) {
		t.Fatalf("create group: %d %s", rec.Code, rec.Body.String())
	}
	rec = do("GET", "/admin/api/skill-groups", "")
	if !strings.Contains(rec.Body.String(), `"name":"homelab"`) {
		t.Fatalf("list groups: %s", rec.Body.String())
	}
	rec = do("POST", "/admin/api/skill-assign", `{"kind":"endpoint","id":1,"skill_ids":[],"group_ids":[1]}`)
	if rec.Code != 200 {
		t.Fatalf("assign: %d %s", rec.Code, rec.Body.String())
	}
	rec = do("GET", "/admin/api/skill-assign?kind=endpoint&id=1", "")
	b := rec.Body.String()
	if !strings.Contains(b, `"group_ids":[1]`) || !strings.Contains(b, `"via":"homelab"`) || !strings.Contains(b, "load: GET") || !strings.Contains(b, "/e/team/skills/ha-mcp?t=") {
		t.Fatalf("assignment view should expand the group and preview the catalog: %s", b)
	}
	rec = do("GET", "/admin/api/skills?group=homelab", "")
	if !strings.Contains(rec.Body.String(), `"groups":["homelab"]`) || !strings.Contains(rec.Body.String(), `"exists":true`) {
		t.Fatalf("index should carry group membership and existence: %s", rec.Body.String()[:200])
	}
	rec = do("POST", "/admin/api/skills/usage-import", `{"source":"hermes","entries":{"ha-mcp":{"calls":9,"sessions":4}}}`)
	if rec.Code != 200 {
		t.Fatalf("usage import: %d %s", rec.Code, rec.Body.String())
	}
	rec = do("GET", "/admin/api/skills?used=1", "")
	if !strings.Contains(rec.Body.String(), `"hermes":{"calls":9,"sessions":4}`) || !strings.Contains(rec.Body.String(), `"score":9`) {
		t.Fatalf("usage should be on the index row: %s", rec.Body.String()[:300])
	}
	// a save that only knows skill_ids must not wipe the groups
	rec = do("POST", "/admin/api/skill-assign", `{"kind":"endpoint","id":1,"skill_ids":[]}`)
	if rec.Code != 200 {
		t.Fatalf("partial assign: %d %s", rec.Code, rec.Body.String())
	}
	if rec = do("GET", "/admin/api/skill-assign?kind=endpoint&id=1", ""); !strings.Contains(rec.Body.String(), `"group_ids":[1]`) {
		t.Fatalf("groups were wiped by a skill-only save: %s", rec.Body.String())
	}
	rec = do("DELETE", "/admin/api/skill-groups/1", "")
	if rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	rec = do("GET", "/admin/api/skill-assign?kind=endpoint&id=1", "")
	if strings.Contains(rec.Body.String(), `"via":"homelab"`) {
		t.Fatal("deleted group still expands")
	}
}
