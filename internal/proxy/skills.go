package proxy

// Skill lazy-loading on share endpoints. When an endpoint has skills assigned,
// handleCore injects a compact catalog (name + description + a fetch URL) into the
// system prompt; the model pulls a skill's full SKILL.md from the fetch endpoint
// only when a task needs it (progressive disclosure), instead of every skill
// living in every context.

import (
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
func skillCatalog(skills []store.Skill, epBase string) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("You have access to the skills below. Each is a packaged set of instructions for a specific kind of task. Only the name and a short description are shown here to save context. When the current task matches a skill, load its full instructions FIRST by fetching the URL shown (an HTTP GET that returns the skill's SKILL.md), then follow them. Do not guess a skill's contents.\n\n")
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
		b.WriteString("  load: GET " + epBase + "/skills/" + skillURLName(s.Name) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func skillURLName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), " ", "-")
}

// serveSkillList writes the assigned-skills catalog (name + description + load
// URL) as an OpenAI-style list.
func serveSkillList(w http.ResponseWriter, skills []store.Skill, base string) {
	data := make([]map[string]any, 0, len(skills))
	for _, s := range skills {
		data = append(data, map[string]any{
			"name": s.Name, "description": s.Description,
			"load": base + "/skills/" + skillURLName(s.Name),
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// serveSkillOne writes one skill's full SKILL.md (+ a listing of the other files
// bundled in its folder), if it is among the given assigned skills.
func serveSkillOne(w http.ResponseWriter, skills []store.Skill, want string) {
	want = skillURLName(want)
	for _, s := range skills {
		if strings.EqualFold(skillURLName(s.Name), want) {
			body, err := os.ReadFile(s.Path)
			if err != nil {
				httpErr(w, "openai", 404, "skill file unavailable")
				return
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

// --- share-endpoint fetch (endpoint-key authed) ---

func (p *Proxy) handleEndpointSkills(w http.ResponseWriter, r *http.Request) {
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		return
	}
	serveSkillList(w, p.Store.SkillsForEndpoint(ep.ID, ""), reqBase(r)+"/e/"+ep.Name)
}

func (p *Proxy) handleEndpointSkill(w http.ResponseWriter, r *http.Request) {
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		return
	}
	serveSkillOne(w, p.Store.SkillsForEndpoint(ep.ID, ""), r.PathValue("name"))
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
	serveSkillList(w, p.Store.SkillsForProvider(prov.ID, ""), reqBase(r)+"/p/"+prov.Name)
}

func (p *Proxy) handleProviderSkill(w http.ResponseWriter, r *http.Request) {
	if !p.publicKeyOK(r) {
		httpErr(w, "openai", 401, "public access requires a valid API key")
		return
	}
	prov, ok := p.Store.ProviderByName(r.PathValue("provider"))
	if !ok {
		httpErr(w, "openai", 404, "unknown provider")
		return
	}
	serveSkillOne(w, p.Store.SkillsForProvider(prov.ID, ""), r.PathValue("name"))
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
