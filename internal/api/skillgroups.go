package api

// Skill groups, target assignment (skills + groups), usage import and the
// enriched index listing for the Skills tab.

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// skillView is one index row plus everything the UI needs to judge it: does
// the file still exist, how often is it used (here and elsewhere), which
// groups carry it, how many copies share its name.
type skillView struct {
	store.Skill
	Exists bool                        `json:"exists"`
	Copies int                         `json:"copies"`
	Groups []string                    `json:"groups"`
	Loads  store.SkillLoad             `json:"loads"`
	Usage  map[string]store.SkillUsage `json:"usage"`
	Score  int64                       `json:"score"` // loads + external calls, for sorting
}

func (a *API) skillViews() ([]skillView, error) {
	skills, err := a.Store.Skills()
	if err != nil {
		return nil, err
	}
	loads := a.Store.SkillLoads()
	usage := a.Store.SkillUsageExternal()
	groups := a.Store.GroupsContaining()
	copies := map[string]int{}
	for _, s := range skills {
		copies[strings.ToLower(s.Name)]++
	}
	out := make([]skillView, 0, len(skills))
	for _, s := range skills {
		k := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s.Name), " ", "-"))
		v := skillView{Skill: s, Copies: copies[strings.ToLower(s.Name)], Groups: groups[k], Loads: loads[k], Usage: usage[k]}
		if v.Groups == nil {
			v.Groups = []string{}
		}
		if v.Usage == nil {
			v.Usage = map[string]store.SkillUsage{}
		}
		if _, err := os.Stat(s.Path); err == nil {
			v.Exists = true
		}
		v.Score = v.Loads.Count
		for _, u := range v.Usage {
			v.Score += u.Calls
		}
		out = append(out, v)
	}
	return out, nil
}

// hSkillsListRich replaces the bare listing: same filters (q, root) plus
// missing=1 / used=1 / group=NAME.
func (a *API) hSkillsListRich(w http.ResponseWriter, r *http.Request) {
	views, err := a.skillViews()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	root, _ := strconv.ParseInt(r.URL.Query().Get("root"), 10, 64)
	onlyMissing := r.URL.Query().Get("missing") == "1"
	onlyUsed := r.URL.Query().Get("used") == "1"
	group := strings.ToLower(r.URL.Query().Get("group"))
	out := make([]skillView, 0, len(views))
	for _, v := range views {
		if root != 0 && v.RootID != root {
			continue
		}
		if onlyMissing && v.Exists {
			continue
		}
		if onlyUsed && v.Score == 0 {
			continue
		}
		if group != "" {
			hit := false
			for _, g := range v.Groups {
				if strings.ToLower(g) == group {
					hit = true
				}
			}
			if !hit {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(v.Name+" "+v.Description+" "+v.Path+" "+v.Tags), q) {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

// ---- groups ----

type groupView struct {
	store.SkillGroup
	Targets []store.TargetRef `json:"targets"`
	Missing []string          `json:"missing"` // members with no readable indexed copy
}

func (a *API) groupView(g store.SkillGroup, all []store.Skill) groupView {
	v := groupView{SkillGroup: g, Targets: a.Store.TargetsUsingGroup(g.ID), Missing: []string{}}
	if v.Targets == nil {
		v.Targets = []store.TargetRef{}
	}
	if all == nil {
		all, _ = a.Store.Skills()
	}
	for _, m := range g.Members {
		if _, ok := store.BestCopyIn(all, m); !ok {
			v.Missing = append(v.Missing, m)
		}
	}
	return v
}

func (a *API) hSkillGroupsList(w http.ResponseWriter, r *http.Request) {
	groups, err := a.Store.SkillGroups()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	all, _ := a.Store.Skills()
	out := make([]groupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, a.groupView(g, all))
	}
	writeJSON(w, 200, out)
}

func (a *API) hSkillGroupSave(w http.ResponseWriter, r *http.Request) {
	var g store.SkillGroup
	if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if ids := r.PathValue("id"); ids != "" {
		g.ID, _ = strconv.ParseInt(ids, 10, 64)
		if g.Members == nil { // PUT without members keeps them
			if cur, ok := a.Store.SkillGroupByID(g.ID); ok {
				g.Members = cur.Members
			}
		}
	} else {
		g.ID = 0
	}
	if err := a.Store.SaveSkillGroup(&g); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	saved, _ := a.Store.SkillGroupByID(g.ID)
	writeJSON(w, 200, a.groupView(saved, nil))
}

func (a *API) hSkillGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteSkillGroup(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// hSkillGroupMembers adds/removes members: {"add":[names],"remove":[names]}.
func (a *API) hSkillGroupMembers(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if _, ok := a.Store.SkillGroupByID(id); !ok {
		httpErr(w, 404, "no such group")
		return
	}
	if err := a.Store.AddGroupMembers(id, body.Add); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if err := a.Store.RemoveGroupMembers(id, body.Remove); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	g, _ := a.Store.SkillGroupByID(id)
	writeJSON(w, 200, a.groupView(g, nil))
}

// ---- targets ----

// hSkillTargets lists every provider and share endpoint with what it carries.
func (a *API) hSkillTargets(w http.ResponseWriter, r *http.Request) {
	type target struct {
		store.TargetRef
		Enabled bool `json:"enabled"`
		Skills  int  `json:"skills"`
		Groups  int  `json:"groups"`
		Missing int  `json:"missing"`
	}
	var out []target
	add := func(kind string, id int64, name string, enabled bool) {
		eff := a.Store.EffectiveSkillsFor(kind, id, "")
		t := target{TargetRef: store.TargetRef{Kind: kind, ID: id, Name: name}, Enabled: enabled, Groups: len(a.Store.GroupAssignmentsFor(kind, id))}
		for _, e := range eff {
			if e.Missing {
				t.Missing++
			} else {
				t.Skills++
			}
		}
		out = append(out, t)
	}
	for _, p := range a.Store.Providers() {
		add("provider", p.ID, p.Name, p.Enabled)
	}
	eps, _ := a.Store.Endpoints()
	for _, e := range eps {
		add("endpoint", e.ID, e.Name, e.Enabled)
	}
	if out == nil {
		out = []target{}
	}
	writeJSON(w, 200, out)
}

// hSkillAssignGetRich: what a target carries — direct skill ids, group ids,
// the expanded effective list, and the catalog text its requests receive.
func (a *API) hSkillAssignGetRich(w http.ResponseWriter, r *http.Request) {
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
	gids := a.Store.GroupAssignmentsFor(kind, id)
	if gids == nil {
		gids = []int64{}
	}
	eff := a.Store.EffectiveSkillsFor(kind, id, "")
	if eff == nil {
		eff = []store.EffectiveSkill{}
	}
	base := "https://" + r.Host
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		base = "http://" + r.Host
	}
	preview := ""
	if a.Proxy != nil {
		preview = a.Proxy.CatalogPreview(kind, id, base)
	}
	writeJSON(w, 200, map[string]any{"skill_ids": ids, "group_ids": gids, "effective": eff, "catalog": preview})
}

func (a *API) hSkillAssignSetRich(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind     string  `json:"kind"`
		ID       int64   `json:"id"`
		SkillIDs []int64 `json:"skill_ids"`
		GroupIDs []int64 `json:"group_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	// A field that is absent leaves that half of the assignment alone: an
	// older page (or a script) that only knows skill_ids must not wipe the
	// groups, and vice versa. An explicit empty list still clears.
	if body.SkillIDs != nil {
		if err := a.Store.SetTargetSkills(body.Kind, body.ID, body.SkillIDs); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
	}
	if body.GroupIDs != nil {
		if err := a.Store.SetTargetGroups(body.Kind, body.ID, body.GroupIDs); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
	}
	eff := a.Store.EffectiveSkillsFor(body.Kind, body.ID, "")
	writeJSON(w, 200, map[string]any{"ok": true, "effective": len(eff)})
}

// ---- usage import ----

// hSkillUsageImport: {"source":"hermes","entries":{"name":{"calls":N,"sessions":M}}}
func (a *API) hSkillUsageImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source  string                      `json:"source"`
		Entries map[string]store.SkillUsage `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.ImportSkillUsage(body.Source, body.Entries); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "source": body.Source, "count": len(body.Entries)})
}
