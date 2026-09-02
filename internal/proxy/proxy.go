// Package proxy implements the data plane: inbound dialect endpoints
// (/v1/chat/completions, /v1/messages, /api/chat), provider resolution,
// declarative transforms, and stream re-framing between dialects.
package proxy

import (
	"crypto/subtle"

	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/transform"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// Build identity, set from main via -ldflags (see Makefile). Served by
// GET /api/version so `make deploy` can prove the new binary is the one
// answering.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

const snippetMax = 2000

// Hub broadcasts trace events to live subscribers (WebUI SSE, TUI tail).
type Hub struct {
	mu   sync.Mutex
	subs map[chan store.Trace]bool
}

func NewHub() *Hub { return &Hub{subs: map[chan store.Trace]bool{}} }

func (h *Hub) Subscribe() chan store.Trace {
	ch := make(chan store.Trace, 64)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan store.Trace) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *Hub) Publish(t store.Trace) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- t:
		default: // slow subscriber: drop rather than block the data plane
		}
	}
	h.mu.Unlock()
}

type Proxy struct {
	Store     *store.Store
	Hub       *Hub
	Client    *http.Client
	models    modelCache
	vision    visionMetaCache
	ctxmeta   contextMetaCache
	summaries summaryCache
	inflight  inflightCounter
	poolload  poolLoadCache
}

func New(s *store.Store) *Proxy {
	return &Proxy{Store: s, Hub: NewHub(), Client: &http.Client{Timeout: upstreamTimeout(s), Transport: providerTransport()},
		models:  modelCache{entries: map[int64]modelCacheEntry{}},
		vision:  visionMetaCache{entries: map[string]bool{}},
		ctxmeta: contextMetaCache{entries: map[string]int{}}}
}

func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "openai", "") })
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "responses", "") })
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "anthropic", "") })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "ollama", "") })
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) { p.handleImages(w, r, "", nil) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleModels(w, r, "") })
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleTags(w, r, "") })

	// Per-provider virtual mounts: /p/{provider}/... scopes every call to one
	// provider and lists only its models (bare ids). This lets a harness treat
	// each cfrproxy provider as its own OpenAI endpoint — the basis for the
	// router→provider→model drill-down in pickers.
	mux.HandleFunc("POST /p/{provider}/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "openai", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/v1/responses", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "responses", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "anthropic", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/api/chat", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "ollama", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/v1/images/generations", func(w http.ResponseWriter, r *http.Request) { p.handleImagesScoped(w, r, r.PathValue("provider")) })
	mux.HandleFunc("GET /p/{provider}/v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleModels(w, r, r.PathValue("provider")) })
	mux.HandleFunc("GET /p/{provider}/api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleTags(w, r, r.PathValue("provider")) })

	// Share endpoints: /e/{name}/... is a scoped, key-authed view exposing only
	// a curated model set (or forcing every request to one model / the auto
	// router). Share the URL + the endpoint's own API key with someone else.
	mux.HandleFunc("POST /e/{endpoint}/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "openai") })
	mux.HandleFunc("POST /e/{endpoint}/v1/responses", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "responses") })
	mux.HandleFunc("POST /e/{endpoint}/v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "anthropic") })
	mux.HandleFunc("POST /e/{endpoint}/api/chat", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "ollama") })
	mux.HandleFunc("POST /e/{endpoint}/v1/images/generations", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointImages(w, r) })
	mux.HandleFunc("GET /e/{endpoint}/v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointModels(w, r) })
	mux.HandleFunc("GET /e/{endpoint}/api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointModels(w, r) })
	// Skill lazy-loading: list the skills assigned to this share endpoint, and
	// fetch one skill's full SKILL.md on demand (see internal/proxy/skills.go).
	mux.HandleFunc("GET /e/{endpoint}/skills", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointSkills(w, r) })
	mux.HandleFunc("GET /e/{endpoint}/skills/{name}", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointSkill(w, r) })
	// Same, provider-scoped: when skills are assigned to a provider, the catalog
	// injected into /p/{provider} requests points here (public-key authed).
	mux.HandleFunc("GET /p/{provider}/skills", func(w http.ResponseWriter, r *http.Request) { p.handleProviderSkills(w, r) })
	mux.HandleFunc("GET /p/{provider}/skills/{name}", func(w http.ResponseWriter, r *http.Request) { p.handleProviderSkill(w, r) })

	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": Version, "commit": Commit, "built": BuildDate})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
}

// scopedModelIDs returns bare model ids for one provider. When the provider
// has a pinned (curated) list and all==false, only pins are returned — this
// is what keeps harness pickers short. all==true returns the live catalog.
// ScopedModelIDs exposes scopedModelIDs to the admin API, which needs the same
// list the data-plane /p/{provider}/v1/models route returns but served from
// behind /admin/ basic auth.
func (p *Proxy) ScopedModelIDs(ctx context.Context, provider string, all bool) []string {
	return p.scopedModelIDs(ctx, provider, all)
}

func (p *Proxy) scopedModelIDs(ctx context.Context, provider string, all bool) []string {
	if isFusionMount(provider) {
		return p.fusionModelIDs(true)
	}
	prov, ok := p.Store.ProviderByName(provider)
	if !ok {
		return nil
	}
	if !all {
		if pins := splitList(prov.PinnedModels); len(pins) > 0 {
			return pins
		}
	}
	ids := p.ModelsCached(ctx, prov)
	if len(ids) == 0 {
		seen := map[string]bool{}
		if prov.DefaultModel != "" {
			ids = append(ids, prov.DefaultModel)
			seen[prov.DefaultModel] = true
		}
		for _, a := range strings.Split(prov.Models, ",") {
			if a = strings.TrimSpace(a); a != "" && !seen[a] {
				ids = append(ids, a)
			}
		}
	}
	return ids
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request, scope string) {
	// Gate the inventory endpoints too. These cost nothing to serve, but they
	// enumerate every provider and model this proxy fronts — access logs showed
	// unauthenticated scanners harvesting /v1/models and walking /p/<provider>/
	// mounts. handle() was already gated; these were not.
	if !p.publicKeyOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ids []string
	if scope != "" {
		ids = p.scopedModelIDs(r.Context(), scope, r.URL.Query().Get("all") != "")
	} else {
		ids = p.AllModelIDs(r.Context())
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		m := map[string]any{"id": id, "object": "model", "owned_by": "cfrproxy"}
		// Advertise the context window so harnesses stop guessing it from the
		// model id. Both spellings, because clients disagree on which to read;
		// omitted entirely when unknown, since a wrong number is worse than
		// none — the harness then falls back to its own resolution.
		if n := p.advertisedContext(scope, id); n > 0 {
			m["context_length"] = n
			m["context_window"] = n
		}
		data = append(data, m)
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) handleTags(w http.ResponseWriter, r *http.Request, scope string) {
	// Gate the inventory endpoints too. These cost nothing to serve, but they
	// enumerate every provider and model this proxy fronts — access logs showed
	// unauthenticated scanners harvesting /v1/models and walking /p/<provider>/
	// mounts. handle() was already gated; these were not.
	if !p.publicKeyOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ids []string
	if scope != "" {
		ids = p.scopedModelIDs(r.Context(), scope, r.URL.Query().Get("all") != "")
	} else {
		ids = p.AllModelIDs(r.Context())
	}
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{"name": id, "model": id, "modified_at": time.Now().UTC().Format(time.RFC3339)})
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// publicKeyOK gates data-plane requests that arrived through a reverse proxy
// (identified by X-Forwarded-For / X-Real-IP, which LAN-direct clients never
// send). When settings key "public_api_keys" is set, proxied requests must
// carry one of those keys as Bearer or x-api-key. Direct LAN traffic is
// unaffected, so local harnesses keep working keyless.
func (p *Proxy) publicKeyOK(r *http.Request) bool {
	// A direct connection from a trusted network is keyless so local harnesses
	// just work. "Direct" means no reverse proxy is in front of it: a request
	// that arrives with forwarding headers is judged as external even when the
	// proxy itself sits on the LAN. Previously the ABSENCE of those headers was
	// the whole test, so anything that could reach the raw port — a container,
	// a stray VPN peer, a forgotten port-forward — had every subscription.
	forwarded := r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != ""
	if !forwarded && p.peerTrusted(r) {
		return true
	}
	// An authenticated admin is always allowed through. The WebUI's model
	// scanner calls /p/<provider>/v1/models?all=1 from the browser, which is
	// NOT under /admin/ and so is subject to this gate; without this bypass,
	// turning the gate on breaks "Scan models" for anyone using the admin UI
	// remotely. Same credentials as /admin/ (see api.basicAuth).
	if u, pw, ok := r.BasicAuth(); ok && p.Store.VerifyAdmin(u, pw) {
		return true
	}
	got := r.Header.Get("x-api-key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if got == "" {
		return false
	}
	for _, k := range splitList(p.Store.Setting("public_api_keys")) {
		if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// defaultTrustedCIDRs are the networks whose direct peers are keyless when
// the trusted_cidrs setting is unset: loopback, RFC1918, the Tailscale CGNAT
// range, and the IPv6 equivalents.
var defaultTrustedCIDRs = mustCIDRs("127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fe80::/10,fc00::/7")

func mustCIDRs(list string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range splitList(list) {
		_, n, err := net.ParseCIDR(c)
		if err == nil {
			out = append(out, n)
		}
	}
	return out
}

// peerTrusted reports whether the TCP peer is inside trusted_cidrs (or the
// defaults). An unparseable setting trusts nobody rather than everybody.
func (p *Proxy) peerTrusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	cidrs := defaultTrustedCIDRs
	if raw := strings.TrimSpace(p.Store.Setting("trusted_cidrs")); raw != "" {
		cidrs = mustCIDRs(raw)
	}
	for _, n := range cidrs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// handle serves the /p/{provider}/... mounts. The path segment is canonicalised
// to the provider's stored name before it becomes a "provider/model" string:
// ResolveModel compares that prefix with EqualFold, so an off-by-a-space or
// off-by-case mount ("/p/Qwen%20/", "/p/qwen/") failed the prefix match and fell
// through to the global fuzzy search — silently answering a Qwen request from
// whichever other provider happened to fuzzy-match the model id. An unknown
// provider is now a 404 rather than a silent reroute.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request, inbound, scope string) {
	if scope != "" && !isFusionMount(scope) {
		prov, ok := p.Store.ProviderByName(scope)
		if !ok {
			httpErr(w, inbound, 404, "unknown provider mount: "+scope)
			return
		}
		scope = prov.Name
	}
	p.handleCore(w, r, inbound, scope, nil)
}

func (p *Proxy) handleCore(w http.ResponseWriter, r *http.Request, inbound, scope string, ep *store.Endpoint) {
	start := time.Now()
	if ep == nil && !p.publicKeyOK(r) { // share endpoints authenticate via their own key
		// Record it. This rejection used to return before any trace was
		// written, so a client configured without the key produced HTTP 401s
		// and a completely empty Live Traces view — the failure existed only on
		// the client's side of the wire, which makes it look like the proxy is
		// silently dropping calls.
		p.recordRejection(r, inbound, 401, "public access requires a valid API key")
		httpErr(w, inbound, 401, "public access requires a valid API key (Authorization: Bearer <key> or x-api-key)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		httpErr(w, inbound, 400, "read body: "+err.Error())
		return
	}
	// Per-request caveman override (header or body). The body form is stripped
	// here so it never reaches an upstream that would reject an unknown param.
	cmMode, cmSet := ParseCavemanMode(r.Header.Get("X-Caveman"))
	if !cmSet {
		if raw, ok := peekBodyParam(body, "caveman"); ok {
			cmMode, cmSet = ParseCavemanMode(raw)
		}
	}
	if stripped, ok := stripBodyParam(body, "caveman"); ok {
		body = stripped
	}
	// Providers like DeepSeek 400 on empty system messages; several agent
	// stacks emit them when no system prompt is configured.
	body = sanitizeEmptySystem(body)
	// A caller that omits max_tokens gets llama.cpp's n_predict = -1, i.e.
	// "generate until the 262K context is full", which parks a serving slot
	// for hours and queues everyone behind it. Supply a ceiling when the
	// operator has configured one; an explicit client limit always wins.
	if n, err := strconv.Atoi(strings.TrimSpace(p.Store.Setting("default_max_tokens"))); err == nil && n > 0 {
		body = defaultMaxTokens(body, n)
	}
	// Resolved per candidate (a failover may land on a provider with a
	// different standing setting); kept here so the response path can see
	// which mode actually served the request.
	var cmEff CavemanMode
	req, err := parseInbound(inbound, body)
	if err != nil {
		httpErr(w, inbound, 400, err.Error())
		return
	}
	// scoped mount forces the provider; a bare model id is qualified to it, and
	// a "provider/model" that names a different provider is corrected.
	reqModel := req.Model
	if scope != "" {
		reqModel = scope + "/" + p.stripProviderPrefix(reqModel)
	}
	// share-endpoint model policy: force overrides; otherwise the requested
	// model must be on the allow-list.
	if ep != nil {
		if ep.ForceModel != "" {
			reqModel = ep.ForceModel
		} else if !p.modelAllowed(*ep, reqModel) {
			httpErr(w, inbound, 403, "model not permitted on this endpoint: "+reqModel)
			return
		}
		// Lazy-load skills: if this endpoint has skills assigned (matching the
		// requested model), prepend a compact catalog to the system prompt. The
		// model pulls a skill's full instructions from /e/{ep}/skills/{name} only
		// when it needs one. Mirrors the InjectDocs path below.
		if skills := p.Store.SkillsForEndpoint(ep.ID, reqModel); len(skills) > 0 {
			if cat := skillCatalog(skills, reqBase(r)+"/e/"+ep.Name); cat != "" {
				if req.System != "" {
					req.System = cat + "\n\n" + req.System
				} else {
					req.System = cat
				}
			}
		}
	}
	autoNote := ""
	if cnote := p.Compress(r.Context(), req); cnote != "" {
		autoNote = cnote + " "
	}
	// fusion: fan out to participants, then this request becomes the judge's
	// synthesis call (which then routes/streams normally below).
	if fcfg, ok := p.FusionConfigFor(reqModel); ok {
		if judge, note, ok := p.FusionWith(r.Context(), req, fcfg); ok {
			reqModel = judge
			autoNote += note
		}
	}
	// resolve the router config: default (auto/auto-plan) or a named custom
	// router (auto:NAME / auto-plan:NAME).
	var rcfg AutoRouterConfig
	haveRouter, wantPlan := false, false
	switch {
	case reqModel == "auto-plan" || strings.HasSuffix(reqModel, "/auto-plan"):
		rcfg, haveRouter, wantPlan = p.AutoRouterConfig(), true, true
	case reqModel == "auto" || reqModel == "cfr-auto" || strings.HasSuffix(reqModel, "/auto"):
		rcfg, haveRouter = p.AutoRouterConfig(), true
	case strings.HasPrefix(reqModel, "auto-plan:"):
		rcfg, haveRouter = p.NamedRouterConfig(reqModel[len("auto-plan:"):])
		wantPlan = haveRouter
	case strings.HasPrefix(reqModel, "auto:"):
		rcfg, haveRouter = p.NamedRouterConfig(reqModel[len("auto:"):])
	}
	if haveRouter {
		if wantPlan {
			if plan := p.PlanWith(r.Context(), req, rcfg); plan != "" {
				brief := "Execution briefing from the planning stage (follow unless clearly wrong):\n" + plan
				appendBriefing(req, brief)
				autoNote = "planned "
			}
		}
		routed, bucket := p.AutoRouteWith(r.Context(), req, rcfg)
		if routed != "" {
			reqModel = routed
			autoNote += "auto→" + bucket + "→" + routed
			// A route may target a fusion ("code": "fusion:deep"). The
			// pre-routing check above only saw the router id, so resolve it
			// here too — otherwise the bucket would route to a model id no
			// provider serves and 503.
			if fcfg, ok := p.FusionConfigFor(reqModel); ok {
				if judge, note, ok := p.FusionWith(r.Context(), req, fcfg); ok {
					reqModel = judge
					autoNote += " " + note
				}
			}
		}
	}
	prov, model, err := p.ResolveModel(r.Context(), reqModel)
	if err != nil {
		httpErr(w, inbound, 503, err.Error())
		return
	}
	req.Model = model

	tr := &store.Trace{TS: start.UnixMilli(), Provider: prov.Name, Model: model, Inbound: inbound,
		Stream: req.Stream, ReqSnip: snip(body), Note: autoNote, Client: clientLabel(r)}
	lastUpstreamByte := func() time.Time { return time.Time{} }
	var sniff *timingsSniffer      // set for streamed responses; see timingsSniffer
	var prefixSnap *prefixSnapshot // static head of the request actually sent
	defer func() {
		tr.LatencyMS = time.Since(start).Milliseconds()
		if t := lastUpstreamByte(); !t.IsZero() {
			tr.PostUS = time.Since(t).Microseconds()
		}
		sniff.apply(tr)
		p.Store.AddTrace(tr)
		p.Hub.Publish(*tr)
		logTrace(r, tr)
		// after AddTrace so a slow/failed log write can never delay the trace
		writeCacheRecord(tr)
		recordPrefix(prefixSnap, tr)
	}()

	// candidate chain: primary, then its fallback chain followed transitively
	// (cycle-safe, max 3 hops). Transient failures retry once per candidate,
	// then move down the chain.
	// A pooled logical model fans out to whichever instance is least busy;
	// llama.cpp queues it there if every member is full.
	var pooled poolChoice
	if spec := p.poolSpecFor(model); spec != nil {
		pooled = p.routePool(spec, prov, req)
		p.inflight.add(pooled.member, 1)
		defer p.inflight.add(pooled.member, -1)
		model = pooled.member
		if spec.Affinity {
			// Only annotated for affinity pools: a plain least-busy pool keeps
			// the trace it has always written. Appended to tr.Note rather than
			// autoNote because autoNote != "" disables raw passthrough, and a
			// pooled request has no reason to lose it.
			tr.Note = strings.TrimSpace(tr.Note + " pool→" + pooled.member + " (" + pooled.why + ")")
		}
	}
	cands := []candidate{{prov: prov, model: model}}
	// Sibling instances of the same pool come first in the chain: they are the
	// SAME weights on another card, so an instance that is down or erroring
	// costs a retry rather than a reroute onto a different (often paid) model.
	// Deliberately not gated on no_fallback — that flag exists to stop silent
	// reroutes to other providers/models, which is the opposite of this.
	if pooled.failover {
		for _, m := range pooled.rest {
			cands = append(cands, candidate{prov: prov, model: m, sibling: true})
		}
	}
	// Fallback can be pinned off, per provider or per share endpoint. When it
	// is, the chain stays a single candidate: the caller gets this provider's
	// real error rather than a silent reroute. That matters because the reroute
	// is invisible and expensive — a local model going down otherwise walks the
	// request onto paid providers, and a share key handed to someone else could
	// spend on providers they were never granted.
	noFallback := prov.NoFallback || (ep != nil && ep.NoFallback)
	seen := map[int64]bool{prov.ID: true}
	cur := prov
	// The per-provider chain is gated by the provider_fallback setting (see
	// providerfallback.go): off by default, so the visible global chain alone
	// decides failover unless the operator opts the per-provider hops back in.
	providerChain := p.ProviderFallbackConfig().Enabled
	for hop := 0; providerChain && !noFallback && hop < 3 && cur.Fallback != ""; hop++ {
		fprov, fmodel, ferr := p.ResolveModel(r.Context(), cur.Fallback)
		if ferr != nil || seen[fprov.ID] {
			break
		}
		seen[fprov.ID] = true
		cands = append(cands, candidate{prov: fprov, model: fmodel, failover: true})
		cur = fprov
	}
	// then the global auto-fallback chain, so every model has a safety net even
	// when its provider has no `fallback` of its own
	// Does this request carry an image? Decided once from the raw body, and
	// used for both chain ordering and the passthrough rule below.
	reqHasImage := bodyHasImage(body)
	// For an image request the vision chain goes FIRST: the generic global
	// chain is ordered for text availability and will happily hand a picture to
	// a text-only model, which answers "I can't see the image" with a 200 and
	// ends the request successfully-but-wrongly.
	blindPrimary := false
	if reqHasImage && !noFallback {
		withVis := p.appendVisionFallback(r.Context(), cands, ep, reqHasImage)
		if !p.visionCapableFor(r.Context(), prov, model) && len(withVis) > len(cands) {
			// The primary cannot see. Put the vision chain IN FRONT of it
			// rather than behind: a text-only model handed a picture usually
			// does not error at all, it answers with a fluent hallucination
			// and HTTP 200, and no error-driven failover rule can catch that.
			// The blind primary stays on as the last resort, so a vision chain
			// that is misconfigured or entirely unreachable degrades to the old
			// behaviour instead of failing the request outright.
			blindPrimary = true
			cands = append(append([]candidate{}, withVis[len(cands):]...), cands...)
		} else {
			cands = withVis
		}
	}
	if !noFallback {
		cands = p.appendGlobalFallback(r.Context(), cands, ep)
	}
	// Once a vision chain is in play, ONLY models that can see may serve the
	// image. The global chain is ordered for text availability and happily
	// contains text-only models. Gated on the chain existing, so a deployment
	// with no vision targets keeps its old behaviour rather than losing images.
	visionChainActive := false
	for _, c := range cands {
		if c.vision {
			visionChainActive = true
			break
		}
	}

	var resp *http.Response
	var used candidate
	var respRules []transform.Rule
	var passth bool
	lastErr := ""
	// primaryErr keeps the ORIGINALLY-REQUESTED provider's own failure, separate
	// from lastErr which keeps walking down the chain. The chat banner names the
	// primary, so it has to quote the primary's reason.
	primaryErr := ""
	if blindPrimary {
		// Seed the reason so the trace and the failover banner say what
		// actually happened. Without it the primary is never attempted, lastErr
		// stays empty, and the banner renders a bare "unavailable".
		lastErr = fmt.Sprintf("%s/%s has no image support — routed to the vision chain", prov.Name, model)
	}
	var softErrs []string // transient errors that were retried/failed-over past
	for _, c := range cands {
		candStatus := 0 // HTTP status of this candidate's failure, 0 = never responded
		// Never hand a picture to a model that cannot read one. Candidates from
		// the vision chain are exempt: listing a model in vision_fallback is an
		// explicit operator declaration that it sees, and that outranks both the
		// name heuristic and a silent provider.
		if visionChainActive && !c.vision && !p.visionCapableFor(r.Context(), c.prov, c.model) {
			lastErr = fmt.Sprintf("%s/%s: skipped, cannot accept images", c.prov.Name, c.model)
			softErrs = append(softErrs, lastErr)
			p.recordAttemptFailure(tr, c, lastErr, 0)
			continue
		}
		creq := *req
		creq.Model = c.model
		if c.prov.InjectDocs && c.prov.DocMarkdown != "" {
			if creq.System != "" {
				creq.System = c.prov.DocMarkdown + "\n\n" + creq.System
			} else {
				creq.System = c.prov.DocMarkdown
			}
		}
		// Lazy-load skills assigned to this provider (any /p/{provider} or routed
		// request): prepend the catalog so the model can fetch a skill's full
		// instructions from /p/{provider}/skills/{name} on demand. Mirrors the
		// InjectDocs path above and, like it, forces the translated send path
		// (skillsInjected feeds rawOK below).
		skillsInjected := false
		if skills := p.Store.SkillsForProvider(c.prov.ID, c.model); len(skills) > 0 {
			if cat := skillCatalog(skills, reqBase(r)+"/p/"+c.prov.Name); cat != "" {
				if creq.System != "" {
					creq.System = cat + "\n\n" + creq.System
				} else {
					creq.System = cat
				}
				skillsInjected = true
			}
		}
		// Caveman payload compression (opt-in per provider, and per share
		// endpoint via ep.Caveman). Runs AFTER docs/skills injection so it can
		// never touch what those just added to System — it only rewrites bulky
		// `tool` results in the message tail, deterministically, so the
		// prefix-cached head is byte-identical turn over turn.
		cmEff = CavemanModeFor(cmMode, cmSet, ep != nil && ep.Caveman, c.prov.Caveman)
		cavemanApplied := false
		if cmEff.CompressIn() {
			// cmSet == the caller asked on THIS request (header/body), which
			// unlocks compressing large user messages too.
			cm := CavemanCompress(&creq, cmSet)
			cavemanApplied = cm.Msgs > 0
			// Accumulate across failover attempts: a request that gets
			// compressed then retried on another provider did the work twice,
			// and the trace should say so rather than report only the last try.
			tr.CMMsgs += cm.Msgs
			tr.CMBefore += cm.Before
			tr.CMAfter += cm.After
		}
		// Enforce the model's usable context BEFORE the upstream has to reject
		// it. A harness cannot always avoid this on its own: Claude Code sizes
		// its window from the context we advertise and compacts against it,
		// but a single turn can add hundreds of thousands of tokens at once —
		// four MCP calls that each return the whole network — and land past
		// the limit with no turn boundary in between at which to compact. The
		// upstream's only answer is a 400 that kills the turn, so squeeze the
		// bulky tool results here instead. Only fires when the request already
		// does not fit, so a conversation that fits is byte-identical to
		// before and the prefix cache is untouched.
		if !cavemanApplied {
			if ctxLimit := p.ContextLimitFor(ep, c.prov, c.model); ctxLimit > 0 {
				// Reserve room for the answer and for the estimate being low.
				// estTokens is chars/4; real Claude Code traffic measures ~3.9
				// chars/token and dense JSON tool results closer to 2.9, so the
				// estimate reads under. The upstream needs prompt + completion
				// to fit, not just the prompt.
				reserve := creq.MaxTokens
				if reserve <= 0 || reserve > ctxLimit/8 {
					reserve = ctxLimit / 8
				}
				if est := estTokens(&creq); est > ctxLimit-reserve {
					if cm := CavemanCompress(&creq, true); cm.Msgs > 0 {
						cavemanApplied = true
						cmEff = CMIn
						tr.CMMsgs += cm.Msgs
						tr.CMBefore += cm.Before
						tr.CMAfter += cm.After
						softErrs = append(softErrs, fmt.Sprintf("%s: request ~%d tok against a %d-tok window (reserving %d) — compressed %d tool result(s) to fit", c.prov.Name, est, ctxLimit, reserve, cm.Msgs))
					}
				}
			}
		}
		// Record the resolved mode even when nothing compressed, so the trace
		// distinguishes "never asked" from "asked and declined/nothing matched".
		if cmSet || cmEff != CMOff {
			tr.CMMode = string(cmEff)
		}
		// creq is now the request as the model will see it (docs + skills
		// injected), which is what a warmup must replay to match byte-for-byte.
		prefixSnap = snapshotPrefix(&creq, c.prov.Name)
		reqRules, respR, err := p.rules(c.prov.ID, inbound)
		if err != nil {
			tr.Status, tr.Err = 500, err.Error()
			httpErr(w, inbound, 500, err.Error())
			return
		}
		// failover responses always take the translated path so the alert
		// notice can be injected into the visible content
		// failover and auto-routed requests take the translated path: failover
		// injects the alert, auto/auto-plan rewrites model and system context —
		// raw passthrough would silently drop those.
		// rawOK: the client's body can be forwarded verbatim (only the model key
		// swapped), which is the only path that preserves image content parts.
		otype := p.otype(c.prov, c.model)
		// cavemanApplied forces the TRANSLATED send path. Raw passthrough
		// forwards the ORIGINAL body, so a compressed creq would be recorded
		// in the trace metrics and then silently discarded — the request the
		// upstream actually sees would be uncompressed. Same reason
		// InjectDocs and skillsInjected disable passthrough.
		rawOK := autoNote == "" && inbound == otype && len(reqRules) == 0 && len(respR) == 0 && !c.prov.InjectDocs && !skillsInjected && !cavemanApplied
		// Ordinary failover takes the translated path so the alert banner can be
		// injected into visible content. An image request can too, but only
		// where the outbound dialect carries pictures through wire.Request —
		// see dialectCarriesImages. Dialects that don't must still be skipped:
		// sending a flattened body would ask a blind question and get a
		// confidently wrong answer back.
		passthrough := rawOK && (!c.failover || reqHasImage)
		if c.failover && reqHasImage && !passthrough && !dialectCarriesImages(otype) {
			// The image cannot be preserved through this target (dialect
			// mismatch, or transform rules rewrite the body). Skipping is the
			// honest outcome — sending the translated body would ask a blind
			// question and get a confidently wrong answer back.
			lastErr = fmt.Sprintf("%s: image failover skipped (cannot preserve image through %s translation)", c.prov.Name, c.prov.Type)
			softErrs = append(softErrs, lastErr)
			p.recordAttemptFailure(tr, c, lastErr, 0)
			continue
		}
		var outBody []byte
		if passthrough {
			outBody = rawWithModel(body, c.model)
		} else {
			outBody, err = buildOutbound(otype, &creq)
			if err != nil {
				tr.Status, tr.Err = 500, err.Error()
				httpErr(w, inbound, 500, err.Error())
				return
			}
			outBody = transform.Apply(outBody, reqRules)
		}
		// maxAttempts is 2 (one transient retry). Dropping a provider-rejected
		// parameter buys one extra attempt, so the recovery never eats the
		// transient budget.
		maxAttempts, droppedParam, paramRetry := 2, false, false
		cavemanRescued := false
		for attempt := 0; attempt < maxAttempts && resp == nil; attempt++ {
			if attempt > 0 && !paramRetry {
				time.Sleep(1200 * time.Millisecond)
			}
			paramRetry = false
			r2, err := p.send(r.Context(), c.prov, providerPath(otype), outBody)
			if err != nil {
				lastErr = c.prov.Name + ": " + err.Error()
				if primaryErr == "" && c.prov.Name == prov.Name {
					primaryErr = lastErr
				}
				if r.Context().Err() != nil {
					return // client gone
				}
				continue
			}
			if transientStatus(r2.StatusCode) {
				eb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
				r2.Body.Close()
				lastErr = fmt.Sprintf("%s: HTTP %d %s", c.prov.Name, r2.StatusCode, snip(eb))
				if primaryErr == "" && c.prov.Name == prov.Name {
					primaryErr = lastErr
				}
				candStatus = r2.StatusCode
				softErrs = append(softErrs, lastErr)
				continue
			}
			// Usage/quota exhaustion often comes back as a 4xx rather than 429 —
			// e.g. Anthropic returns "out of extra usage" as a 400 invalid_request_error
			// when a subscription's rolling usage window trips under load. Retrying the
			// same provider won't help, but it IS exactly what a fallback chain is for,
			// so move to the next candidate instead of hard-failing the harness.
			if r2.StatusCode >= 400 && r2.StatusCode < 500 {
				eb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
				r2.Body.Close()
				// 402 Payment Required is definitionally an exhausted account,
				// whatever wording the provider chose — don't play whack-a-mole
				// with error strings for it.
				if r2.StatusCode == 402 || usageExhausted(eb) {
					lastErr = fmt.Sprintf("%s: usage cap (HTTP %d) %s", c.prov.Name, r2.StatusCode, snip(eb))
					if primaryErr == "" && c.prov.Name == prov.Name {
						primaryErr = lastErr
					}
					candStatus = r2.StatusCode
					softErrs = append(softErrs, lastErr)
					break // stop retrying this provider; try the next candidate
				}
				// context overflow: a larger-window model down the chain can
				// still serve this, so don't hard-fail the harness either
				if contextExceeded(eb) {
					// ...but first try to make it fit. A harness turn that
					// overflows is usually one enormous tool result away from
					// fitting — an MCP call that returned every client on the
					// network, a whole log, a directory dump. Compressing
					// those is exactly what caveman is for, and it rescues the
					// turn on THIS model, which matters because the operator
					// may have pinned the provider (no_fallback) precisely so
					// their local model keeps the conversation. Costs nothing
					// when nothing bulky is there to compress: cm.Msgs == 0
					// falls straight through to the failover path below.
					if !cavemanRescued {
						if cm := CavemanCompress(&creq, true); cm.Msgs > 0 {
							if nb, berr := buildOutbound(otype, &creq); berr == nil {
								outBody = transform.Apply(nb, reqRules)
								cavemanRescued, paramRetry = true, true
								maxAttempts++
								tr.CMMsgs += cm.Msgs
								tr.CMBefore += cm.Before
								tr.CMAfter += cm.After
								tr.CMMode = string(CMIn)
								softErrs = append(softErrs, fmt.Sprintf("%s: context overflow — compressed %d tool result(s) (~%d→~%d bytes) and retried", c.prov.Name, cm.Msgs, cm.Before, cm.After))
								continue
							}
						}
					}
					lastErr = fmt.Sprintf("%s: context overflow (HTTP %d) %s", c.prov.Name, r2.StatusCode, snip(eb))
					if primaryErr == "" && c.prov.Name == prov.Name {
						primaryErr = lastErr
					}
					candStatus = r2.StatusCode
					softErrs = append(softErrs, lastErr)
					break
				}
				// image-specific rejection: a vision-capable model further down
				// the chain can still serve this. These arrive as 400s, which a
				// harness classifies as non-retryable and aborts the whole turn
				// on — exactly the case the vision chain exists to rescue.
				if visionFailure(eb) {
					lastErr = fmt.Sprintf("%s: vision failure (HTTP %d) %s", c.prov.Name, r2.StatusCode, snip(eb))
					if primaryErr == "" && c.prov.Name == prov.Name {
						primaryErr = lastErr
					}
					candStatus = r2.StatusCode
					softErrs = append(softErrs, lastErr)
					break
				}
				// A parameter the harness always sends but this model refuses
				// (omp's enable_thinking:false vs Alibaba's thinking models).
				// Passthrough forwards the client's body verbatim, so this is
				// the only layer that can recover it: drop the named key and
				// retry once. Structural keys are never dropped.
				if !droppedParam {
					if k := rejectedParam(eb); k != "" {
						if nb, ok := stripBodyParam(outBody, k); ok {
							outBody, droppedParam, paramRetry = nb, true, true
							maxAttempts++
							softErrs = append(softErrs, fmt.Sprintf("%s rejected parameter %q — dropped and retried", c.prov.Name, k))
							continue
						}
					}
				}
				// Any other 4xx on a request that carries an image: treat it as
				// "this provider can't take the picture" and move down the
				// chain. The wording varies far more than it can be enumerated
				// — Alibaba says "Download multimodal file timed out",
				// OpenCode's Rust console says "unknown variant `image_url`,
				// expected `text`" — and visionFailure above only catches the
				// phrasings seen so far. This is the same reasoning the 402
				// rule states outright: don't play whack-a-mole with error
				// strings. Bounded by maxVisionHops, and if the body really is
				// malformed then every hop rejects it and the client still
				// gets an error, just a few seconds later.
				if reqHasImage {
					lastErr = fmt.Sprintf("%s: image rejected (HTTP %d) %s", c.prov.Name, r2.StatusCode, snip(eb))
					candStatus = r2.StatusCode
					softErrs = append(softErrs, lastErr)
					break
				}
				// genuine 4xx (bad request/auth) — restore the body for the error path
				r2.Body = io.NopCloser(bytes.NewReader(eb))
			}
			resp = r2
		}
		if resp != nil {
			used, respRules, passth = c, respR, passthrough
			req = &creq
			break
		}
		// This candidate did not serve the request. Record it under ITS OWN
		// name: previously a failed attempt produced no row at all and the
		// reason was only visible inside the *successful* provider's error
		// text, so a provider failing 100% of the time showed a completely
		// empty, healthy-looking panel while every request failed over off it.
		p.recordAttemptFailure(tr, c, lastErr, candStatus)
	}
	if resp == nil {
		if reqHasImage && visionChainActive {
			// Name the actual problem: "502 upstream error" sends the operator
			// hunting the wrong thing when every vision target is down.
			lastErr = "no vision-capable model could serve this image: " + strings.Join(softErrs, " | ")
		}
		tr.Status, tr.Err = 502, lastErr
		httpErr(w, inbound, 502, lastErr)
		return
	}
	upstreamBody := newLastByteReader(resp.Body)
	resp.Body = upstreamBody
	lastUpstreamByte = upstreamBody.lastByte
	if req.Stream {
		// llama.cpp puts `timings` in the final SSE chunk, which is consumed by
		// readStream/copyRaw and never surfaces in the trace. Tap the tail.
		sniff = newTimingsSniffer(resp.Body)
		resp.Body = sniff
	}
	defer resp.Body.Close()
	// Headers are in hand. For a STREAMED response that is genuine
	// time-to-first-token. For a non-streamed one the upstream withholds
	// headers until the whole completion is ready, so recording it would
	// collapse the derived generation window to ~0ms and yield absurd rates.
	if req.Stream {
		tr.TTFBMS = time.Since(start).Milliseconds()
	}
	tr.Provider, tr.Model = used.prov.Name, used.model
	if pooled.active() && used.sibling {
		// The picked instance failed and a sibling served it. Re-point the
		// bindings, or every conversation on the dead card pays this same
		// failed round-trip for the whole affinity TTL.
		pooled.rebind(used.model)
	}
	alert := ""
	if used.failover {
		// The banner must pair the primary's NAME with the primary's OWN reason.
		// It used to print failureLabel(lastErr), which is the last failure in
		// the whole chain — so a local model that died with an empty HTTP 502
		// was announced as "fred quota exhausted" because a later hop ran out
		// of credits. That sends the operator to a billing page instead of to
		// the service that is actually down. Fall back to lastErr only when the
		// primary produced no error of its own.
		primaryReason := primaryErr
		if primaryReason == "" {
			primaryReason = lastErr
		}
		tr.Err = "failover from " + prov.Name + " (" + lastErr + ")"
		// Deliberately terse, and rate-limited per "from -> to" pair rather than
		// per conversation: harnesses inject the current time and other volatile
		// text into the system prompt, so any content-derived key looks new on
		// every call and suppresses nothing. The full reason stays on tr.Err —
		// visible in the trace and the WebUI errors panel — where it belongs.
		if noticeCache.announce(prov.Name + "->" + used.prov.Name + "/" + used.model) {
			alert = fmt.Sprintf("⚠️ failover: %s %s → %s active\n\n",
				prov.Name, failureLabel(primaryReason), used.model)
		}
	} else if used.sibling {
		// Intra-pool retry: same provider, same weights, another card. No
		// banner is injected into the reply — nothing about the answer changed
		// — but the errors panel must still show that an instance is down.
		tr.Err = "pool failover: " + prov.Name + "/" + pooled.member + " → " + used.model + " (" + lastErr + ")"
	} else if len(softErrs) > 0 && resp.StatusCode < 400 {
		// request recovered on the same provider after one or more transient
		// errors (429 rate limit, 5xx, overload). The final response is a
		// success, but record what was hit so it surfaces in the WebUI errors
		// panel — otherwise a retried rate-limit is completely invisible.
		tr.Err = "recovered after transient: " + strings.Join(softErrs, " | ")
	}

	if resp.StatusCode >= 400 { // non-transient provider error (auth, bad request)
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tr.Status = resp.StatusCode
		tr.Err = snip(eb)
		httpErr(w, inbound, resp.StatusCode, fmt.Sprintf("provider %s: %s", used.prov.Name, snip(eb)))
		return
	}

	uotype := p.otype(used.prov, used.model)
	if passth {
		p.copyRaw(w, resp, req.Stream, uotype, tr)
		return
	}

	if req.Stream {
		deltas := make(chan wire.Delta, 16)
		go readStream(uotype, resp.Body, deltas)
		// capture usage from the final delta as it flows through
		var upt, uct, ucached int
		cap := make(chan wire.Delta, 16)
		go func(in <-chan wire.Delta) {
			if alert != "" {
				cap <- wire.Delta{Text: alert}
			}
			for d := range in {
				if d.Finish != "" {
					upt, uct, ucached = d.PromptTokens, d.CompletionTokens, d.CachedTokens
				}
				cap <- d
			}
			close(cap)
		}(deltas)
		if err := writeStream(inbound, w, req.Model, cap); err != nil {
			// The writer stopped early (client gone, or an upstream error). The
			// reader and relay goroutines may still hold deltas; drain them so
			// they can observe the closed body (deferred above) and exit
			// instead of blocking on a channel nobody reads.
			go func() {
				for range cap {
				}
			}()
			tr.Status, tr.Err = 200, "stream aborted: "+err.Error()
			return
		}
		tr.Status = 200
		tr.RespSnip = "(streamed)"
		tr.PromptTokens, tr.CompletionTokens, tr.CachedTokens = upt, uct, ucached
		return
	}

	rb, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, inbound, 502, err.Error())
		return
	}
	norm, err := parseOutboundResponse(uotype, rb)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, inbound, 502, err.Error())
		return
	}
	norm.Model = req.Model
	tr.PromptTokens, tr.CompletionTokens, tr.CachedTokens = norm.PromptTokens, norm.CompletionTokens, norm.CachedTokens
	// rb is the raw upstream body: llama.cpp's `timings` object lives there and
	// does not survive translation into the inbound dialect.
	applyCacheTimings(tr, rb)
	// Output compression (CMOut/CMBoth only — never from a standing checkbox,
	// because this changes what the CALLER receives). Streams are excluded:
	// compressing one would mean buffering the whole reply, destroying the
	// streaming the client asked for. Applied before `alert` is prepended so a
	// failover banner is never truncated away.
	if cmEff.CompressOut() {
		if !req.Stream {
			if cm := CavemanCompressResponse(norm); cm.Msgs > 0 {
				tr.CMMsgs += cm.Msgs
				tr.CMBefore += cm.Before
				tr.CMAfter += cm.After
			}
		} else if tr.Note == "" {
			tr.Note = "caveman: out skipped (streamed)"
		}
	}
	norm.Content = alert + norm.Content
	final := buildInboundResponse(inbound, norm)
	final = transform.Apply(final, respRules)
	tr.Status = 200
	tr.RespSnip = snip(final)
	w.Header().Set("Content-Type", "application/json")
	w.Write(final)
}

type candidate struct {
	prov     store.Provider
	model    string
	failover bool
	// vision marks a candidate appended by the image-specific chain. Such a
	// candidate MUST be sent as raw passthrough: the translated path rebuilds
	// the body from wire.Request, whose content has already been flattened to
	// text with image parts dropped, so a translated "vision fallback" would
	// quietly ask a blind question and get a confident wrong answer.
	vision bool
	// sibling marks another instance of the same pooled model on the same
	// provider. It is NOT a failover in the user-visible sense: the weights,
	// the provider and the answer are identical, so it must not trigger the
	// failover banner or lose raw passthrough — only be recorded.
	sibling bool
}

// transientStatus: worth retrying / failing over. 4xx auth/validation errors
// are not — they'd fail identically anywhere.
func transientStatus(code int) bool {
	switch code {
	case 408, 429, 500, 502, 503, 504, 524, 529:
		return true
	}
	return false
}

// usageExhausted detects provider usage/quota-exhaustion errors that arrive as
// a 4xx (not 429) — these should fail over to the next candidate, not hard-fail.
func usageExhausted(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, m := range []string{
		"out of extra usage",
		"usage limit",
		"usage_limit",
		"insufficient_quota",
		"exceeded your current quota",
		"resource_exhausted",
		"quota exceeded",
		// prepaid/credit exhaustion — same practical meaning: this account
		// cannot serve the request, so retrying it is wasted, fail over instead
		"insufficient balance",
		"no resource package",
		"please recharge",
		"credit balance is too low",
		"insufficient credits",
		"balance exhausted", // xAI: "Grok Build usage balance exhausted"
		"usage balance",
		"out of credits",
		// Plan/entitlement gating: the account exists and the key is valid, but
		// this tier is not allowed to call the endpoint. Retrying is pointless
		// and hard-failing turns into a harness retry storm, so treat it like
		// exhaustion and move down the chain. Matched on wording rather than a
		// bare 403, because 403 also covers permission errors the next provider
		// cannot fix — and 401 must keep hard-failing (TestNoFailoverOn401).
		"upgrade_required",
		"upgrade required",
		"plan_required",
		"plan does not include",
		"not included in your plan",
		"requires a paid plan",
		"subscription required",
		// The account cannot use THIS model right now — typically a plan change
		// still propagating, or a model dropped from the tier's catalogue. It
		// arrives as a 400, which otherwise hard-fails and shows up downstream
		// as a dead agent ("Non-retryable client error"). Another provider in
		// the chain can serve the request, so move on instead.
		"unsupported_model",
		"is not supported on this endpoint",
		"model not available",
		"model is not available",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func rawWithModel(body []byte, model string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	doc["model"] = model
	b, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return b
}

// copyRaw streams the provider's bytes through untouched (passthrough mode),
// scanning them for usage so token/cache stats are captured without altering
// the response.
func (p *Proxy) copyRaw(w http.ResponseWriter, resp *http.Response, stream bool, ptype string, tr *store.Trace) {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	tr.Status = resp.StatusCode
	if stream {
		tr.RespSnip = "(streamed passthrough)"
		fl, _ := w.(http.Flusher)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		sc.Split(scanLinesKeepEnds)
		for sc.Scan() {
			chunk := sc.Bytes()
			w.Write(chunk)
			if fl != nil {
				fl.Flush()
			}
			// anthropic splits usage across message_start (input+cache) and
			// message_delta (output); keep the max so neither is clobbered.
			if pt, ct, cached, ok := usageFromStreamLine(ptype, chunk); ok {
				tr.PromptTokens = maxInt(tr.PromptTokens, pt)
				tr.CompletionTokens = maxInt(tr.CompletionTokens, ct)
				tr.CachedTokens = maxInt(tr.CachedTokens, cached)
			}
		}
		return
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	tr.RespSnip = snip(rb)
	if pt, ct, cached, ok := usageFromBody(ptype, rb); ok {
		tr.PromptTokens, tr.CompletionTokens, tr.CachedTokens = pt, ct, cached
	}
	applyCacheTimings(tr, rb)
	w.Write(rb)
}

// scanLinesKeepEnds splits on \n while preserving the newline, so passthrough
// bytes are forwarded verbatim.
func scanLinesKeepEnds(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// usageFromBody extracts token usage from a complete non-streamed response.
func usageFromBody(ptype string, body []byte) (pt, ct, cached int, ok bool) {
	switch ptype {
	case "anthropic":
		var v struct {
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				OutputTokens         int `json:"output_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &v) == nil && (v.Usage.InputTokens > 0 || v.Usage.OutputTokens > 0) {
			return v.Usage.InputTokens, v.Usage.OutputTokens, v.Usage.CacheReadInputTokens, true
		}
	case "responses":
		// The Responses API names these differently from chat completions; the
		// default branch below would find none of them and log 0/0/0. Reached
		// on the raw-passthrough path, i.e. an inbound /v1/responses request
		// against a Responses-capable openai provider.
		return wire.UsageFromResponsesBody(body)
	case "ollama":
		var v struct {
			PromptEvalCount int `json:"prompt_eval_count"`
			EvalCount       int `json:"eval_count"`
		}
		if json.Unmarshal(body, &v) == nil && (v.PromptEvalCount > 0 || v.EvalCount > 0) {
			return v.PromptEvalCount, v.EvalCount, 0, true
		}
	default:
		var v struct {
			Usage struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &v) == nil && (v.Usage.PromptTokens > 0 || v.Usage.CompletionTokens > 0) {
			return v.Usage.PromptTokens, v.Usage.CompletionTokens, v.Usage.PromptTokensDetails.CachedTokens, true
		}
	}
	return 0, 0, 0, false
}

// usageFromStreamLine pulls usage out of one SSE/NDJSON line if present.
// Anthropic splits usage across message_start (input+cache) and message_delta
// (output), so callers keep the last non-zero values seen.
func usageFromStreamLine(ptype string, line []byte) (pt, ct, cached int, ok bool) {
	data := line
	if i := bytes.Index(line, []byte("data:")); i >= 0 {
		data = bytes.TrimSpace(line[i+5:])
	}
	if !bytes.Contains(data, []byte("usage")) && !bytes.Contains(data, []byte("eval_count")) {
		return 0, 0, 0, false
	}
	return usageFromBody(ptype, data)
}

// injectProviderHeaders sets the provider's configured extra outbound headers
// (Provider.Headers, a JSON object of name -> value) onto an outbound request.
// A value of "@file:<path>" is read from the file on every request, so a CLI
// auth token the upstream rotates never goes stale. Injected headers override
// the defaults set by the caller (Authorization/x-api-key), which is how a
// proxy routes harness traffic while sending the exact Authorization and
// User-Agent a native CLI sends. Malformed header config or an unreadable
// @file value is skipped rather than failing the request — the default auth
// still applies, and the operator's trace shows the resulting status.
func (p *Proxy) injectProviderHeaders(req *http.Request, prov store.Provider) {
	if strings.TrimSpace(prov.Headers) == "" {
		return
	}
	var hdr map[string]string
	if err := json.Unmarshal([]byte(prov.Headers), &hdr); err != nil {
		return
	}
	for name, v := range hdr {
		if strings.EqualFold(name, "content-type") {
			continue // never let injected headers break the JSON body framing
		}
		val := v
		if strings.HasPrefix(v, "@file:") {
			b, err := os.ReadFile(strings.TrimPrefix(v, "@file:"))
			if err != nil {
				continue
			}
			val = strings.TrimRight(string(b), "\r\n")
		}
		req.Header.Set(name, val)
	}
}

func (p *Proxy) send(ctx context.Context, prov store.Provider, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	// tolerate bases pasted in SDK convention that already end in the version
	// segment: ".../v1" for openai/anthropic, ".../api" for ollama
	switch prov.Type {
	case "ollama":
		if strings.HasSuffix(base, "/api") {
			path = strings.TrimPrefix(path, "/api")
		}
	case "commandcode":
		// base is the API root; tolerate a base pasted with the Pro-only
		// /provider/v1 segment (the models listing lives there, generation does
		// not). /alpha/generate is always the generation path.
		base = strings.TrimSuffix(base, "/provider/v1")
		path = "/alpha/generate"
	default:
		if endsWithVersion(base) {
			path = strings.TrimPrefix(path, "/v1")
		}
	}
	url := base + path
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch prov.Type {
	case "anthropic":
		if prov.APIKey != "" {
			req.Header.Set("x-api-key", prov.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	case "commandcode":
		// The gateway decides CLI vs API access from this identity: the CLI
		// fingerprint headers make it treat cfrproxy like the `cmd` CLI, which
		// is the only shape the Go plan permits. A fresh session id per
		// request; the operator can pin a version or session via Headers.
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
		req.Header.Set("x-cli-environment", "production")
		req.Header.Set("x-command-code-version", wire.CommandCodeVersion)
		req.Header.Set("x-session-id", newUUID())
	default:
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
	}
	p.injectProviderHeaders(req, prov)
	return p.Client.Do(req)
}

// newUUID returns a random UUIDv4 for the per-request x-session-id header.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// practically unreachable; a zero-ish id still satisfies the header
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UnixNano()>>32)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// rules returns enabled request- and response-phase rules that match this
// provider and inbound dialect.
func (p *Proxy) rules(providerID int64, inbound string) (reqRules, respRules []transform.Rule, err error) {
	all, err := p.Store.Transforms()
	if err != nil {
		return nil, nil, err
	}
	for _, t := range all {
		if !t.Enabled || (t.ProviderID != 0 && t.ProviderID != providerID) || (t.Target != "" && t.Target != inbound) {
			continue
		}
		rules, err := transform.Parse(t.Rules)
		if err != nil {
			return nil, nil, fmt.Errorf("transform %q: %w", t.Name, err)
		}
		if t.Phase == "request" {
			reqRules = append(reqRules, rules...)
		} else {
			respRules = append(respRules, rules...)
		}
	}
	return reqRules, respRules, nil
}

// NormalizeBase cleans up a user-entered base URL: trims whitespace and
// slashes, adds a scheme (http for private/local hosts, https otherwise), and
// strips accidentally pasted endpoint paths like /chat/completions.
func NormalizeBase(raw string) string {
	b := strings.TrimRight(strings.TrimSpace(raw), "/")
	if b == "" {
		return b
	}
	if !strings.Contains(b, "://") {
		host := b
		if i := strings.IndexAny(host, "/:"); i >= 0 {
			host = host[:i]
		}
		if host == "localhost" || !strings.Contains(host, ".") ||
			strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "10.") ||
			strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "172.16.") ||
			strings.HasPrefix(host, "100.") {
			b = "http://" + b
		} else {
			b = "https://" + b
		}
	}
	for _, suffix := range []string{"/chat/completions", "/messages", "/api/chat", "/api/generate"} {
		if strings.HasSuffix(b, suffix) {
			b = strings.TrimSuffix(b, suffix)
			// keep a trailing /v1 or /api — the suffix-aware join handles it
			break
		}
	}
	return strings.TrimRight(b, "/")
}

// DiscoverBase probes conventional base-URL variants with a 1-token request
// and returns the first base whose chat endpoint exists (any HTTP status
// except 404/405 counts — 400/401 still prove the path is right). Returns the
// original base and a warning note when nothing responds.
func (p *Proxy) DiscoverBase(ctx context.Context, prov store.Provider) (string, string) {
	base := NormalizeBase(prov.BaseURL)
	candidates := []string{base}
	if prov.Type == "openai" && !endsWithVersion(base) && !strings.HasSuffix(base, "/api") {
		candidates = append(candidates, base+"/api/v1") // openrouter-style domain-root paste
	}
	model := prov.DefaultModel
	if model == "" {
		model = "cfrproxy-probe"
	}
	body, err := buildOutbound(prov.Type, &wire.Request{Model: model,
		Messages: []wire.Msg{{Role: "user", Content: "hi"}}, MaxTokens: 1})
	if err != nil {
		return base, ""
	}
	for _, cand := range candidates {
		probe := prov
		probe.BaseURL = cand
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		resp, err := p.send(cctx, probe, providerPath(prov.Type), body)
		if err != nil {
			cancel()
			continue
		}
		code := resp.StatusCode
		resp.Body.Close()
		cancel()
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			note := fmt.Sprintf("endpoint verified (HTTP %d)", code)
			if cand != prov.BaseURL {
				note = fmt.Sprintf("base URL resolved to %s (HTTP %d)", cand, code)
			}
			return cand, note
		}
	}
	return base, "warning: no conventional endpoint variant responded; saved as entered"
}

// TestProvider sends a small prompt directly to one provider (TUI/WebUI test button).
func (p *Proxy) TestProvider(ctx context.Context, prov store.Provider, prompt string) (*wire.Response, error) {
	model := prov.DefaultModel
	if model == "" {
		return nil, fmt.Errorf("provider %s has no default_model set", prov.Name)
	}
	req := &wire.Request{Model: model, Messages: []wire.Msg{{Role: "user", Content: prompt}}, MaxTokens: 256}
	body, err := buildOutbound(prov.Type, req)
	if err != nil {
		return nil, err
	}
	resp, err := p.send(ctx, prov, providerPath(prov.Type), body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snip(rb))
	}
	return parseOutboundResponse(prov.Type, rb)
}

// ---- dialect dispatch ----

func parseInbound(dialect string, body []byte) (*wire.Request, error) {
	switch dialect {
	case "anthropic":
		return wire.ParseAnthropicRequest(body)
	case "ollama":
		return wire.ParseOllamaRequest(body)
	case "responses":
		return wire.ParseResponsesRequest(body)
	default:
		return wire.ParseOpenAIRequest(body)
	}
}

func buildOutbound(ptype string, req *wire.Request) ([]byte, error) {
	switch ptype {
	case "anthropic":
		return wire.BuildAnthropicRequest(req)
	case "ollama":
		return wire.BuildOllamaRequest(req)
	case "responses":
		return wire.BuildResponsesRequest(req)
	case "commandcode":
		return wire.BuildCommandCodeRequest(req)
	default:
		return wire.BuildOpenAIRequest(req)
	}
}

func parseOutboundResponse(ptype string, body []byte) (*wire.Response, error) {
	switch ptype {
	case "anthropic":
		return wire.ParseAnthropicResponse(body)
	case "ollama":
		return wire.ParseOllamaResponse(body)
	case "responses":
		return wire.ParseResponsesResponse(body)
	case "commandcode":
		return wire.ParseCommandCodeResponse(body)
	default:
		return wire.ParseOpenAIResponse(body)
	}
}

func buildInboundResponse(dialect string, r *wire.Response) []byte {
	switch dialect {
	case "anthropic":
		return wire.BuildAnthropicResponse(r)
	case "ollama":
		return wire.BuildOllamaResponse(r)
	case "responses":
		return wire.BuildResponsesResponse(r)
	default:
		return wire.BuildOpenAIResponse(r)
	}
}

func readStream(ptype string, body io.Reader, out chan<- wire.Delta) {
	switch ptype {
	case "anthropic":
		wire.ReadAnthropicStream(body, out)
	case "ollama":
		wire.ReadOllamaStream(body, out)
	case "responses":
		wire.ReadResponsesStream(body, out)
	case "commandcode":
		wire.ReadCommandCodeStream(body, out)
	default:
		wire.ReadOpenAIStream(body, out)
	}
}

func writeStream(dialect string, w http.ResponseWriter, model string, in <-chan wire.Delta) error {
	switch dialect {
	case "anthropic":
		return wire.WriteAnthropicStream(w, model, in)
	case "ollama":
		return wire.WriteOllamaStream(w, model, in)
	case "responses":
		return wire.WriteResponsesStream(w, model, in)
	default:
		return wire.WriteOpenAIStream(w, model, in)
	}
}

func providerPath(ptype string) string {
	switch ptype {
	case "anthropic":
		return "/v1/messages"
	case "ollama":
		return "/api/chat"
	case "responses":
		return "/v1/responses"
	case "commandcode":
		return "/alpha/generate"
	default:
		return "/v1/chat/completions"
	}
}

// DefaultResponsesModels are the model-id globs served via the OpenAI Responses
// API (/v1/responses) instead of chat completions when the upstream is an
// `openai`-type provider. Override with the "responses_models" setting (comma
// globs); set it to "-" to disable Responses routing entirely.
var DefaultResponsesModels = []string{"gpt-5*", "o1", "o1-*", "o3", "o3-*", "o4", "o4-*", "codex*"}

// responsesCapable reports whether a model id should be routed to the Responses
// API. Matches the bare upstream id (provider scope stripped) against the
// configured/default glob list.
func (p *Proxy) responsesCapable(model string) bool {
	raw := strings.TrimSpace(p.Store.Setting("responses_models"))
	if raw == "-" {
		return false // explicit kill-switch
	}
	pats := DefaultResponsesModels
	if raw != "" {
		pats = splitList(raw)
	}
	m := model
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	lm := strings.ToLower(m)
	for _, pat := range pats {
		if matchGlob(strings.ToLower(strings.TrimSpace(pat)), lm) {
			return true
		}
	}
	return false
}

// otype is the effective OUTBOUND wire dialect for a provider+model: an
// `openai` provider serving a Responses-capable model talks to the upstream via
// /v1/responses; everything else uses the provider's own declared type.
func (p *Proxy) otype(prov store.Provider, model string) string {
	if prov.Type == "openai" && p.responsesCapable(model) {
		return "responses"
	}
	return prov.Type
}

// ---- helpers ----

func snip(b []byte) string {
	s := string(redactImagePayloads(b))
	if len(s) > snippetMax {
		s = s[:snippetMax] + "…"
	}
	return s
}

// dataURLImageRe matches the base64 payload of an inline image data URL.
var dataURLImageRe = regexp.MustCompile(`data:image/[A-Za-z0-9.+-]+;base64,[A-Za-z0-9+/=_-]*`)

// redactImagePayloads strips inline image bytes out of anything bound for a
// trace snippet or a log line. A vision request is mostly base64: left alone
// it would evict the actual conversation from the snippet window, bloat the
// trace table, and park a copy of the user's screenshot — a construction plan,
// a whiteboard, whatever it is — in a database that exists for debugging. The
// marker keeps the trace honest about the image having been there.
func redactImagePayloads(b []byte) []byte {
	if !bytes.Contains(b, []byte(";base64,")) {
		return b // fast path: no inline image anywhere in the body
	}
	return dataURLImageRe.ReplaceAllFunc(b, func(m []byte) []byte {
		i := bytes.Index(m, []byte(";base64,"))
		head := m[:i+len(";base64,")]
		n := len(m) - len(head)
		return append(append([]byte{}, head...), []byte(fmt.Sprintf("[%d base64 chars redacted]", n))...)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, dialect string, code int, msg string) {
	switch dialect {
	case "anthropic":
		writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}})
	case "ollama":
		writeJSON(w, code, map[string]any{"error": msg})
	default:
		writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "type": "api_error"}})
	}
}

// providerTransport is the connection pool for outbound provider calls.
//
// A bare &http.Client{} uses http.DefaultTransport, whose MaxIdleConnsPerHost
// is Go's DefaultMaxIdleConnsPerHost = 2. That default is sized for a program
// talking to a handful of hosts occasionally, not for a proxy fanning many
// concurrent requests at a few upstreams — and here it bites hardest because
// EVERY OAuth-backed provider (claude, codex, gemini, grok, command, opencode)
// shares one host, so the whole subscription fleet contends for two pooled
// connections. Past that, connections are opened and thrown away per request:
// a fresh TCP + TLS handshake each time against remote providers, and socket
// churn against local ones.
//
// This does not change how many requests can be in flight — Go opens as many
// connections as it needs either way — it changes how many are *reused*.
// upstreamTimeout is the ceiling on a whole provider call, read once at
// startup from the "upstream_timeout_minutes" setting. 0 (the default) means
// no ceiling.
//
// http.Client.Timeout covers connect, headers AND the entire body read, so on
// a streaming generation it is a guillotine on the answer itself. The old
// fixed 10 minutes cut two legitimate cases: an agent turn that simply
// generates for longer than that, and a request waiting its turn behind busy
// slots — llama.cpp sends no response headers until a slot frees, so a deep
// queue looks exactly like a hung upstream ("context deadline exceeded while
// awaiting headers") right up until it would have succeeded.
//
// Nothing is left unguarded by dropping it: every provider call is made with
// the inbound request's context (see send), so a client that gives up or
// disconnects cancels the upstream call immediately, and the dial/TLS
// timeouts below still fail fast on a host that is genuinely unreachable.
// An operator who wants a hard ceiling back can set the minutes.
func upstreamTimeout(s *store.Store) time.Duration {
	if s == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s.Setting("upstream_timeout_minutes")))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Minute
}

func providerTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64 // was effectively 2
	t.IdleConnTimeout = 120 * time.Second
	// MaxConnsPerHost stays 0 (unlimited): a cap here would queue requests
	// behind each other, which is the exact failure this is meant to avoid.
	t.MaxConnsPerHost = 0
	// With no overall client deadline, these are what still catch an upstream
	// that is down rather than slow: they bound getting *connected*, never
	// how long a generation may run once it is.
	t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	// ResponseHeaderTimeout is deliberately left unset: a request queued
	// behind busy slots legitimately waits minutes for its first byte, and
	// any fixed value here would kill exactly the deep-queue case above.
	return t
}

// recordAttemptFailure writes a trace for a candidate that did NOT serve the
// request, filed under that candidate's own provider and model.
//
// Without it a failed attempt left no row anywhere: the reason survived only
// inside the *successful* provider's error text, so a provider failing every
// single request showed an empty, healthy-looking panel. Making the failure
// visible under the provider that actually failed is the whole point.
//
// Status 0 means the provider never returned an HTTP response at all
// (connection refused, DNS, timeout).
func (p *Proxy) recordAttemptFailure(main *store.Trace, c candidate, reason string, status int) {
	if reason == "" {
		return
	}
	t := store.Trace{
		TS:       time.Now().UnixMilli(),
		Provider: c.prov.Name,
		Model:    c.model,
		Inbound:  main.Inbound,
		Stream:   main.Stream,
		Status:   status,
		Err:      reason,
		Note:     "attempt failed — request continued down the fallback chain",
		ReqSnip:  main.ReqSnip,
	}
	p.Store.AddTrace(&t)
	p.Hub.Publish(t)
	logTrace(nil, &t)
}

// recordRejection writes a trace for a request refused before it reached a
// provider (auth, unknown mount). Without it these failures are invisible: the
// client sees an HTTP error, Live Traces shows nothing at all, and the natural
// conclusion is that the proxy is dropping calls silently.
//
// clientHint records where it came from, because the common cause is one
// machine out of several being misconfigured.
func (p *Proxy) recordRejection(r *http.Request, inbound string, status int, reason string) {
	t := store.Trace{
		TS:      time.Now().UnixMilli(),
		Model:   r.URL.Path,
		Inbound: inbound,
		Status:  status,
		Err:     reason,
		Note:    "rejected before routing — from " + clientHint(r),
	}
	p.Store.AddTrace(&t)
	p.Hub.Publish(t)
	logTrace(r, &t)
}

// clientHint identifies the caller for a rejection trace: the forwarded client
// address when behind a reverse proxy, else the direct peer.
func clientHint(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v) + " (via proxy)"
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v) + " (via proxy)"
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}
