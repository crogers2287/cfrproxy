package store

// Skill index: a filesystem-backed registry of Anthropic-style SKILL.md folders
// scattered across the machine. The scanner walks configured roots, caches each
// skill's frontmatter (name/description) + location, and the proxy layer can then
// assign skills to a share endpoint and lazy-load them at request time.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SkillRoot struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
}

type Skill struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Path          string `json:"path"` // absolute path to the SKILL.md file
	RootID        int64  `json:"root_id"`
	RelDir        string `json:"rel_dir"` // dir holding SKILL.md, relative to its root
	IsSymlink     bool   `json:"is_symlink"`
	SymlinkTarget string `json:"symlink_target"`
	Size          int64  `json:"size"`
	MTime         int64  `json:"mtime"`
	SHA           string `json:"sha"`
	Tags          string `json:"tags"`
	ScannedAt     int64  `json:"scanned_at"`
}

// SkillAssignment attaches a skill to a target — a share endpoint
// (TargetKind "endpoint") OR a provider (TargetKind "provider") — optionally
// scoped to a model glob within it.
type SkillAssignment struct {
	ID         int64  `json:"id"`
	TargetKind string `json:"target_kind"` // "endpoint" | "provider"
	TargetID   int64  `json:"target_id"`
	ModelGlob  string `json:"model_glob"`
	SkillID    int64  `json:"skill_id"`
	Enabled    bool   `json:"enabled"`
}

// offLimitsPath blocks the work shares that must never be scanned or written.
func offLimitsPath(p string) bool {
	lp := strings.ToLower(p)
	return strings.Contains(lp, "filecabinet") || strings.Contains(lp, "ash-shared")
}

// ---- skill roots ----

func (s *Store) SkillRoots() ([]SkillRoot, error) {
	rows, err := s.db.Query(`SELECT id,path,label,enabled FROM skill_roots ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillRoot
	for rows.Next() {
		var r SkillRoot
		var en int
		if err := rows.Scan(&r.ID, &r.Path, &r.Label, &en); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SaveSkillRoot(r *SkillRoot) error {
	r.Path = strings.TrimSpace(r.Path)
	if r.Path == "" {
		return errors.New("skill root path is required")
	}
	abs, err := filepath.Abs(r.Path)
	if err != nil {
		return err
	}
	r.Path = abs
	if offLimitsPath(r.Path) {
		return errors.New("that path is an off-limits work share")
	}
	if r.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO skill_roots(path,label,enabled) VALUES(?,?,?)
			ON CONFLICT(path) DO UPDATE SET label=excluded.label,enabled=excluded.enabled`,
			r.Path, r.Label, b2i(r.Enabled))
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
		return nil
	}
	_, err = s.db.Exec(`UPDATE skill_roots SET path=?,label=?,enabled=? WHERE id=?`,
		r.Path, r.Label, b2i(r.Enabled), r.ID)
	return err
}

func (s *Store) DeleteSkillRoot(id int64) error {
	_, err := s.db.Exec(`DELETE FROM skill_roots WHERE id=?`, id)
	return err
}

// SeedSkillRoots inserts any candidate dirs that exist and are not off-limits,
// ignoring ones already present. Used once to bootstrap the index.
func (s *Store) SeedSkillRoots(candidates []string) {
	for _, c := range candidates {
		abs, err := filepath.Abs(strings.TrimSpace(c))
		if err != nil || abs == "" || offLimitsPath(abs) {
			continue
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			continue
		}
		s.db.Exec(`INSERT OR IGNORE INTO skill_roots(path,label,enabled) VALUES(?,?,1)`, abs, filepath.Base(abs))
	}
}

// ---- skills ----

func (s *Store) Skills() ([]Skill, error) {
	rows, err := s.db.Query(`SELECT id,name,description,path,root_id,rel_dir,is_symlink,symlink_target,size,mtime,sha,tags,scanned_at FROM skills ORDER BY name,path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkillRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *Store) SkillByID(id int64) (Skill, bool) {
	row := s.db.QueryRow(`SELECT id,name,description,path,root_id,rel_dir,is_symlink,symlink_target,size,mtime,sha,tags,scanned_at FROM skills WHERE id=?`, id)
	sk, err := scanSkillRow(row)
	if err != nil {
		return Skill{}, false
	}
	return sk, true
}

// rowScanner unifies *sql.Row and *sql.Rows for scanSkillRow.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSkillRow(r rowScanner) (Skill, error) {
	var sk Skill
	var isl int
	if err := r.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Path, &sk.RootID, &sk.RelDir,
		&isl, &sk.SymlinkTarget, &sk.Size, &sk.MTime, &sk.SHA, &sk.Tags, &sk.ScannedAt); err != nil {
		return Skill{}, err
	}
	sk.IsSymlink = isl == 1
	return sk, nil
}

// SkillsForTarget returns the skills assigned to a target (kind "endpoint" or
// "provider") whose model_glob matches the requested model (or is "*"). An empty
// model returns every assigned skill regardless of glob (used by the fetch API).
func (s *Store) SkillsForTarget(kind string, id int64, model string) []Skill {
	rows, err := s.db.Query(`SELECT s.id,s.name,s.description,s.path,s.root_id,s.rel_dir,s.is_symlink,s.symlink_target,s.size,s.mtime,s.sha,s.tags,s.scanned_at, a.model_glob
		FROM skill_assignments a JOIN skills s ON s.id=a.skill_id
		WHERE a.target_kind=? AND a.target_id=? AND a.enabled=1 ORDER BY s.name`, kind, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Skill
	seen := map[int64]bool{}
	for rows.Next() {
		var sk Skill
		var isl int
		var glob string
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Path, &sk.RootID, &sk.RelDir,
			&isl, &sk.SymlinkTarget, &sk.Size, &sk.MTime, &sk.SHA, &sk.Tags, &sk.ScannedAt, &glob); err != nil {
			continue
		}
		sk.IsSymlink = isl == 1
		// model=="" means "every skill assigned to this endpoint" (used by the
		// fetch endpoints, which have no model context); otherwise honor the glob.
		if model != "" && glob != "" && glob != "*" && !matchGlobStore(glob, model) {
			continue
		}
		if !seen[sk.ID] {
			seen[sk.ID] = true
			out = append(out, sk)
		}
	}
	return out
}

// matchGlobStore is a lowercase '*' glob match (mirrors proxy.matchGlob so the
// store has no dependency on the proxy package).
func matchGlobStore(pat, str string) bool {
	pat, str = strings.ToLower(pat), strings.ToLower(str)
	if !strings.Contains(pat, "*") {
		return pat == str
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(str, parts[0]) || !strings.HasSuffix(str, parts[len(parts)-1]) {
		return false
	}
	rest := str[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return true
}

// SkillsForEndpoint / SkillsForProvider are thin wrappers over SkillsForTarget.
func (s *Store) SkillsForEndpoint(id int64, model string) []Skill {
	return s.SkillsForTarget("endpoint", id, model)
}
func (s *Store) SkillsForProvider(id int64, model string) []Skill {
	return s.SkillsForTarget("provider", id, model)
}

// ---- assignments ----

func normKind(k string) string {
	if strings.EqualFold(k, "provider") {
		return "provider"
	}
	return "endpoint"
}

func (s *Store) SkillAssignmentsFor(kind string, targetID int64) ([]SkillAssignment, error) {
	rows, err := s.db.Query(`SELECT id,target_kind,target_id,model_glob,skill_id,enabled FROM skill_assignments WHERE target_kind=? AND target_id=? ORDER BY id`, normKind(kind), targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillAssignment
	for rows.Next() {
		var a SkillAssignment
		var en int
		if err := rows.Scan(&a.ID, &a.TargetKind, &a.TargetID, &a.ModelGlob, &a.SkillID, &en); err != nil {
			return nil, err
		}
		a.Enabled = en == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteSkillAssignment(id int64) error {
	_, err := s.db.Exec(`DELETE FROM skill_assignments WHERE id=?`, id)
	return err
}

// SetTargetSkills replaces the "*"-glob assignments for a target (provider or
// endpoint) with the given skill ids — the "assign these skills to this
// provider/API" UI action.
func (s *Store) SetTargetSkills(kind string, targetID int64, skillIDs []int64) error {
	kind = normKind(kind)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM skill_assignments WHERE target_kind=? AND target_id=? AND model_glob='*'`, kind, targetID); err != nil {
		tx.Rollback()
		return err
	}
	for _, id := range skillIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO skill_assignments(target_kind,target_id,model_glob,skill_id,enabled) VALUES(?, ?, '*', ?, 1)`, kind, targetID, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---- scanner ----

var skipDirNames = map[string]bool{
	"node_modules": true, ".git": true, ".cache": true, ".pnpm-store": true,
	"dist": true, "build": true, "vendor": true,
}

// ScanSkills walks every enabled root, upserts each SKILL.md found, and prunes
// rows whose file no longer exists. Returns the number of skills indexed.
func (s *Store) ScanSkills(maxDepth int) (int, error) {
	roots, err := s.SkillRoots()
	if err != nil {
		return 0, err
	}
	if maxDepth <= 0 {
		maxDepth = 8
	}
	now := time.Now().Unix()
	present := map[string]bool{}
	for _, root := range roots {
		if !root.Enabled || offLimitsPath(root.Path) {
			continue
		}
		rootPath := root.Path
		filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entry: skip, don't abort the walk
			}
			if d.IsDir() {
				if skipDirNames[d.Name()] || offLimitsPath(path) {
					return fs.SkipDir
				}
				if rel, _ := filepath.Rel(rootPath, path); rel != "." && strings.Count(rel, string(os.PathSeparator)) > maxDepth {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.EqualFold(d.Name(), "SKILL.md") {
				return nil
			}
			present[path] = true
			s.upsertSkill(rootPath, root.ID, path, now)
			return nil
		})
	}
	// prune vanished skills belonging to enabled roots. An assignment points
	// at an index row, so a skill that merely moved (folders get reorganised)
	// used to take its assignments with it: the endpoint's catalog silently
	// lost the skill. Re-point assignments at another indexed copy of the same
	// name first — preferring one outside an archive folder — and delete only
	// when there is nowhere to go.
	if all, err := s.Skills(); err == nil {
		byName := map[string][]Skill{}
		for _, sk := range all {
			byName[strings.ToLower(sk.Name)] = append(byName[strings.ToLower(sk.Name)], sk)
		}
		for _, sk := range all {
			if present[sk.Path] {
				continue
			}
			if _, statErr := os.Lstat(sk.Path); statErr == nil {
				continue
			}
			if alt, ok := pickSurvivor(byName[strings.ToLower(sk.Name)], sk.ID, present); ok {
				s.db.Exec(`UPDATE skill_assignments SET skill_id=? WHERE skill_id=?`, alt.ID, sk.ID)
			}
			s.db.Exec(`DELETE FROM skills WHERE id=?`, sk.ID)
		}
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM skills`).Scan(&n)
	return n, nil
}

// pickSurvivor chooses the indexed copy of a name that should inherit a
// vanished row's assignments: a present copy outside an archive folder, else
// any present copy.
func pickSurvivor(copies []Skill, goneID int64, present map[string]bool) (Skill, bool) {
	var archived *Skill
	for i := range copies {
		c := copies[i]
		if c.ID == goneID || !present[c.Path] {
			continue
		}
		if strings.Contains(c.Path, string(os.PathSeparator)+"_archive") {
			if archived == nil {
				archived = &copies[i]
			}
			continue
		}
		return c, true
	}
	if archived != nil {
		return *archived, true
	}
	return Skill{}, false
}

func (s *Store) upsertSkill(rootPath string, rootID int64, path string, now int64) {
	li, err := os.Lstat(path)
	if err != nil {
		return
	}
	isLink := li.Mode()&os.ModeSymlink != 0
	target := ""
	if isLink {
		target, _ = os.Readlink(path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name, desc, tags := parseFrontmatter(body)
	if name == "" {
		name = filepath.Base(filepath.Dir(path)) // fall back to the folder name
	}
	sum := sha256.Sum256(body)
	fi, _ := os.Stat(path)
	var size, mtime int64
	if fi != nil {
		size, mtime = fi.Size(), fi.ModTime().Unix()
	}
	relDir, _ := filepath.Rel(rootPath, filepath.Dir(path))
	s.db.Exec(`INSERT INTO skills(name,description,path,root_id,rel_dir,is_symlink,symlink_target,size,mtime,sha,tags,scanned_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
		  name=excluded.name,description=excluded.description,root_id=excluded.root_id,rel_dir=excluded.rel_dir,
		  is_symlink=excluded.is_symlink,symlink_target=excluded.symlink_target,size=excluded.size,
		  mtime=excluded.mtime,sha=excluded.sha,tags=excluded.tags,scanned_at=excluded.scanned_at`,
		name, desc, path, rootID, relDir, b2i(isLink), target, size, mtime, hex.EncodeToString(sum[:]), tags, now)
}

// parseFrontmatter extracts name/description/tags from a leading YAML `---` block
// without a YAML dependency. It handles single-line `key: value` pairs (quoted or
// bare); good enough for the SKILL.md contract (name + one-line description).
func parseFrontmatter(body []byte) (name, desc, tags string) {
	txt := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(strings.TrimLeft(txt, "\n"), "---") {
		return "", "", ""
	}
	txt = strings.TrimLeft(txt, "\n")
	rest := strings.TrimPrefix(txt, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", ""
	}
	fm := rest[:end]
	for _, line := range strings.Split(fm, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, `"'`)
		switch strings.ToLower(key) {
		case "name":
			name = val
		case "description":
			desc = val
		case "tags", "keywords":
			tags = val
		}
	}
	return name, desc, tags
}

// ---- content read/write + distribute (all guarded to stay inside a root) ----

func (s *Store) pathInEnabledRoot(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil || offLimitsPath(abs) {
		return false
	}
	roots, _ := s.SkillRoots()
	for _, r := range roots {
		if !r.Enabled {
			continue
		}
		rp := strings.TrimRight(r.Path, string(os.PathSeparator))
		if abs == rp || strings.HasPrefix(abs, rp+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// SkillContent returns the SKILL.md path and body for a skill id.
func (s *Store) SkillContent(id int64) (string, string, error) {
	sk, ok := s.SkillByID(id)
	if !ok {
		return "", "", errors.New("no such skill")
	}
	b, err := os.ReadFile(sk.Path)
	if err != nil {
		return sk.Path, "", err
	}
	return sk.Path, string(b), nil
}

// WriteSkillContent overwrites a skill's SKILL.md in place after backing it up.
// Refuses any path outside an enabled root.
func (s *Store) WriteSkillContent(id int64, body string) error {
	sk, ok := s.SkillByID(id)
	if !ok {
		return errors.New("no such skill")
	}
	if !s.pathInEnabledRoot(sk.Path) {
		return errors.New("refusing to write outside a configured skill root")
	}
	if old, err := os.ReadFile(sk.Path); err == nil {
		bak := fmt.Sprintf("%s.bak-%d", sk.Path, time.Now().Unix())
		os.WriteFile(bak, old, 0o644)
	}
	if err := os.WriteFile(sk.Path, []byte(body), 0o644); err != nil {
		return err
	}
	s.upsertSkill(rootPathFor(s, sk.RootID), sk.RootID, sk.Path, time.Now().Unix())
	return nil
}

func rootPathFor(s *Store, rootID int64) string {
	roots, _ := s.SkillRoots()
	for _, r := range roots {
		if r.ID == rootID {
			return r.Path
		}
	}
	return ""
}

// SymlinkSkill links a skill's folder into a destination root under the same
// folder name (the "lazy-load into an agent's skills dir" action). destRootID
// names an enabled root; the link lands directly inside it.
func (s *Store) SymlinkSkill(id, destRootID int64) (string, error) {
	return s.placeSkill(id, destRootID, true)
}

// CopySkill copies a skill's folder into a destination root.
func (s *Store) CopySkill(id, destRootID int64) (string, error) {
	return s.placeSkill(id, destRootID, false)
}

func (s *Store) placeSkill(id, destRootID int64, symlink bool) (string, error) {
	sk, ok := s.SkillByID(id)
	if !ok {
		return "", errors.New("no such skill")
	}
	dest := rootPathFor(s, destRootID)
	if dest == "" || !s.pathInEnabledRoot(dest) {
		return "", errors.New("destination is not an enabled skill root")
	}
	srcDir := filepath.Dir(sk.Path)
	name := filepath.Base(srcDir)
	target := filepath.Join(dest, name)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%q already exists in the destination root", name)
	}
	if symlink {
		if err := os.Symlink(srcDir, target); err != nil {
			return "", err
		}
	} else {
		if err := copyTree(srcDir, target); err != nil {
			return "", err
		}
	}
	return target, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}
