package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// ListModels queries a provider's native model-listing endpoint and returns
// the model IDs it serves.
func (p *Proxy) ListModels(ctx context.Context, prov store.Provider) ([]string, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	var url string
	switch prov.Type {
	case "ollama":
		if strings.HasSuffix(base, "/api") {
			url = base + "/tags"
		} else {
			url = base + "/api/tags"
		}
	default: // openai + anthropic both serve GET .../v1/models
		if strings.HasSuffix(base, "/v1") {
			url = base + "/models"
		} else {
			url = base + "/v1/models"
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	switch prov.Type {
	case "anthropic":
		if prov.APIKey != "" {
			req.Header.Set("x-api-key", prov.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	if prov.Type == "ollama" {
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		var ids []string
		for _, m := range out.Models {
			ids = append(ids, m.Name)
		}
		return ids, nil
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// modelCache holds per-provider scan results so the data-plane listing
// endpoints don't hammer providers on every harness poll.
type modelCache struct {
	mu      sync.Mutex
	entries map[int64]modelCacheEntry
}

type modelCacheEntry struct {
	models []string
	at     time.Time
}

const modelCacheTTL = 60 * time.Second

// ModelsCached returns a provider's model list from cache, scanning when
// stale. Errors degrade to an empty list (callers fall back to the
// configured default model).
func (p *Proxy) ModelsCached(ctx context.Context, prov store.Provider) []string {
	p.models.mu.Lock()
	e, ok := p.models.entries[prov.ID]
	p.models.mu.Unlock()
	if ok && time.Since(e.at) < modelCacheTTL {
		return e.models
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ids, err := p.ListModels(cctx, prov)
	if err != nil {
		ids = nil
	}
	ids = applyModelsFilter(ids, prov.ModelsFilter)
	p.models.mu.Lock()
	p.models.entries[prov.ID] = modelCacheEntry{models: ids, at: time.Now()}
	p.models.mu.Unlock()
	return ids
}

// applyModelsFilter narrows a scan to the provider's category: comma-separated
// globs ("gpt-*"); "!" prefix excludes ("claude-*,!claude-command-*").
func applyModelsFilter(ids []string, filter string) []string {
	pats := splitList(filter)
	if len(pats) == 0 {
		return ids
	}
	var inc, exc []string
	for _, p := range pats {
		if strings.HasPrefix(p, "!") {
			exc = append(exc, strings.TrimPrefix(p, "!"))
		} else {
			inc = append(inc, p)
		}
	}
	match := func(pat, id string) bool {
		pat, id = strings.ToLower(pat), strings.ToLower(id)
		if !strings.Contains(pat, "*") {
			return pat == id
		}
		parts := strings.Split(pat, "*")
		if !strings.HasPrefix(id, parts[0]) || !strings.HasSuffix(id, parts[len(parts)-1]) {
			return false
		}
		rest := id[len(parts[0]):]
		for _, mid := range parts[1 : len(parts)-1] {
			i := strings.Index(rest, mid)
			if i < 0 {
				return false
			}
			rest = rest[i+len(mid):]
		}
		return true
	}
	var out []string
	for _, id := range ids {
		ok := len(inc) == 0
		for _, p := range inc {
			if match(p, id) {
				ok = true
				break
			}
		}
		if ok {
			for _, p := range exc {
				if match(p, id) {
					ok = false
					break
				}
			}
		}
		if ok {
			out = append(out, id)
		}
	}
	return out
}

// FuzzyModel matches a wanted model against a list: exact > case-insensitive
// > unique substring > unique punctuation-blind substring (so "Qwen3.8"
// finds "qwen-3.8-max-preview-thinking").
func FuzzyModel(models []string, want string) (string, bool) {
	for _, m := range models {
		if m == want {
			return m, true
		}
	}
	for _, m := range models {
		if strings.EqualFold(m, want) {
			return m, true
		}
	}
	var subs []string
	for _, m := range models {
		if strings.Contains(strings.ToLower(m), strings.ToLower(want)) {
			subs = append(subs, m)
		}
	}
	if len(subs) == 1 {
		return subs[0], true
	}
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	subs = nil
	for _, m := range models {
		if strings.Contains(norm(m), norm(want)) {
			subs = append(subs, m)
		}
	}
	if len(subs) == 1 {
		return subs[0], true
	}
	return "", false
}

// MatchMapPattern reports whether a model matches a map pattern: exact
// (case-insensitive) or trailing-* prefix ("claude-sonnet*").
func MatchMapPattern(pattern, model string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.TrimSuffix(pattern, "*")))
	}
	return strings.EqualFold(pattern, model)
}

// ResolveModel routes an inbound model string to (provider, real model id):
//  1. model map rewrite (harness preset names → any provider/model)
//  2. case-insensitive "provider/model" prefix, model fuzzy-matched against
//     the provider's live scan
//  3. bare alias from a provider's alias list
//  4. unique fuzzy match across all enabled providers' scans
//  5. fallback: highest-priority enabled provider and ITS default model —
//     unknown harness names route somewhere useful instead of erroring
func (p *Proxy) ResolveModel(ctx context.Context, model string) (store.Provider, string, error) {
	if mapped := p.Store.ModelMapLookup(model, MatchMapPattern); mapped != "" {
		model = mapped
	}
	provs := p.Store.Providers()
	if i := strings.IndexByte(model, '/'); i > 0 {
		name, rest := model[:i], model[i+1:]
		for _, prov := range provs {
			if !strings.EqualFold(prov.Name, name) {
				continue
			}
			// explicitly-addressed providers must not silently fall through —
			// a disabled provider is a loud error, not a reroute
			if !prov.Enabled {
				return store.Provider{}, "", fmt.Errorf("provider %q is disabled — enable it in the WebUI or with: cfrproxy provider edit --name %s", prov.Name, prov.Name)
			}
			if rest == "" {
				return prov, prov.DefaultModel, nil
			}
			if m, ok := FuzzyModel(p.ModelsCached(ctx, prov), rest); ok {
				return prov, m, nil
			}
			return prov, rest, nil // pass through as typed
		}
	}
	for _, prov := range provs {
		if !prov.Enabled {
			continue
		}
		for _, alias := range strings.Split(prov.Models, ",") {
			if a := strings.TrimSpace(alias); a != "" && strings.EqualFold(a, model) {
				return prov, model, nil
			}
		}
	}
	if model != "" && model != "default" {
		for _, prov := range provs {
			if !prov.Enabled {
				continue
			}
			if m, ok := FuzzyModel(p.ModelsCached(ctx, prov), model); ok {
				return prov, m, nil
			}
		}
	}
	for _, prov := range provs {
		if prov.Enabled {
			return prov, prov.DefaultModel, nil
		}
	}
	return store.Provider{}, "", fmt.Errorf("no enabled providers configured")
}

// AllModelIDs merges every enabled provider's scanned models (as
// provider/model), plus configured aliases and defaults. Scans run in
// parallel on cold cache.
func (p *Proxy) AllModelIDs(ctx context.Context) []string {
	provs := p.Store.Providers()
	type result struct {
		idx    int
		models []string
	}
	var wg sync.WaitGroup
	results := make([]result, 0, len(provs))
	var mu sync.Mutex
	for i, prov := range provs {
		if !prov.Enabled {
			continue
		}
		wg.Add(1)
		go func(i int, prov store.Provider) {
			defer wg.Done()
			ms := p.ModelsCached(ctx, prov)
			mu.Lock()
			results = append(results, result{i, ms})
			mu.Unlock()
		}(i, prov)
	}
	wg.Wait()
	byIdx := map[int][]string{}
	for _, r := range results {
		byIdx[r.idx] = r.models
	}
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if c := p.AutoRouterConfig(); c.Enabled && len(c.Routes) > 0 {
		add("auto") // virtual task-routing model, listed first
		if c.Planner != "" {
			add("auto-plan") // plan stage + routed execution
		}
	}
	for i, prov := range provs {
		if !prov.Enabled {
			continue
		}
		// pinned list caps what pickers see for this provider
		if pins := splitList(prov.PinnedModels); len(pins) > 0 {
			for _, m := range pins {
				add(prov.Name + "/" + m)
			}
			for _, alias := range strings.Split(prov.Models, ",") {
				add(strings.TrimSpace(alias))
			}
			continue
		}
		for _, m := range byIdx[i] {
			add(prov.Name + "/" + m)
		}
		if len(byIdx[i]) == 0 && prov.DefaultModel != "" {
			add(prov.Name + "/" + prov.DefaultModel)
		}
		for _, alias := range strings.Split(prov.Models, ",") {
			add(strings.TrimSpace(alias))
		}
	}
	if len(ids) == 0 {
		ids = []string{"default"}
	}
	return ids
}
