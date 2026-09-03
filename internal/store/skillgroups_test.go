package store

import (
	"os"
	"path/filepath"
	"testing"
)

func groupFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "cfrproxy-skillgroups")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := filepath.Join(dir, "skills")
	for _, n := range []string{"alpha", "beta", "_archive-old/gamma"} {
		p := filepath.Join(root, n)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "SKILL.md"), []byte("---\nname: "+filepath.Base(n)+"\ndescription: d\n---\n# "+n+"\n"), 0o644)
	}
	s.SaveSkillRoot(&SkillRoot{Path: root, Enabled: true})
	if _, err := s.ScanSkills(4); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestGroupsExpandByNameAndReportMissing(t *testing.T) {
	s, _ := groupFixture(t)
	g := &SkillGroup{Name: "ops", Description: "ops bundle", Members: []string{"Alpha", "gamma", "nowhere"}}
	if err := s.SaveSkillGroup(g); err != nil {
		t.Fatal(err)
	}
	if g2, _ := s.SkillGroupByName("OPS"); len(g2.Members) != 3 {
		t.Fatalf("members not saved: %+v", g2)
	}
	ep := &Endpoint{Name: "team", APIKey: "k", Enabled: true}
	s.SaveEndpoint(ep)
	all, _ := s.Skills()
	var beta int64
	for _, sk := range all {
		if sk.Name == "beta" {
			beta = sk.ID
		}
	}
	s.SetTargetSkills("endpoint", ep.ID, []int64{beta})
	if err := s.SetTargetGroups("endpoint", ep.ID, []int64{g.ID}); err != nil {
		t.Fatal(err)
	}
	eff := s.EffectiveSkillsFor("endpoint", ep.ID, "")
	got := map[string]EffectiveSkill{}
	for _, e := range eff {
		got[e.Name] = e
	}
	if len(eff) != 4 || got["beta"].Via != "direct" || got["alpha"].Via != "ops" || got["gamma"].Via != "ops" || !got["nowhere"].Missing {
		t.Fatalf("unexpected expansion: %+v", eff)
	}
	if got["gamma"].Path == "" || !filepathHas(got["gamma"].Path, "_archive-old") {
		t.Fatalf("archived copy should still resolve when it is the only one: %+v", got["gamma"])
	}
	if ts := s.TargetsUsingGroup(g.ID); len(ts) != 1 || ts[0].Name != "team" {
		t.Fatalf("targets using group: %+v", ts)
	}
	if err := s.RemoveGroupMembers(g.ID, []string{"nowhere"}); err != nil {
		t.Fatal(err)
	}
	if eff := s.EffectiveSkillsFor("endpoint", ep.ID, ""); len(eff) != 3 {
		t.Fatalf("after removing the missing member: %+v", eff)
	}
	if err := s.DeleteSkillGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if eff := s.EffectiveSkillsFor("endpoint", ep.ID, ""); len(eff) != 1 || eff[0].Name != "beta" {
		t.Fatalf("deleting a group should leave direct assignments: %+v", eff)
	}
}

func TestSkillLoadsAndExternalUsage(t *testing.T) {
	s, _ := groupFixture(t)
	s.RecordSkillLoad("Alpha", "endpoint", 1)
	s.RecordSkillLoad("alpha", "endpoint", 1)
	s.RecordSkillLoad("alpha", "provider", 2)
	if l := s.SkillLoads()["alpha"]; l.Count != 3 || l.LastTS == 0 {
		t.Fatalf("loads: %+v", l)
	}
	if err := s.ImportSkillUsage("hermes", map[string]SkillUsage{"alpha": {Calls: 10, Sessions: 4}, "Beta": {Calls: 2, Sessions: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportSkillUsage("hermes", map[string]SkillUsage{"alpha": {Calls: 11, Sessions: 5}}); err != nil {
		t.Fatal(err)
	}
	u := s.SkillUsageExternal()
	if u["alpha"]["hermes"].Calls != 11 || len(u["beta"]) != 0 {
		t.Fatalf("re-import should replace the source: %+v", u)
	}
}

func filepathHas(p, part string) bool {
	return len(p) > 0 && len(part) > 0 && (filepath.ToSlash(p) != "" && contains(filepath.ToSlash(p), part))
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
