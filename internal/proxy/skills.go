package proxy

// Skill lazy-loading on share endpoints. When an endpoint has skills assigned,
// handleCore injects a compact catalog (name + description + a fetch URL) into the
// system prompt; the model pulls a skill's full SKILL.md from the fetch endpoint
// only when a task needs it (progressive disclosure), instead of every skill
// living in every context.

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// reqBase returns the public origin the client reached us on (honoring a
// terminating proxy's X-Forwarded-Proto), e.g. "https://api.example.com".
func reqBase(r *http.Request) string {
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// skillCatalog builds the system-prompt block advertising an endpoint's skills.
// Returns "" when there are none.
// skill load URLs carry a capability token (?t=) so an agent whose only HTTP
// tool is a plain GET — no headers, no way to attach the endpoint key — can
// still fetch the SKILL.md the catalog points at. Header auth keeps working
// for agents that have it. See store.SkillToken for what the token grants.
func (p *Proxy) skillToken(scope, skill string) string {
	return p.Store.SkillToken(scope, skillURLName(skill))
}

func (p *Proxy) skillTokenOK(r *http.Request, scope, skill string) bool {
	t := r.URL.Query().Get("t")
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(p.skillToken(scope, skill))) == 1
}

// skillByToken resolves WHICH skill a load URL's token was minted for,
// regardless of the name in the path. Models re-type the URL from the
// catalog and mangle unusual names ("cfrfl-email" became "cflfl-email"); the
// token survives copy-paste intact, and it is the capability, so it decides.
func (p *Proxy) skillByToken(r *http.Request, scope string, skills []store.Skill) (store.Skill, bool) {
	t := r.URL.Query().Get("t")
	if t == "" {
		return store.Skill{}, false
	}
	for _, s := range skills {
		if subtle.ConstantTimeCompare([]byte(t), []byte(p.skillToken(scope, s.Name))) == 1 {
			return s, true
		}
	}
	return store.Skill{}, false
}

// nearestSkill tolerates small typos in a key-authenticated fetch: one
// assigned skill within edit distance 2 of the requested name.
func nearestSkill(skills []store.Skill, want string) (store.Skill, bool) {
	want = strings.ToLower(skillURLName(want))
	best, bestD := store.Skill{}, 3
	for _, s := range skills {
		if d := editDistance(strings.ToLower(skillURLName(s.Name)), want); d < bestD {
			best, bestD = s, d
		}
	}
	return best, bestD <= 2
}

func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := 1
			if a[i-1] == b[j-1] {
				c = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+c)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func logSkillFetch(r *http.Request, scope, want, served, outcome string) {
	Log.Info("skill", "path", r.URL.Path, "scope", scope, "requested", want, "served", served, "outcome", outcome)
}

func skillCatalog(skills []store.Skill, epBase string, tok func(name string) string) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("You have access to the skills below. Each is a packaged set of instructions for a specific kind of task. Only the name and a short description are shown here to save context. When the current task matches a skill, load its full instructions FIRST by fetching the URL shown (a plain HTTP GET, no authentication needed — the URL carries its own token — returns the skill's SKILL.md), then follow them. Do not guess a skill's contents.\n\n")
	seen := map[string]bool{} // one line per skill name, even if several copies are assigned
	for _, s := range skills {
		key := strings.ToLower(skillURLName(s.Name))
		if seen[key] {
			continue
		}
		seen[key] = true
		desc := s.Description
		if desc == "" {
			desc = "(no description)"
		}
		b.WriteString("- **" + s.Name + "**: " + desc + "\n")
		b.WriteString("  load: GET " + skillLoadURL(epBase, s.Name, tok) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func skillURLName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), " ", "-")
}

func skillLoadURL(base, name string, tok func(string) string) string {
	u := base + "/skills/" + skillURLName(name)
	if tok != nil {
		if t := tok(name); t != "" {
			u += "?t=" + t
		}
	}
	return u
}

// serveSkillList writes the assigned-skills catalog (name + description + load
// URL) as an OpenAI-style list.
func serveSkillList(w http.ResponseWriter, skills []store.Skill, base string, tok func(string) string) {
	data := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		data = append(data, map[string]any{
			"name": s.Name, "description": s.Description,
			"load": skillLoadURL(base, s.Name, tok),
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// serveSkillOne writes one skill's full SKILL.md (+ a listing of the other files
// bundled in its folder), if it is among the given assigned skills.
// effective lists the skills a target actually carries: direct assignments
// plus every member of its assigned groups, resolved to readable copies.
func (p *Proxy) effective(kind string, id int64, model string) []store.Skill {
	var out []store.Skill
	for _, e := range p.Store.EffectiveSkillsFor(kind, id, model) {
		if !e.Missing {
			out = append(out, e.Skill)
		}
	}
	return out
}

// CatalogPreview renders the catalog block a target's requests receive, for
// the admin UI's assignment view.
func (p *Proxy) CatalogPreview(kind string, id int64, base string) string {
	kind = strings.ToLower(kind)
	scope := "e:"
	if kind == "provider" {
		scope = "p:"
	}
	name := ""
	if kind == "provider" {
		if prov, ok := p.Store.ProviderByID(id); ok {
			name = prov.Name
		}
	} else {
		eps, _ := p.Store.Endpoints()
		for _, e := range eps {
			if e.ID == id {
				name = e.Name
			}
		}
	}
	if name == "" {
		return ""
	}
	mount := "/e/"
	if kind == "provider" {
		mount = "/p/"
	}
	return skillCatalog(p.effective(kind, id, ""), base+mount+name, func(n string) string { return p.skillToken(scope+name, n) })
}

func (p *Proxy) serveSkillOne(w http.ResponseWriter, skills []store.Skill, want string) {
	p.serveSkillOneFor(w, skills, want, "", 0)
}

func (p *Proxy) serveSkillOneFor(w http.ResponseWriter, skills []store.Skill, want, kind string, targetID int64) {
	want = skillURLName(want)
	for _, s := range skills {
		if strings.EqualFold(skillURLName(s.Name), want) {
			if kind != "" {
				p.Store.RecordSkillLoad(s.Name, kind, targetID)
			}
			body, err := os.ReadFile(s.Path)
			if err != nil {
				// The assigned copy moved since the last rescan (skills get
				// reorganised; assignments point at index rows). Any other
				// indexed copy of the same name is the same skill — serve it
				// rather than fail the lazy-load until someone rescans and
				// re-assigns by hand.
				if alt, ok := p.readableSkillNamed(s.Name); ok {
					s, body, err = alt, nil, nil
					if body, err = os.ReadFile(alt.Path); err != nil {
						httpErr(w, "openai", 404, "skill file unavailable")
						return
					}
				} else {
					httpErr(w, "openai", 404, "skill file unavailable")
					return
				}
			}
			out := string(body)
			if extra := siblingFiles(s.Path); extra != "" {
				out += "\n\n---\nBundled files in this skill folder (fetch relative paths from the same base if needed):\n" + extra
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.WriteHeader(200)
			w.Write([]byte(out))
			return
		}
	}
	httpErr(w, "openai", 404, "no such skill here")
}

// readableSkillNamed finds another indexed copy of a skill whose file still
// exists, preferring copies outside an archive folder.
func (p *Proxy) readableSkillNamed(name string) (store.Skill, bool) {
	all, err := p.Store.Skills()
	if err != nil {
		return store.Skill{}, false
	}
	var archived *store.Skill
	for i := range all {
		s := all[i]
		if !strings.EqualFold(skillURLName(s.Name), skillURLName(name)) {
			continue
		}
		if _, err := os.Stat(s.Path); err != nil {
			continue
		}
		if strings.Contains(s.Path, "/_archive") {
			if archived == nil {
				archived = &all[i]
			}
			continue
		}
		return s, true
	}
	if archived != nil {
		return *archived, true
	}
	return store.Skill{}, false
}

// --- share-endpoint fetch (endpoint-key authed) ---

func (p *Proxy) handleEndpointSkills(w http.ResponseWriter, r *http.Request) {
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		return
	}
	scope := "e:" + ep.Name
	serveSkillList(w, p.effective("endpoint", ep.ID, ""), reqBase(r)+"/e/"+ep.Name, func(n string) string { return p.skillToken(scope, n) })
}

func (p *Proxy) handleEndpointSkill(w http.ResponseWriter, r *http.Request) {
	want := r.PathValue("name")
	// a load URL from the catalog authorizes itself — and names the skill,
	// even when the path was re-typed wrongly; otherwise the endpoint key
	if ep, found := p.Store.EndpointByName(r.PathValue("endpoint")); found && ep.Enabled {
		skills := p.effective("endpoint", ep.ID, "")
		if s, ok := p.skillByToken(r, "e:"+ep.Name, skills); ok {
			if !strings.EqualFold(skillURLName(s.Name), skillURLName(want)) {
				logSkillFetch(r, "e:"+ep.Name, want, s.Name, "served by token (name mismatch)")
			} else {
				logSkillFetch(r, "e:"+ep.Name, want, s.Name, "served by token")
			}
			p.serveSkillOneFor(w, skills, s.Name, "endpoint", ep.ID)
			return
		}
	}
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		logSkillFetch(r, "e:"+r.PathValue("endpoint"), want, "", "401: no valid token or endpoint key")
		return
	}
	skills := p.effective("endpoint", ep.ID, "")
	if s, ok := nearestSkill(skills, want); ok {
		logSkillFetch(r, "e:"+ep.Name, want, s.Name, "served by key")
		p.serveSkillOneFor(w, skills, s.Name, "endpoint", ep.ID)
		return
	}
	logSkillFetch(r, "e:"+ep.Name, want, "", "404: not assigned here")
	p.serveSkillOneFor(w, skills, want, "endpoint", ep.ID)
}

// --- provider-mount fetch (public-key authed) ---

func (p *Proxy) handleProviderSkills(w http.ResponseWriter, r *http.Request) {
	if !p.publicKeyOK(r) {
		httpErr(w, "openai", 401, "public access requires a valid API key")
		return
	}
	prov, ok := p.Store.ProviderByName(r.PathValue("provider"))
	if !ok {
		httpErr(w, "openai", 404, "unknown provider")
		return
	}
	scope := "p:" + prov.Name
	serveSkillList(w, p.effective("provider", prov.ID, ""), reqBase(r)+"/p/"+prov.Name, func(n string) string { return p.skillToken(scope, n) })
}

func (p *Proxy) handleProviderSkill(w http.ResponseWriter, r *http.Request) {
	prov, ok := p.Store.ProviderByName(r.PathValue("provider"))
	if !ok {
		httpErr(w, "openai", 404, "unknown provider")
		return
	}
	want := r.PathValue("name")
	skills := p.effective("provider", prov.ID, "")
	if s, ok := p.skillByToken(r, "p:"+prov.Name, skills); ok {
		logSkillFetch(r, "p:"+prov.Name, want, s.Name, "served by token")
		p.serveSkillOneFor(w, skills, s.Name, "provider", prov.ID)
		return
	}
	if !p.publicKeyOK(r) {
		logSkillFetch(r, "p:"+prov.Name, want, "", "401: no valid token or public key")
		httpErr(w, "openai", 401, "public access requires a valid API key")
		return
	}
	if s, ok := nearestSkill(skills, want); ok {
		p.serveSkillOneFor(w, skills, s.Name, "provider", prov.ID)
		return
	}
	p.serveSkillOneFor(w, skills, want, "provider", prov.ID)
}

// siblingFiles lists the non-SKILL.md files that live alongside a skill, so the
// model knows what resources (scripts, templates) the skill ships with.
func siblingFiles(skillPath string) string {
	dir := filepath.Dir(skillPath)
	var names []string
	filepath.WalkDir(dir, func(pth string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), "SKILL.md") {
			return nil
		}
		if rel, e := filepath.Rel(dir, pth); e == nil {
			names = append(names, rel)
		}
		return nil
	})
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return "- " + strings.Join(names, "\n- ")
}
