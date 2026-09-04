package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// endsWithVersion reports whether a base URL's last path segment is a version
// like /v1, /v4, /v1beta — meaning the base already includes the API version
// and cfrproxy should NOT add its own /v1.
func endsWithVersion(base string) bool {
	base = strings.TrimRight(base, "/")
	seg := base[strings.LastIndex(base, "/")+1:]
	return len(seg) >= 2 && seg[0] == 'v' && seg[1] >= '0' && seg[1] <= '9'
}

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
	case "commandcode":
		// The catalog is served under the Pro-only /provider/v1 path (it is
		// open to any plan), while generation lives at /alpha/generate.
		base = strings.TrimSuffix(base, "/provider/v1")
		url = base + "/provider/v1/models"
	default: // openai + anthropic both serve GET .../v1/models
		if endsWithVersion(base) {
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
	p.injectProviderHeaders(req, prov)
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
			ID   string `json:"id"`
			Meta struct {
				// llama-swap publishes real per-model capability alongside the
				// id. When a provider tells us whether a model reads images,
				// that beats guessing from its name — see recordVisionMeta.
				LlamaSwap struct {
					IsVision any `json:"isVision"`
					Context  any `json:"context"`
				} `json:"llamaswap"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	for _, m := range out.Data {
		if v, ok := truthy(m.Meta.LlamaSwap.IsVision); ok {
			p.recordVisionMeta(prov.Name, m.ID, v)
		}
		if n, ok := asInt(m.Meta.LlamaSwap.Context); ok && n > 0 {
			p.recordContextMeta(prov.Name, m.ID, n)
		}
	}
	// dedupe: llama-swap lists an alias and its target under the same id, so a
	// raw pass makes pickers show the same model twice
	var ids []string
	seen := map[string]bool{}
	for _, m := range out.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
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

// ListModelsFiltered scans a provider and narrows the catalog to what that
// provider will actually serve, applying models_filter the same way the data
// plane's ModelsCached does. Returns the raw scan size alongside so callers can
// report "12 of 139" instead of silently hiding models.
//
// Prefer this over ListModels for anything user-facing: a shared upstream
// (CLIProxyAPI, OpenRouter) answers /v1/models with its whole catalog, and
// routing then rejects everything the provider's filter excludes — so an
// unfiltered list offers models that cannot actually be reached.
func (p *Proxy) ListModelsFiltered(ctx context.Context, prov store.Provider) (ids []string, scanned int, err error) {
	raw, err := p.ListModels(ctx, prov)
	if err != nil {
		return nil, 0, err
	}
	return applyModelsFilter(raw, prov.ModelsFilter), len(raw), nil
}

// ApplyModelsFilter is applyModelsFilter for callers outside the package —
// the OAuth scan uses it to preview which models a candidate provider's filter
// would actually match before creating it.
func ApplyModelsFilter(ids []string, filter string) []string {
	return applyModelsFilter(ids, filter)
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

// providerAllowsModel reports whether a model passes a provider's models_filter.
// A provider with a filter is an allow-list: a model that fails it is NOT
// served by that provider (important when several providers share one backend).
func providerAllowsModel(prov store.Provider, model string) bool {
	if strings.TrimSpace(prov.ModelsFilter) == "" {
		return true
	}
	return len(applyModelsFilter([]string{model}, prov.ModelsFilter)) == 1
}

// ResolveModel routes an inbound model string to (provider, real model id):
//  1. model map rewrite (harness preset names → any provider/model)
//  2. case-insensitive "provider/model" prefix, model fuzzy-matched against
//     the provider's live scan
//  3. bare alias from a provider's alias list
//  4. unique fuzzy match across all enabled providers' scans
//  5. fallback: highest-priority enabled provider and ITS default model —
//     unknown harness names route somewhere useful instead of erroring
//
// stripProviderPrefix removes a leading "provider/" from a model id, but ONLY
// when that prefix actually names a provider.
//
// A scoped mount (/p/{provider}) needs to correct a caller who addressed a
// different provider — "grok/grok-4.5" sent to /p/fred should become
// fred's. But plenty of model ids are themselves vendor-qualified
// ("thinkingmachines/inkling", "deepseek/deepseek-v4-pro"), and blindly
// stripping to the last segment mangles them: bare "inkling" is ambiguous
// against "inkling-small", so the fuzzy match declines and the request 503s
// with `model "inkling" is not served`. Ids whose stripped form happened to be
// unique kept working, which is why this only shows up on some models.
func (p *Proxy) stripProviderPrefix(model string) string {
	i := strings.IndexByte(model, '/')
	if i <= 0 {
		return model
	}
	if _, ok := p.Store.ProviderByName(model[:i]); ok {
		return model[i+1:]
	}
	return model
}

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
			// A provider with a models_filter only serves matching models — do
			// not leak an arbitrary model to a shared backend (e.g. CLIProxyAPI
			// serving grok + claude behind separate filtered providers).
			if !providerAllowsModel(prov, rest) {
				return store.Provider{}, "", fmt.Errorf("model %q is not served by provider %q", rest, prov.Name)
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

// mapAliasIDs returns the model_map keys worth advertising, sorted so the
// listing is stable.
//
// Glob patterns ("claude-sonnet*") are deliberately excluded: those are
// interception rules for ids a client already knows — they exist to catch a
// name the harness will send anyway — and a pattern is not a selectable id.
// An exact key is the opposite: a name that exists only because the operator
// invented it, and that nothing will ever send unless it is advertised.
//
// An alias whose target names a provider that is missing or disabled is left
// out, since ResolveModel answers that with a hard error: offering it in a
// picker would hand the caller an id that can only fail.
func (p *Proxy) mapAliasIDs() []string {
	m := p.Store.ModelMap()
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for alias, target := range m {
		if alias == "" || strings.Contains(alias, "*") {
			continue
		}
		// A target with no provider prefix is another virtual name ("auto") and
		// is left to resolve on its own.
		if i := strings.IndexByte(target, '/'); i > 0 {
			prov, ok := p.Store.ProviderByName(target[:i])
			if !ok || !prov.Enabled {
				continue
			}
		}
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
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
	if c := p.AutoRouterConfig(); c.Enabled && (len(c.Routes) > 0 || c.Smart.on()) {
		add("auto") // virtual task-routing model, listed first
		if c.Planner != "" {
			add("auto-plan") // plan stage + routed execution
		}
	}
	if f := p.FusionConfig(); f.Enabled && len(f.Participants) > 0 && f.Judge != "" {
		add("fusion") // parallel drafts → judge synthesis
	}
	if routers, err := p.Store.Routers(); err == nil {
		for _, rt := range routers {
			if rt.Enabled {
				add("auto:" + rt.Name)
				if rt.Planner != "" {
					add("auto-plan:" + rt.Name)
				}
			}
		}
	}
	// Named fusions list exactly like named routers, so a picker shows
	// "fusion:deep" beside "auto:code" and either can be selected, put in a
	// router bucket, or named as a provider's default model.
	if fusions, err := p.Store.Fusions(); err == nil {
		for _, f := range fusions {
			if f.Enabled && f.Judge != "" && len(f.Participants) > 0 {
				add("fusion:" + f.Name)
			}
		}
	}
	// Exact model_map aliases are selectable names in their own right, the same
	// class of thing as "auto" and "fusion:deep": the operator declared them,
	// ResolveModel rewrites them before anything else, and a picker offering one
	// gets exactly what the map promises. They were invisible here, so an alias
	// only worked if you already knew its name and typed it — which is no use to
	// a client that enumerates.
	for _, alias := range p.mapAliasIDs() {
		add(alias)
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

// isAutoMount reports whether a /p/{name} mount addresses the virtual router
// provider. Like the fusion mount (fusion.go), it exists because a harness
// picker such as Hermes/Telegram can only show what some provider lists, and
// "auto" belongs to no configured provider — so it never appeared there.
func isAutoMount(scope string) bool {
	return strings.EqualFold(scope, "auto") || strings.EqualFold(scope, "cfrproxy-auto")
}

// autoModelIDs lists the router ids a scoped /p/cfrproxy-auto mount serves:
// the default router, its plan variant, and every enabled named router.
func (p *Proxy) autoModelIDs() []string {
	var ids []string
	if c := p.AutoRouterConfig(); c.Enabled && (len(c.Routes) > 0 || c.Smart.on()) {
		ids = append(ids, "auto")
		if c.Planner != "" {
			ids = append(ids, "auto-plan")
		}
	}
	if routers, err := p.Store.Routers(); err == nil {
		for _, rt := range routers {
			if rt.Enabled {
				ids = append(ids, "auto:"+rt.Name)
				if rt.Planner != "" {
					ids = append(ids, "auto-plan:"+rt.Name)
				}
			}
		}
	}
	return ids
}
