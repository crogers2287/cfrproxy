package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A skill folder that moves inside its root keeps its assignments: the
// rescan re-points them at the copy that still exists instead of deleting
// the row and silently dropping the skill from the endpoint's catalog.
func TestRescanRepointsAssignmentsWhenSkillMoves(t *testing.T) {
	dir, _ := os.MkdirTemp("", "cfrproxy-skillmove")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	root := filepath.Join(dir, "skills")
	old := filepath.Join(root, "ha-mcp")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("---\nname: ha-mcp\ndescription: d\n---\nbody\n"), 0o644)
	if err := s.SaveSkillRoot(&SkillRoot{Path: root, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScanSkills(4); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Skills()
	if len(all) != 1 {
		t.Fatalf("want 1 skill, got %d", len(all))
	}
	ep := &Endpoint{Name: "team", APIKey: "k", Enabled: true}
	s.SaveEndpoint(ep)
	if err := s.SetTargetSkills("endpoint", ep.ID, []int64{all[0].ID}); err != nil {
		t.Fatal(err)
	}
	// reorganise: the folder moves under an archive, and a fresh copy appears elsewhere
	os.MkdirAll(filepath.Join(root, "_archive-x"), 0o755)
	os.Rename(old, filepath.Join(root, "_archive-x", "ha-mcp"))
	if _, err := s.ScanSkills(4); err != nil {
		t.Fatal(err)
	}
	got := s.SkillsForEndpoint(ep.ID, "")
	if len(got) != 1 || got[0].Path != filepath.Join(root, "_archive-x", "ha-mcp", "SKILL.md") {
		t.Fatalf("assignment should follow the moved skill, got %+v", got)
	}
	live := filepath.Join(root, "home", "ha-mcp")
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "SKILL.md"), []byte("---\nname: ha-mcp\ndescription: d\n---\nbody\n"), 0o644)
	os.RemoveAll(filepath.Join(root, "_archive-x"))
	if _, err := s.ScanSkills(4); err != nil {
		t.Fatal(err)
	}
	got = s.SkillsForEndpoint(ep.ID, "")
	if len(got) != 1 || got[0].Path != filepath.Join(live, "SKILL.md") {
		t.Fatalf("assignment should prefer the non-archived copy, got %+v", got)
	}
}
