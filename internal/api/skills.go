package api

// Admin API for the skill index: list/search skills, view+edit SKILL.md, manage
// scan roots, rescan the filesystem, distribute (symlink/copy) a skill into
// another root, and assign skills to share endpoints. Mirrors the routers/fusions
// handler shape (api.go).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func (a *API) hSkillsList(w http.ResponseWriter, r *http.Request) {
	skills, err := a.Store.Skills()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	root := r.URL.Query().Get("root")
	out := make([]store.Skill, 0, len(skills))
	for _, s := range skills {
		if root != "" {
			if id, _ := strconv.ParseInt(root, 10, 64); id != 0 && s.RootID != id {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(s.Name+" "+s.Description+" "+s.Path), q) {
			continue
		}
		out = append(out, s)
	}
	writeJSON(w, 200, out)
}

func (a *API) hSkillGet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sk, ok := a.Store.SkillByID(id)
	if !ok {
		httpErr(w, 404, "no such skill")
		return
	}
	_, content, _ := a.Store.SkillContent(id)
	writeJSON(w, 200, map[string]any{"skill": sk, "content": content})
}

func (a *API) hSkillSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.WriteSkillContent(id, body.Content); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	sk, _ := a.Store.SkillByID(id)
	writeJSON(w, 200, sk)
}

func (a *API) hSkillRescan(w http.ResponseWriter, r *http.Request) {
	// bootstrap roots on first run so there is something to scan
	if roots, _ := a.Store.SkillRoots(); len(roots) == 0 {
		a.Store.SeedSkillRoots(defaultSkillRootCandidates())
	}
	n, err := a.Store.ScanSkills(8)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": n})
}

func (a *API) hSkillDistribute(w http.ResponseWriter, r *http.Request, symlink bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		DestRootID int64 `json:"dest_root_id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var (
		target string
		err    error
	)
	if symlink {
		target, err = a.Store.SymlinkSkill(id, body.DestRootID)
	} else {
		target, err = a.Store.CopySkill(id, body.DestRootID)
	}
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	a.Store.ScanSkills(8) // pick up the new copy/link
	writeJSON(w, 200, map[string]any{"ok": true, "target": target})
}

// ---- scan roots ----

func (a *API) hSkillRootsList(w http.ResponseWriter, r *http.Request) {
	roots, err := a.Store.SkillRoots()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if roots == nil {
		roots = []store.SkillRoot{}
	}
	writeJSON(w, 200, roots)
}

func (a *API) hSkillRootSave(w http.ResponseWriter, r *http.Request) {
	var rt store.SkillRoot
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.SaveSkillRoot(&rt); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, rt)
}

func (a *API) hSkillRootDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteSkillRoot(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- per-target assignment (kind = "provider" | "endpoint") ----

func (a *API) hSkillAssignGet(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	asgs, err := a.Store.SkillAssignmentsFor(kind, id)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(asgs))
	for _, x := range asgs {
		ids = append(ids, x.SkillID)
	}
	writeJSON(w, 200, map[string]any{"skill_ids": ids})
}

func (a *API) hSkillAssignSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind     string  `json:"kind"`
		ID       int64   `json:"id"`
		SkillIDs []int64 `json:"skill_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.SetTargetSkills(body.Kind, body.ID, body.SkillIDs); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// defaultSkillRootCandidates is the first-run seed: known skill homes on this
// machine. Non-existent ones are skipped by SeedSkillRoots; users manage the list
// from the UI afterward.
func defaultSkillRootCandidates() []string {
	home, _ := os.UserHomeDir()
	rel := []string{
		"vault/Skills",
		".claude/skills",
		".agents/skills",
		".hermes/hermes-agent/skills",
		".hermes/hermes-agent/optional-skills",
		".hermes/skills",
		"wm/hermes-smart-router/skills",
		"wm/hermes-smart-router/optional-skills",
		".metaclaw/skills",
		".skillclaw/skills",
		"openclaw-skills",
		"skills",
	}
	out := make([]string, 0, len(rel))
	for _, p := range rel {
		out = append(out, filepath.Join(home, p))
	}
	return out
}
