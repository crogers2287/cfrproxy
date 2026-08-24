package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	body := []byte("---\nname: pdf-fill\ndescription: \"Fill PDF forms from a data map.\"\ntags: docs, pdf\n---\n\n# Body\ndo stuff\n")
	name, desc, tags := parseFrontmatter(body)
	if name != "pdf-fill" {
		t.Fatalf("name = %q", name)
	}
	if desc != "Fill PDF forms from a data map." {
		t.Fatalf("desc = %q", desc)
	}
	if tags != "docs, pdf" {
		t.Fatalf("tags = %q", tags)
	}
	if n, _, _ := parseFrontmatter([]byte("no frontmatter here")); n != "" {
		t.Fatalf("expected empty name for body without frontmatter, got %q", n)
	}
}

func TestScanAndPrune(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "skills")
	skA := filepath.Join(root, "alpha")
	if err := os.MkdirAll(skA, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(skA, "SKILL.md"), []byte("---\nname: alpha\ndescription: the alpha skill\n---\nbody\n"), 0o644)
	// a node_modules dir must be skipped
	nm := filepath.Join(root, "node_modules", "pkg")
	os.MkdirAll(nm, 0o755)
	os.WriteFile(filepath.Join(nm, "SKILL.md"), []byte("---\nname: ignored\ndescription: x\n---\n"), 0o644)

	sr := &SkillRoot{Path: root, Enabled: true}
	if err := s.SaveSkillRoot(sr); err != nil {
		t.Fatal(err)
	}
	n, err := s.ScanSkills(8)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("scanned %d skills, want 1 (node_modules must be skipped)", n)
	}
	skills, _ := s.Skills()
	if len(skills) != 1 || skills[0].Name != "alpha" {
		t.Fatalf("unexpected skills: %+v", skills)
	}

	// editing outside a root is refused; inside is allowed and keeps a .bak
	id := skills[0].ID
	if err := s.WriteSkillContent(id, "---\nname: alpha\ndescription: edited\n---\nnew body\n"); err != nil {
		t.Fatalf("write inside root should succeed: %v", err)
	}
	if !hasBackup(skA) {
		t.Fatalf("expected a .bak file after edit")
	}
	if !s.pathInEnabledRoot(filepath.Join(root, "x")) {
		t.Fatalf("path inside root should be allowed")
	}
	if s.pathInEnabledRoot(filepath.Join(dir, "elsewhere", "x")) {
		t.Fatalf("path outside every root must be refused")
	}

	// removing the file and rescanning prunes the row
	os.RemoveAll(skA)
	n, _ = s.ScanSkills(8)
	if n != 0 {
		t.Fatalf("after removal, scanned %d, want 0 (prune failed)", n)
	}
}

func hasBackup(dir string) bool {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) != "" && len(e.Name()) > len("SKILL.md.bak-") && e.Name()[:len("SKILL.md.bak-")] == "SKILL.md.bak-" {
			return true
		}
	}
	return false
}
