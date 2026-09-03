package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// A skill's load URL from the catalog must work as a plain GET: agents whose
// only HTTP tool cannot set headers (a URL "Read" tool) have no way to send
// the endpoint key, and that was the whole lazy-load path failing with 401.
func TestSkillLoadURLAuthorizesItself(t *testing.T) {
	s := newDiscoveryStore(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "ha-mcp"), 0o755)
	os.WriteFile(filepath.Join(root, "ha-mcp", "SKILL.md"), []byte("---\nname: ha-mcp\ndescription: Home Assistant via MCP\n---\n# ha-mcp\nDo the thing.\n"), 0o644)
	if err := s.SaveSkillRoot(&store.SkillRoot{Path: root, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScanSkills(4); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Skills()
	var skills []store.Skill
	for _, sk := range all {
		if sk.Name == "ha-mcp" && strings.HasPrefix(sk.Path, root) {
			skills = append(skills, sk)
		}
	}
	if len(skills) != 1 {
		t.Fatalf("expected the fixture skill to be indexed once, got %d of %d: %+v", len(skills), len(all), all)
	}
	ep := &store.Endpoint{Name: "team", APIKey: "cfr_team", Enabled: true}
	if err := s.SaveEndpoint(ep); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTargetSkills("endpoint", ep.ID, []int64{skills[0].ID}); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer up.Close()
	s.SaveProvider(&store.Provider{Name: "prov", Type: "openai", BaseURL: up.URL, DefaultModel: "m", Enabled: true})
	prov, _ := s.ProviderByName("prov")
	s.SetTargetSkills("provider", prov.ID, []int64{skills[0].ID})

	p := New(s)
	mux := http.NewServeMux()
	p.Register(mux)
	get := func(path string, hdr map[string]string, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if remote != "" {
			r.RemoteAddr = remote
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec
	}

	// the catalog the model sees carries a tokenized load URL
	cat := skillCatalog(s.SkillsForEndpoint(ep.ID, ""), "https://x/e/team", func(n string) string { return p.skillToken("e:team", n) })
	if !strings.Contains(cat, "GET https://x/e/team/skills/ha-mcp?t=") {
		t.Fatalf("catalog lacks a tokenized load URL:\n%s", cat)
	}
	tok := p.skillToken("e:team", "ha-mcp")

	if rec := get("/e/team/skills/ha-mcp", nil, ""); rec.Code != 401 {
		t.Fatalf("bare GET without key or token: want 401, got %d", rec.Code)
	}
	if rec := get("/e/team/skills/ha-mcp?t="+tok, nil, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Do the thing") {
		t.Fatalf("tokenized GET: want 200 with the SKILL.md, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/e/team/skills/ha-mcp?t="+tok[:31]+"0", nil, ""); rec.Code != 401 {
		t.Fatalf("wrong token: want 401, got %d", rec.Code)
	}
	if rec := get("/e/team/skills/other?t="+tok, nil, ""); rec.Code == 200 {
		t.Fatal("a token for one skill must not open another")
	}
	if rec := get("/e/team/skills/ha-mcp", map[string]string{"Authorization": "Bearer cfr_team"}, ""); rec.Code != 200 {
		t.Fatalf("endpoint key still works: want 200, got %d", rec.Code)
	}
	// the list endpoint (key-authed) hands out the same tokenized URLs
	if rec := get("/e/team/skills", map[string]string{"Authorization": "Bearer cfr_team"}, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "/e/team/skills/ha-mcp?t="+tok) {
		t.Fatalf("list should carry tokenized load URLs: %d %s", rec.Code, rec.Body.String())
	}
	// provider mounts: a public peer with the token but no key gets the skill
	s.SetSetting("public_api_keys", "cfr_pub")
	ptok := p.skillToken("p:prov", "ha-mcp")
	if rec := get("/p/prov/skills/ha-mcp?t="+ptok, nil, "203.0.113.9:4000"); rec.Code != 200 {
		t.Fatalf("provider-mount tokenized GET from a public peer: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/p/prov/skills/ha-mcp", nil, "203.0.113.9:4000"); rec.Code != 401 {
		t.Fatalf("provider-mount bare GET from a public peer: want 401, got %d", rec.Code)
	}
}
