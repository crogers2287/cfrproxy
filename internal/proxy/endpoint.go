package proxy

// Share endpoints: /e/{name}/... — a scoped, key-authenticated view of the
// proxy that exposes only a curated set of models (or forces every request to
// one model / the auto router). Lets you hand someone a URL + key that reaches
// only what you allow.

import (
	"context"
	"net/http"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// authEndpoint resolves the {endpoint} path value, enforces its API key, and
// returns the endpoint. Writes the error response and returns ok=false on
// failure. Share endpoints ALWAYS require the key (LAN or not).
func (p *Proxy) authEndpoint(w http.ResponseWriter, r *http.Request, inbound string) (store.Endpoint, bool) {
	name := r.PathValue("endpoint")
	ep, found := p.Store.EndpointByName(name)
	if !found || !ep.Enabled {
		httpErr(w, inbound, 404, "no such endpoint")
		return ep, false
	}
	got := r.Header.Get("x-api-key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if ep.APIKey == "" || got != ep.APIKey {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cfrproxy"`)
		httpErr(w, inbound, 401, "invalid or missing API key for this endpoint")
		return ep, false
	}
	return ep, true
}

// endpointModels returns the model ids this endpoint exposes: the forced model
// (if any), else the configured allow-list, else the whole catalog.
func (p *Proxy) endpointModels(ctx context.Context, ep store.Endpoint) []string {
	if ep.ForceModel != "" {
		return []string{ep.ForceModel}
	}
	if pats := splitList(ep.Models); len(pats) > 0 {
		// expand globs against the live catalog; keep exact ids as-is
		all := p.AllModelIDs(ctx)
		var out []string
		seen := map[string]bool{}
		for _, pat := range pats {
			if strings.Contains(pat, "*") {
				for _, m := range all {
					if matchGlob(pat, m) && !seen[m] {
						seen[m] = true
						out = append(out, m)
					}
				}
			} else if !seen[pat] {
				seen[pat] = true
				out = append(out, pat)
			}
		}
		return out
	}
	return p.AllModelIDs(ctx)
}

// modelAllowed reports whether a requested model is permitted on this endpoint.
func (p *Proxy) modelAllowed(ep store.Endpoint, model string) bool {
	pats := splitList(ep.Models)
	if len(pats) == 0 {
		return true // no restriction
	}
	for _, pat := range pats {
		if strings.Contains(pat, "*") {
			if matchGlob(pat, model) {
				return true
			}
		} else if strings.EqualFold(pat, model) {
			return true
		}
	}
	return false
}

func matchGlob(pat, s string) bool {
	pat, s = strings.ToLower(pat), strings.ToLower(s)
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(s, parts[0]) || !strings.HasSuffix(s, parts[len(parts)-1]) {
		return false
	}
	rest := s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return true
}

func (p *Proxy) handleEndpointModels(w http.ResponseWriter, r *http.Request) {
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		return
	}
	ids := p.endpointModels(r.Context(), ep)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "cfrproxy"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// handleEndpoint authenticates against the endpoint key, applies its model
// policy (force or allow-list), then hands off to the normal data path.
func (p *Proxy) handleEndpoint(w http.ResponseWriter, r *http.Request, inbound string) {
	ep, ok := p.authEndpoint(w, r, inbound)
	if !ok {
		return
	}
	p.handleCore(w, r, inbound, "", &ep)
}
