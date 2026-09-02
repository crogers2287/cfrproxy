// Package api is the management plane: REST API + live SSE trace stream +
// embedded WebUI, all behind HTTP basic auth.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/transform"
)

//go:embed webui
var webuiFS embed.FS

type API struct {
	Store *store.Store
	Proxy *proxy.Proxy
}

// EnsureCredentials makes sure basic-auth creds exist; returns a freshly
// generated password (to print once) when it had to create them.
func (a *API) EnsureCredentials() (user, freshPass string, err error) {
	user = a.Store.Setting("admin_user")
	if user == "" {
		user = "admin"
		if err := a.Store.SetSetting("admin_user", user); err != nil {
			return "", "", err
		}
	}
	if a.Store.Setting("admin_pass_hash") != "" {
		return user, "", nil
	}
	raw := make([]byte, 12)
	rand.Read(raw)
	pass := base64.RawURLEncoding.EncodeToString(raw)
	if err := a.SetPassword(pass); err != nil {
		return "", "", err
	}
	return user, pass, nil
}

func (a *API) SetPassword(pass string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return a.Store.SetSetting("admin_pass_hash", string(h))
}

func (a *API) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		wantUser := a.Store.Setting("admin_user")
		hash := a.Store.Setting("admin_pass_hash")
		if !ok || hash == "" ||
			subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) != 1 ||
			bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="cfrproxy"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) Register(mux *http.ServeMux) {
	sub, _ := fs.Sub(webuiFS, "webui")
	inner := http.NewServeMux()
	// embed.FS reports a zero ModTime for every file, so http.FileServer never
	// emits Last-Modified/ETag and browsers are left with no validator at all.
	// Without an explicit no-store, that reads as "cache this indefinitely" —
	// after every rebuild+restart, phones kept serving whatever admin HTML/JS
	// they'd last fetched, silently reintroducing fixed bugs. The WebUI is a
	// low-traffic single-page admin panel; always-fresh beats any caching win.
	noStoreFileServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		http.FileServer(http.FS(sub)).ServeHTTP(w, r)
	})
	inner.Handle("GET /admin/", http.StripPrefix("/admin/", noStoreFileServer))

	inner.HandleFunc("GET /admin/api/status", a.hStatus)
	inner.HandleFunc("GET /admin/api/providers", a.hProvidersList)
	inner.HandleFunc("POST /admin/api/providers", a.hProviderCreate)
	inner.HandleFunc("PUT /admin/api/providers/{id}", a.hProviderUpdate)
	inner.HandleFunc("DELETE /admin/api/providers/{id}", a.hProviderDelete)
	inner.HandleFunc("POST /admin/api/providers/reorder", a.hReorder)
	inner.HandleFunc("POST /admin/api/providers/scan-models", a.hProviderScanModels)
	inner.HandleFunc("POST /admin/api/providers/{id}/test", a.hTest)
	inner.HandleFunc("GET /admin/api/providers/{id}/models", a.hProviderModels)
	// Admin-plane mirrors of the model list. The WebUI used to fetch /v1/models
	// and /p/<prov>/v1/models directly, but those are data-plane routes: once
	// public_api_keys is configured they require a key, and a browser will NOT
	// attach the /admin/ basic-auth credentials to them (different protection
	// space), so the model pickers silently emptied for anyone reaching the UI
	// through the reverse proxy. These live under /admin/ where the browser
	// already authenticates.
	inner.HandleFunc("GET /admin/api/allmodels", a.hAllModels)
	inner.HandleFunc("GET /admin/api/scopedmodels/{provider}", a.hScopedModels)
	inner.HandleFunc("POST /admin/api/providers/{id}/docs/fetch", a.hDocsFetch)
	inner.HandleFunc("GET /admin/api/providers/{id}/docs", a.hDocsGet)
	inner.HandleFunc("GET /admin/api/transforms", a.hTransformsList)
	inner.HandleFunc("POST /admin/api/transforms", a.hTransformCreate)
	inner.HandleFunc("PUT /admin/api/transforms/{id}", a.hTransformUpdate)
	inner.HandleFunc("DELETE /admin/api/transforms/{id}", a.hTransformDelete)
	inner.HandleFunc("POST /admin/api/transforms/{id}/toggle", a.hTransformToggle)
	a.registerOAuth(inner)
	inner.HandleFunc("GET /admin/api/endpoints", a.hEndpointsList)
	inner.HandleFunc("POST /admin/api/endpoints", a.hEndpointSave)
	inner.HandleFunc("PUT /admin/api/endpoints/{id}", a.hEndpointSave)
	inner.HandleFunc("DELETE /admin/api/endpoints/{id}", a.hEndpointDelete)
	inner.HandleFunc("GET /admin/api/agents", a.hAgentsList)
	inner.HandleFunc("POST /admin/api/agents", a.hAgentSave)
	inner.HandleFunc("PUT /admin/api/agents/{id}", a.hAgentSave)
	inner.HandleFunc("DELETE /admin/api/agents/{id}", a.hAgentDelete)
	inner.HandleFunc("GET /admin/api/roundtable", a.hRTGet)
	inner.HandleFunc("GET /admin/api/roundtable-logs", a.hRTLogs)
	inner.HandleFunc("GET /admin/api/roundtable-logs/{id}", a.hRTLogDetail)
	inner.HandleFunc("PUT /admin/api/roundtable", a.hRTSet)
	inner.HandleFunc("GET /admin/api/compression", a.hCompGet)
	inner.HandleFunc("PUT /admin/api/compression", a.hCompSet)
	inner.HandleFunc("GET /admin/api/routers", a.hRoutersList)
	inner.HandleFunc("POST /admin/api/routers", a.hRouterSave)
	inner.HandleFunc("PUT /admin/api/routers/{id}", a.hRouterSave)
	inner.HandleFunc("DELETE /admin/api/routers/{id}", a.hRouterDelete)
	inner.HandleFunc("GET /admin/api/fusions", a.hFusionsList)
	inner.HandleFunc("POST /admin/api/fusions", a.hFusionSave)
	inner.HandleFunc("PUT /admin/api/fusions/{id}", a.hFusionSave)
	inner.HandleFunc("DELETE /admin/api/fusions/{id}", a.hFusionDelete)
	inner.HandleFunc("GET /admin/api/fusion", a.hFusionGet)
	inner.HandleFunc("PUT /admin/api/fusion", a.hFusionSet)
	// Skill index (see internal/api/skills.go, internal/store/skills.go)
	inner.HandleFunc("GET /admin/api/skills", a.hSkillsList)
	inner.HandleFunc("POST /admin/api/skills/rescan", a.hSkillRescan)
	inner.HandleFunc("GET /admin/api/skills/{id}", a.hSkillGet)
	inner.HandleFunc("PUT /admin/api/skills/{id}", a.hSkillSave)
	inner.HandleFunc("POST /admin/api/skills/{id}/symlink", func(w http.ResponseWriter, r *http.Request) { a.hSkillDistribute(w, r, true) })
	inner.HandleFunc("POST /admin/api/skills/{id}/copy", func(w http.ResponseWriter, r *http.Request) { a.hSkillDistribute(w, r, false) })
	inner.HandleFunc("GET /admin/api/skill-roots", a.hSkillRootsList)
	inner.HandleFunc("POST /admin/api/skill-roots", a.hSkillRootSave)
	inner.HandleFunc("DELETE /admin/api/skill-roots/{id}", a.hSkillRootDelete)
	inner.HandleFunc("GET /admin/api/skill-assign", a.hSkillAssignGet)
	inner.HandleFunc("POST /admin/api/skill-assign", a.hSkillAssignSet)
	inner.HandleFunc("GET /admin/api/autoroute", a.hAutoRouteGet)
	inner.HandleFunc("PUT /admin/api/autoroute", a.hAutoRouteSet)
	inner.HandleFunc("GET /admin/api/global-fallback", a.hGlobalFBGet)
	inner.HandleFunc("PUT /admin/api/global-fallback", a.hGlobalFBSet)
	inner.HandleFunc("GET /admin/api/provider-fallback", func(w http.ResponseWriter, r *http.Request) {
		settingJSON(a, w, "provider_fallback", `{"enabled":false}`)
	})
	inner.HandleFunc("PUT /admin/api/provider-fallback", func(w http.ResponseWriter, r *http.Request) {
		settingJSONSet(a, w, r, "provider_fallback")
	})
	inner.HandleFunc("GET /admin/api/modelmap", a.hModelMapGet)
	inner.HandleFunc("PUT /admin/api/modelmap", a.hModelMapPut)
	inner.HandleFunc("GET /admin/api/stats", a.hStats)
	inner.HandleFunc("GET /admin/api/traces", a.hTraces)
	inner.HandleFunc("GET /admin/api/usage", a.hUsage)
	inner.HandleFunc("GET /admin/api/logs/stream", a.hLogStream)

	mux.Handle("/admin/", a.basicAuth(inner))
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
}

func (a *API) hStatus(w http.ResponseWriter, r *http.Request) {
	provs := a.Store.Providers()
	enabled := 0
	for _, p := range provs {
		if p.Enabled {
			enabled++
		}
	}
	writeJSON(w, 200, map[string]any{"providers": len(provs), "enabled": enabled, "time": time.Now().Unix()})
}

// providers are returned without decrypted keys or full doc bodies
func publicProvider(p store.Provider) store.Provider {
	p.APIKey = ""
	if len(p.DocMarkdown) > 0 {
		p.DocMarkdown = "" // fetched separately via /docs
	}
	return p
}

func (a *API) hProvidersList(w http.ResponseWriter, r *http.Request) {
	provs := a.Store.Providers()
	out := make([]store.Provider, 0, len(provs))
	for _, p := range provs {
		out = append(out, publicProvider(p))
	}
	writeJSON(w, 200, out)
}

func (a *API) hProviderCreate(w http.ResponseWriter, r *http.Request) {
	var p store.Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	p.ID = 0
	base, note := a.Proxy.DiscoverBase(r.Context(), p)
	p.BaseURL = base
	if err := a.Store.SaveProvider(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"provider": publicProvider(p), "note": note})
}

func (a *API) hProviderUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	existing, ok := a.Store.ProviderByID(id)
	if !ok {
		httpErr(w, 404, "provider not found")
		return
	}
	p := existing
	p.APIKey = "" // only set if the payload includes a new key
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	p.ID = id
	note := ""
	if p.BaseURL != existing.BaseURL || p.Type != existing.Type {
		probe := p
		if probe.APIKey == "" {
			probe.APIKey = existing.APIKey // probe with the kept key
		}
		p.BaseURL, note = a.Proxy.DiscoverBase(r.Context(), probe)
	}
	if err := a.Store.SaveProvider(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"provider": publicProvider(p), "note": note})
}

func (a *API) hProviderDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteProvider(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hReorder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.Reorder(body.IDs); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hTest(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	prov, ok := a.Store.ProviderByID(id)
	if !ok {
		httpErr(w, 404, "provider not found")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Prompt == "" {
		body.Prompt = "Reply with the single word: pong"
	}
	start := time.Now()
	resp, err := a.Proxy.TestProvider(r.Context(), prov, body.Prompt)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "latency_ms": time.Since(start).Milliseconds()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "content": resp.Content, "model": resp.Model,
		"latency_ms": time.Since(start).Milliseconds(), "tokens": resp.CompletionTokens})
}

// hAllModels mirrors GET /v1/models for the admin UI.
func (a *API) hAllModels(w http.ResponseWriter, r *http.Request) {
	ids := a.Proxy.AllModelIDs(r.Context())
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m := map[string]any{"id": id, "object": "model"}
		// Carry the derived window so the UI can show what a share would be
		// capping, instead of asking the operator to type a number blind.
		// Omitted when unknown, matching the public listing.
		if n := a.Proxy.AdvertisedContext("", id); n > 0 {
			m["context_length"] = n
		}
		data = append(data, m)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// hScopedModels mirrors GET /p/{provider}/v1/models?all=1 for the admin UI.
func (a *API) hScopedModels(w http.ResponseWriter, r *http.Request) {
	prov := r.PathValue("provider")
	if _, ok := a.Store.ProviderByName(prov); !ok {
		httpErr(w, 404, "unknown provider: "+prov)
		return
	}
	ids := a.Proxy.ScopedModelIDs(r.Context(), prov, r.URL.Query().Get("all") != "")
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (a *API) hProviderModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	prov, ok := a.Store.ProviderByID(id)
	if !ok {
		httpErr(w, 404, "provider not found")
		return
	}
	models, scanned, err := a.Proxy.ListModelsFiltered(r.Context(), prov)
	if err != nil {
		writeJSON(w, 200, map[string]any{"models": []string{}, "error": err.Error()})
		return
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, 200, map[string]any{"models": models, "count": len(models),
		"scanned": scanned, "filter": prov.ModelsFilter})
}

// hProviderScanModels scans a provider's catalog from whatever the admin form
// currently holds, so "Scan models" works while ADDING a provider — before it
// has an id. An existing id fills in anything the form left blank (notably the
// stored API key, which the form never round-trips). When the pasted base URL
// doesn't answer, it retries through the same discovery the save path uses so a
// domain-root paste ("https://host.com") scans instead of erroring.
func (a *API) hProviderScanModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      int64  `json:"id"`
		Type    string `json:"type"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		// pointer so an omitted field keeps the stored filter while an
		// explicitly-empty one clears it — the form must be able to do both
		ModelsFilter *string `json:"models_filter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	var prov store.Provider
	if body.ID > 0 {
		if existing, ok := a.Store.ProviderByID(body.ID); ok {
			prov = existing
		}
	}
	if body.Type != "" {
		prov.Type = body.Type
	}
	if body.BaseURL != "" {
		prov.BaseURL = body.BaseURL
	}
	if body.APIKey != "" {
		prov.APIKey = body.APIKey
	}
	if body.ModelsFilter != nil {
		prov.ModelsFilter = *body.ModelsFilter
	}
	if prov.Type == "" {
		prov.Type = "openai"
	}
	if strings.TrimSpace(prov.BaseURL) == "" {
		writeJSON(w, 200, map[string]any{"models": []string{}, "error": "enter a base URL first"})
		return
	}
	models, scanned, err := a.Proxy.ListModelsFiltered(r.Context(), prov)
	if err != nil {
		probe := prov
		if discovered, _ := a.Proxy.DiscoverBase(r.Context(), probe); discovered != "" && discovered != prov.BaseURL {
			probe.BaseURL = discovered
			if m2, n2, err2 := a.Proxy.ListModelsFiltered(r.Context(), probe); err2 == nil {
				writeJSON(w, 200, map[string]any{"models": m2, "count": len(m2), "scanned": n2,
					"filter": prov.ModelsFilter, "base_url": discovered})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"models": []string{}, "error": err.Error()})
		return
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, 200, map[string]any{"models": models, "count": len(models),
		"scanned": scanned, "filter": prov.ModelsFilter})
}

func (a *API) hDocsGet(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	prov, ok := a.Store.ProviderByID(id)
	if !ok {
		httpErr(w, 404, "provider not found")
		return
	}
	writeJSON(w, 200, map[string]any{"doc_url": prov.DocURL, "doc_markdown": prov.DocMarkdown, "inject_docs": prov.InjectDocs})
}

// hDocsFetch pulls doc_url server-side and stores the body as the provider's markdown.
func (a *API) hDocsFetch(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	prov, ok := a.Store.ProviderByID(id)
	if !ok {
		httpErr(w, 404, "provider not found")
		return
	}
	if prov.DocURL == "" {
		httpErr(w, 400, "provider has no doc_url")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", prov.DocURL, nil)
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		httpErr(w, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		httpErr(w, 502, fmt.Sprintf("fetch %s: HTTP %d", prov.DocURL, resp.StatusCode))
		return
	}
	prov.DocMarkdown = string(b)
	if err := a.Store.SaveProvider(&prov); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "bytes": len(b)})
}

func (a *API) hTransformsList(w http.ResponseWriter, r *http.Request) {
	ts, err := a.Store.Transforms()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if ts == nil {
		ts = []store.Transform{}
	}
	writeJSON(w, 200, ts)
}

func (a *API) hTransformCreate(w http.ResponseWriter, r *http.Request) {
	var t store.Transform
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if _, err := transform.Parse(t.Rules); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	t.ID = 0
	if err := a.Store.SaveTransform(&t); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

func (a *API) hTransformUpdate(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var t store.Transform
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if _, err := transform.Parse(t.Rules); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	t.ID = id
	if err := a.Store.SaveTransform(&t); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

func (a *API) hTransformDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteTransform(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hTransformToggle(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.SetTransformEnabled(id, body.Enabled); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hEndpointsList(w http.ResponseWriter, r *http.Request) {
	eps, err := a.Store.Endpoints()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if eps == nil {
		eps = []store.Endpoint{}
	}
	writeJSON(w, 200, eps)
}

func (a *API) hEndpointSave(w http.ResponseWriter, r *http.Request) {
	var e store.Endpoint
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if ids := r.PathValue("id"); ids != "" {
		e.ID, _ = strconv.ParseInt(ids, 10, 64)
	} else {
		e.ID = 0
		if e.APIKey == "" {
			raw := make([]byte, 18)
			rand.Read(raw)
			e.APIKey = "cfr_" + base64.RawURLEncoding.EncodeToString(raw)
		}
	}
	if err := a.Store.SaveEndpoint(&e); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, e)
}

func (a *API) hEndpointDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteEndpoint(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hAgentsList(w http.ResponseWriter, r *http.Request) {
	ps, err := a.Store.AgentProfiles()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if ps == nil {
		ps = []store.AgentProfile{}
	}
	writeJSON(w, 200, ps)
}

func (a *API) hAgentSave(w http.ResponseWriter, r *http.Request) {
	var p store.AgentProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if ids := r.PathValue("id"); ids != "" {
		p.ID, _ = strconv.ParseInt(ids, 10, 64)
	} else {
		p.ID = 0
	}
	if err := a.Store.SaveAgentProfile(&p); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, p)
}

func (a *API) hAgentDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteAgentProfile(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func settingJSON(a *API, w http.ResponseWriter, key string, defaults string) {
	raw := a.Store.Setting(key)
	if raw == "" {
		raw = defaults
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(raw))
}

func settingJSONSet(a *API, w http.ResponseWriter, r *http.Request, key string) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !json.Valid(b) {
		httpErr(w, 400, "invalid JSON")
		return
	}
	if err := a.Store.SetSetting(key, string(b)); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (a *API) hRTGet(w http.ResponseWriter, r *http.Request) {
	settingJSON(a, w, "roundtable", `{"moderator":"","rounds":2,"max_tokens":1200}`)
}

func (a *API) hRTLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	logs, err := a.Store.RoundtableLogs(limit)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if logs == nil {
		logs = []store.RoundtableLog{}
	}
	writeJSON(w, 200, logs)
}

func (a *API) hRTLogDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	l, ok := a.Store.RoundtableLogByID(id)
	if !ok {
		httpErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, l)
}
func (a *API) hRTSet(w http.ResponseWriter, r *http.Request) { settingJSONSet(a, w, r, "roundtable") }
func (a *API) hCompGet(w http.ResponseWriter, r *http.Request) {
	settingJSON(a, w, "compression", `{"enabled":false,"threshold_tokens":24000,"keep_recent":8,"summarizer":"","target_words":500}`)
}

func (a *API) hGlobalFBGet(w http.ResponseWriter, r *http.Request) {
	settingJSON(a, w, "global_fallback", `{"enabled":false,"targets":[]}`)
}
func (a *API) hGlobalFBSet(w http.ResponseWriter, r *http.Request) {
	settingJSONSet(a, w, r, "global_fallback")
}
func (a *API) hCompSet(w http.ResponseWriter, r *http.Request) {
	settingJSONSet(a, w, r, "compression")
}

func (a *API) hRoutersList(w http.ResponseWriter, r *http.Request) {
	rs, err := a.Store.Routers()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if rs == nil {
		rs = []store.Router{}
	}
	writeJSON(w, 200, rs)
}

func (a *API) hRouterSave(w http.ResponseWriter, r *http.Request) {
	var rt store.Router
	if err := json.NewDecoder(r.Body).Decode(&rt); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if ids := r.PathValue("id"); ids != "" {
		rt.ID, _ = strconv.ParseInt(ids, 10, 64)
	} else {
		rt.ID = 0
	}
	if err := a.Store.SaveRouter(&rt); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, rt)
}

func (a *API) hRouterDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteRouter(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// Named fusions mirror named routers: same list/save/delete shape, so the WebUI
// section and any script that already drives /admin/api/routers needs no new
// vocabulary to drive /admin/api/fusions.

func (a *API) hFusionsList(w http.ResponseWriter, r *http.Request) {
	fs, err := a.Store.Fusions()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if fs == nil {
		fs = []store.Fusion{}
	}
	writeJSON(w, 200, fs)
}

func (a *API) hFusionSave(w http.ResponseWriter, r *http.Request) {
	var f store.Fusion
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if ids := r.PathValue("id"); ids != "" {
		f.ID, _ = strconv.ParseInt(ids, 10, 64)
	} else {
		f.ID = 0
	}
	if err := a.Store.SaveFusion(&f); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, f)
}

func (a *API) hFusionDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.Store.DeleteFusion(id); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *API) hFusionGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.Proxy.FusionConfig())
}

func (a *API) hFusionSet(w http.ResponseWriter, r *http.Request) {
	var c proxy.FusionConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(c)
	if err := a.Store.SetSetting("fusion", string(b)); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

func (a *API) hAutoRouteGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.Proxy.AutoRouterConfig())
}

func (a *API) hAutoRouteSet(w http.ResponseWriter, r *http.Request) {
	var c proxy.AutoRouterConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(c)
	if err := a.Store.SetSetting("auto_router", string(b)); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c)
}

func (a *API) hModelMapGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.Store.ModelMap())
}

func (a *API) hModelMapPut(w http.ResponseWriter, r *http.Request) {
	var m map[string]string
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if err := a.Store.SetModelMap(m); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, m)
}

func (a *API) hStats(w http.ResponseWriter, r *http.Request) {
	st, err := a.Store.Stats()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if st == nil {
		st = []store.ModelStat{}
	}
	writeJSON(w, 200, st)
}

// hUsage serves the durable per-day usage rollup (usage_daily). Unlike
// /admin/api/traces — a rolling 5000-row buffer, ~22 h at real volume — this
// survives indefinitely, so "what burned the plan this week" is answerable
// from the proxy itself. Watch two columns in particular:
//
//	no_usage : responses that carried no token accounting at all (a provider
//	           invisible to billing oversight)
//	fellback : attempts that failed and continued down the fallback chain,
//	           i.e. one logical call billed against several providers.
//
// ?since=YYYY-MM-DD (default: 30 days back), ?limit=N.
func (a *API) hUsage(w http.ResponseWriter, r *http.Request) {
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	if since == "" {
		since = time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.UsageDaily(since, limit)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSON(w, 200, rows)
}

func (a *API) hTraces(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ts, err := a.Store.Traces(after, limit)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if ts == nil {
		ts = []store.Trace{}
	}
	writeJSON(w, 200, ts)
}

func (a *API) hLogStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := a.Proxy.Hub.Subscribe()
	defer a.Proxy.Hub.Unsubscribe(ch)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case t := <-ch:
			b, _ := json.Marshal(t)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
