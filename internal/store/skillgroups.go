package store

// Skill groups, load counters and usage import.
//
// A group is a named bundle of skills identified by NAME, not by index row:
// the index is rebuilt by rescans and the same skill usually exists as several
// copies (a vault master, per-agent symlinks, an archive), so a group that
// pointed at row ids would rot on every reorganisation. Assigning a group to a
// provider or share endpoint expands each member name to the best readable
// copy at request time.
//
// Loads are counted per skill name per target when a model actually fetches
// the SKILL.md through the lazy-load URL — the only ground truth cfrproxy has
// about which skills are used. External usage (Hermes skill_view calls,
// Claude Code / Codex SKILL.md reads mined from session logs) is imported
// per source so the UI can rank by "used anywhere", not just "used here".

import (
	"errors"
	"os"
	"sort"
	"strings"
	"time"
)

type SkillGroup struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Color       string   `json:"color"`
	Members     []string `json:"members"` // skill names
	CreatedAt   int64    `json:"created_at"`
}

// TargetRef names a provider or share endpoint that carries an assignment.
type TargetRef struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type SkillLoad struct {
	Count  int64 `json:"count"`
	LastTS int64 `json:"last_ts"`
}

type SkillUsage struct {
	Calls    int64 `json:"calls"`
	Sessions int64 `json:"sessions"`
}

func (s *Store) migrateSkillGroups() {
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_groups (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  color TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0
)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_group_members (
  group_id INTEGER NOT NULL,
  skill_name TEXT NOT NULL,
  UNIQUE(group_id, skill_name)
)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_group_assignments (
  id INTEGER PRIMARY KEY,
  target_kind TEXT NOT NULL DEFAULT 'endpoint',
  target_id INTEGER NOT NULL,
  group_id INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  UNIQUE(target_kind, target_id, group_id)
)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_loads (
  skill_name TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_id INTEGER NOT NULL,
  count INTEGER NOT NULL DEFAULT 0,
  last_ts INTEGER NOT NULL DEFAULT 0,
  UNIQUE(skill_name, target_kind, target_id)
)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_usage_external (
  skill_name TEXT NOT NULL,
  source TEXT NOT NULL,
  calls INTEGER NOT NULL DEFAULT 0,
  sessions INTEGER NOT NULL DEFAULT 0,
  imported_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(skill_name, source)
)`)
}

// ---- groups ----

func normSkillName(n string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(n, " ", "-")))
}

func (s *Store) SkillGroups() ([]SkillGroup, error) {
	rows, err := s.db.Query(`SELECT id,name,description,color,created_at FROM skill_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillGroup
	for rows.Next() {
		var g SkillGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	rows.Close()
	for i := range out {
		out[i].Members = s.groupMembers(out[i].ID)
	}
	if out == nil {
		out = []SkillGroup{}
	}
	return out, nil
}

func (s *Store) groupMembers(id int64) []string {
	rows, err := s.db.Query(`SELECT skill_name FROM skill_group_members WHERE group_id=? ORDER BY skill_name`, id)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) SkillGroupByID(id int64) (SkillGroup, bool) {
	var g SkillGroup
	if err := s.db.QueryRow(`SELECT id,name,description,color,created_at FROM skill_groups WHERE id=?`, id).
		Scan(&g.ID, &g.Name, &g.Description, &g.Color, &g.CreatedAt); err != nil {
		return SkillGroup{}, false
	}
	g.Members = s.groupMembers(g.ID)
	return g, true
}

func (s *Store) SkillGroupByName(name string) (SkillGroup, bool) {
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM skill_groups WHERE lower(name)=lower(?)`, strings.TrimSpace(name)).Scan(&id); err != nil {
		return SkillGroup{}, false
	}
	return s.SkillGroupByID(id)
}

// SaveSkillGroup creates (ID==0) or updates a group and replaces its members.
func (s *Store) SaveSkillGroup(g *SkillGroup) error {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return errors.New("group name is required")
	}
	if strings.ContainsAny(g.Name, "/?#") {
		return errors.New("group name must not contain / ? #")
	}
	if g.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO skill_groups(name,description,color,created_at) VALUES(?,?,?,?)`,
			g.Name, g.Description, g.Color, time.Now().Unix())
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errors.New("a group with that name already exists")
			}
			return err
		}
		g.ID, _ = res.LastInsertId()
	} else {
		if _, err := s.db.Exec(`UPDATE skill_groups SET name=?,description=?,color=? WHERE id=?`, g.Name, g.Description, g.Color, g.ID); err != nil {
			return err
		}
	}
	return s.SetGroupMembers(g.ID, g.Members)
}

func (s *Store) SetGroupMembers(id int64, names []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM skill_group_members WHERE group_id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	seen := map[string]bool{}
	for _, n := range names {
		k := normSkillName(n)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if _, err := tx.Exec(`INSERT OR IGNORE INTO skill_group_members(group_id,skill_name) VALUES(?,?)`, id, k); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// AddGroupMembers / RemoveGroupMembers edit membership without replacing it.
func (s *Store) AddGroupMembers(id int64, names []string) error {
	for _, n := range names {
		if k := normSkillName(n); k != "" {
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO skill_group_members(group_id,skill_name) VALUES(?,?)`, id, k); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) RemoveGroupMembers(id int64, names []string) error {
	for _, n := range names {
		if _, err := s.db.Exec(`DELETE FROM skill_group_members WHERE group_id=? AND skill_name=?`, id, normSkillName(n)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteSkillGroup(id int64) error {
	s.db.Exec(`DELETE FROM skill_group_members WHERE group_id=?`, id)
	s.db.Exec(`DELETE FROM skill_group_assignments WHERE group_id=?`, id)
	_, err := s.db.Exec(`DELETE FROM skill_groups WHERE id=?`, id)
	return err
}

// GroupsContaining maps lowercase skill name -> group names, for the index view.
func (s *Store) GroupsContaining() map[string][]string {
	rows, err := s.db.Query(`SELECT m.skill_name, g.name FROM skill_group_members m JOIN skill_groups g ON g.id=m.group_id ORDER BY g.name`)
	if err != nil {
		return map[string][]string{}
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var sk, g string
		if rows.Scan(&sk, &g) == nil {
			out[sk] = append(out[sk], g)
		}
	}
	return out
}

// ---- group assignments ----

func (s *Store) GroupAssignmentsFor(kind string, targetID int64) []int64 {
	rows, err := s.db.Query(`SELECT group_id FROM skill_group_assignments WHERE target_kind=? AND target_id=? AND enabled=1 ORDER BY group_id`, normKind(kind), targetID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

func (s *Store) SetTargetGroups(kind string, targetID int64, groupIDs []int64) error {
	kind = normKind(kind)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM skill_group_assignments WHERE target_kind=? AND target_id=?`, kind, targetID); err != nil {
		tx.Rollback()
		return err
	}
	for _, id := range groupIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO skill_group_assignments(target_kind,target_id,group_id,enabled) VALUES(?,?,?,1)`, kind, targetID, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// TargetsUsingGroup lists the providers/endpoints a group is assigned to.
func (s *Store) TargetsUsingGroup(groupID int64) []TargetRef {
	rows, err := s.db.Query(`SELECT target_kind, target_id FROM skill_group_assignments WHERE group_id=? AND enabled=1`, groupID)
	if err != nil {
		return nil
	}
	var out []TargetRef
	for rows.Next() {
		var t TargetRef
		if rows.Scan(&t.Kind, &t.ID) == nil {
			out = append(out, t)
		}
	}
	// resolve names only after the cursor is closed: the store runs on a
	// single connection, so a nested query while rows are open would deadlock
	rows.Close()
	for i := range out {
		out[i].Name = s.targetName(out[i].Kind, out[i].ID)
	}
	return out
}

func (s *Store) targetName(kind string, id int64) string {
	if normKind(kind) == "provider" {
		if p, ok := s.ProviderByID(id); ok {
			return p.Name
		}
		return "?"
	}
	eps, _ := s.Endpoints()
	for _, e := range eps {
		if e.ID == id {
			return e.Name
		}
	}
	return "?"
}

// ---- resolving names to files ----

// BestSkillCopy picks the index row that should serve a skill name: a readable
// copy outside an archive folder, else any readable copy.
func (s *Store) BestSkillCopy(name string) (Skill, bool) {
	all, err := s.Skills()
	if err != nil {
		return Skill{}, false
	}
	return bestCopy(all, name)
}

// BestCopyIn is BestSkillCopy over an already-loaded index, for callers that
// resolve many names at once (one table read instead of one per name).
func BestCopyIn(all []Skill, name string) (Skill, bool) { return bestCopy(all, name) }

func bestCopy(all []Skill, name string) (Skill, bool) {
	want := normSkillName(name)
	var archived *Skill
	for i := range all {
		sk := all[i]
		if normSkillName(sk.Name) != want {
			continue
		}
		if _, err := os.Stat(sk.Path); err != nil {
			continue
		}
		if strings.Contains(sk.Path, string(os.PathSeparator)+"_archive") {
			if archived == nil {
				archived = &all[i]
			}
			continue
		}
		return sk, true
	}
	if archived != nil {
		return *archived, true
	}
	return Skill{}, false
}

// EffectiveSkill is one entry of a target's expanded skill list.
type EffectiveSkill struct {
	Skill
	Via     string `json:"via"`     // "direct" or the group name
	Missing bool   `json:"missing"` // named by a group but no readable copy is indexed
}

// EffectiveSkillsFor expands a target's direct assignments plus its groups.
// Direct rows win over group members of the same name; a group member with
// no readable copy is reported as Missing rather than silently dropped.
func (s *Store) EffectiveSkillsFor(kind string, targetID int64, model string) []EffectiveSkill {
	kind = normKind(kind)
	var out []EffectiveSkill
	seen := map[string]bool{}
	for _, sk := range s.SkillsForTarget(kind, targetID, model) {
		k := normSkillName(sk.Name)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, EffectiveSkill{Skill: sk, Via: "direct"})
	}
	gids := s.GroupAssignmentsFor(kind, targetID)
	if len(gids) == 0 {
		return out
	}
	all, _ := s.Skills()
	for _, gid := range gids {
		g, ok := s.SkillGroupByID(gid)
		if !ok {
			continue
		}
		for _, name := range g.Members {
			k := normSkillName(name)
			if seen[k] {
				continue
			}
			seen[k] = true
			if sk, ok := bestCopy(all, name); ok {
				out = append(out, EffectiveSkill{Skill: sk, Via: g.Name})
			} else {
				out = append(out, EffectiveSkill{Skill: Skill{Name: name}, Via: g.Name, Missing: true})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// ---- loads + external usage ----

func (s *Store) RecordSkillLoad(name, kind string, targetID int64) {
	s.db.Exec(`INSERT INTO skill_loads(skill_name,target_kind,target_id,count,last_ts) VALUES(?,?,?,1,?)
		ON CONFLICT(skill_name,target_kind,target_id) DO UPDATE SET count=count+1, last_ts=excluded.last_ts`,
		normSkillName(name), normKind(kind), targetID, time.Now().Unix())
}

// SkillLoads totals loads per skill name across all targets.
func (s *Store) SkillLoads() map[string]SkillLoad {
	rows, err := s.db.Query(`SELECT skill_name, SUM(count), MAX(last_ts) FROM skill_loads GROUP BY skill_name`)
	if err != nil {
		return map[string]SkillLoad{}
	}
	defer rows.Close()
	out := map[string]SkillLoad{}
	for rows.Next() {
		var n string
		var l SkillLoad
		if rows.Scan(&n, &l.Count, &l.LastTS) == nil {
			out[n] = l
		}
	}
	return out
}

// ImportSkillUsage replaces one source's counts (e.g. "hermes", "claude",
// "codex") with the given entries.
func (s *Store) ImportSkillUsage(source string, entries map[string]SkillUsage) error {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return errors.New("source is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM skill_usage_external WHERE source=?`, source); err != nil {
		tx.Rollback()
		return err
	}
	now := time.Now().Unix()
	for name, u := range entries {
		k := normSkillName(name)
		if k == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO skill_usage_external(skill_name,source,calls,sessions,imported_at) VALUES(?,?,?,?,?)`, k, source, u.Calls, u.Sessions, now); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// SkillUsageExternal maps skill name -> source -> usage.
func (s *Store) SkillUsageExternal() map[string]map[string]SkillUsage {
	rows, err := s.db.Query(`SELECT skill_name, source, calls, sessions FROM skill_usage_external`)
	if err != nil {
		return map[string]map[string]SkillUsage{}
	}
	defer rows.Close()
	out := map[string]map[string]SkillUsage{}
	for rows.Next() {
		var n, src string
		var u SkillUsage
		if rows.Scan(&n, &src, &u.Calls, &u.Sessions) == nil {
			if out[n] == nil {
				out[n] = map[string]SkillUsage{}
			}
			out[n][src] = u
		}
	}
	return out
}
