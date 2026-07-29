// Package proxy implements the data plane: inbound dialect endpoints
// (/v1/chat/completions, /v1/messages, /api/chat), provider resolution,
// declarative transforms, and stream re-framing between dialects.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/transform"
	"github.com/crogers2287/cfrproxy/internal/wire"
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
	summaries summaryCache
}

func New(s *store.Store) *Proxy {
	return &Proxy{Store: s, Hub: NewHub(), Client: &http.Client{Timeout: 10 * time.Minute, Transport: providerTransport()},
		models: modelCache{entries: map[int64]modelCacheEntry{}}}
}

func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "openai", "") })
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "anthropic", "") })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "ollama", "") })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleModels(w, r, "") })
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleTags(w, r, "") })

	// Per-provider virtual mounts: /p/{provider}/... scopes every call to one
	// provider and lists only its models (bare ids). This lets a harness treat
	// each cfrproxy provider as its own OpenAI endpoint — the basis for the
	// router→provider→model drill-down in pickers.
	mux.HandleFunc("POST /p/{provider}/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "openai", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "anthropic", r.PathValue("provider")) })
	mux.HandleFunc("POST /p/{provider}/api/chat", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "ollama", r.PathValue("provider")) })
	mux.HandleFunc("GET /p/{provider}/v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleModels(w, r, r.PathValue("provider")) })
	mux.HandleFunc("GET /p/{provider}/api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleTags(w, r, r.PathValue("provider")) })

	// Share endpoints: /e/{name}/... is a scoped, key-authed view exposing only
	// a curated model set (or forcing every request to one model / the auto
	// router). Share the URL + the endpoint's own API key with someone else.
	mux.HandleFunc("POST /e/{endpoint}/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "openai") })
	mux.HandleFunc("POST /e/{endpoint}/v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "anthropic") })
	mux.HandleFunc("POST /e/{endpoint}/api/chat", func(w http.ResponseWriter, r *http.Request) { p.handleEndpoint(w, r, "ollama") })
	mux.HandleFunc("GET /e/{endpoint}/v1/models", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointModels(w, r) })
	mux.HandleFunc("GET /e/{endpoint}/api/tags", func(w http.ResponseWriter, r *http.Request) { p.handleEndpointModels(w, r) })

	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": "0.7.0-cfrproxy"})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
}

// scopedModelIDs returns bare model ids for one provider. When the provider
// has a pinned (curated) list and all==false, only pins are returned — this
// is what keeps harness pickers short. all==true returns the live catalog.
func (p *Proxy) scopedModelIDs(ctx context.Context, provider string, all bool) []string {
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
	var ids []string
	if scope != "" {
		ids = p.scopedModelIDs(r.Context(), scope, r.URL.Query().Get("all") != "")
	} else {
		ids = p.AllModelIDs(r.Context())
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "cfrproxy"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) handleTags(w http.ResponseWriter, r *http.Request, scope string) {
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
	if r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("X-Real-IP") == "" {
		return true // direct connection — not via the public proxy
	}
	keys := splitList(p.Store.Setting("public_api_keys"))
	if len(keys) == 0 {
		return true // gate not configured
	}
	got := r.Header.Get("x-api-key")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	for _, k := range keys {
		if got == k {
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
	if scope != "" {
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
		httpErr(w, inbound, 401, "public access requires a valid API key (Authorization: Bearer <key> or x-api-key)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		httpErr(w, inbound, 400, "read body: "+err.Error())
		return
	}
	req, err := parseInbound(inbound, body)
	if err != nil {
		httpErr(w, inbound, 400, err.Error())
		return
	}
	// scoped mount forces the provider; a bare model id is qualified to it, and
	// a "provider/model" that names a different provider is corrected.
	reqModel := req.Model
	if scope != "" {
		m := reqModel
		if i := strings.IndexByte(m, '/'); i > 0 {
			m = m[i+1:]
		}
		reqModel = scope + "/" + m
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
	}
	autoNote := ""
	if cnote := p.Compress(r.Context(), req); cnote != "" {
		autoNote = cnote + " "
	}
	// fusion: fan out to participants, then this request becomes the judge's
	// synthesis call (which then routes/streams normally below).
	if reqModel == "fusion" || strings.HasSuffix(reqModel, "/fusion") {
		if judge, note, ok := p.Fusion(r.Context(), req); ok {
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
		}
	}
	prov, model, err := p.ResolveModel(r.Context(), reqModel)
	if err != nil {
		httpErr(w, inbound, 503, err.Error())
		return
	}
	req.Model = model

	tr := &store.Trace{TS: start.UnixMilli(), Provider: prov.Name, Model: model, Inbound: inbound,
		Stream: req.Stream, ReqSnip: snip(body), Note: autoNote}
	defer func() {
		tr.LatencyMS = time.Since(start).Milliseconds()
		p.Store.AddTrace(tr)
		p.Hub.Publish(*tr)
	}()

	// candidate chain: primary, then its fallback chain followed transitively
	// (cycle-safe, max 3 hops). Transient failures retry once per candidate,
	// then move down the chain.
	cands := []candidate{{prov: prov, model: model}}
	seen := map[int64]bool{prov.ID: true}
	cur := prov
	for hop := 0; hop < 3 && cur.Fallback != ""; hop++ {
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
	cands = p.appendGlobalFallback(r.Context(), cands, ep)

	var resp *http.Response
	var used candidate
	var respRules []transform.Rule
	var passth bool
	lastErr := ""
	var softErrs []string // transient errors that were retried/failed-over past
	for _, c := range cands {
		candStatus := 0 // HTTP status of this candidate's failure, 0 = never responded
		creq := *req
		creq.Model = c.model
		if c.prov.InjectDocs && c.prov.DocMarkdown != "" {
			if creq.System != "" {
				creq.System = c.prov.DocMarkdown + "\n\n" + creq.System
			} else {
				creq.System = c.prov.DocMarkdown
			}
		}
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
		passthrough := !c.failover && autoNote == "" && inbound == c.prov.Type && len(reqRules) == 0 && len(respR) == 0 && !c.prov.InjectDocs
		var outBody []byte
		if passthrough {
			outBody = rawWithModel(body, c.model)
		} else {
			outBody, err = buildOutbound(c.prov.Type, &creq)
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
		for attempt := 0; attempt < maxAttempts && resp == nil; attempt++ {
			if attempt > 0 && !paramRetry {
				time.Sleep(1200 * time.Millisecond)
			}
			paramRetry = false
			r2, err := p.send(r.Context(), c.prov, providerPath(c.prov.Type), outBody)
			if err != nil {
				lastErr = c.prov.Name + ": " + err.Error()
				if r.Context().Err() != nil {
					return // client gone
				}
				continue
			}
			if transientStatus(r2.StatusCode) {
				eb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
				r2.Body.Close()
				lastErr = fmt.Sprintf("%s: HTTP %d %s", c.prov.Name, r2.StatusCode, snip(eb))
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
					candStatus = r2.StatusCode
					softErrs = append(softErrs, lastErr)
					break // stop retrying this provider; try the next candidate
				}
				// context overflow: a larger-window model down the chain can
				// still serve this, so don't hard-fail the harness either
				if contextExceeded(eb) {
					lastErr = fmt.Sprintf("%s: context overflow (HTTP %d) %s", c.prov.Name, r2.StatusCode, snip(eb))
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
		tr.Status, tr.Err = 502, lastErr
		httpErr(w, inbound, 502, lastErr)
		return
	}
	defer resp.Body.Close()
	tr.Provider, tr.Model = used.prov.Name, used.model
	alert := ""
	if used.failover {
		tr.Err = "failover from " + prov.Name + " (" + lastErr + ")"
		// Deliberately terse, and rate-limited per "from -> to" pair rather than
		// per conversation: harnesses inject the current time and other volatile
		// text into the system prompt, so any content-derived key looks new on
		// every call and suppresses nothing. The full reason stays on tr.Err —
		// visible in the trace and the WebUI errors panel — where it belongs.
		if noticeCache.announce(prov.Name + "->" + used.prov.Name + "/" + used.model) {
			alert = fmt.Sprintf("⚠️ failover: %s %s → %s active\n\n",
				prov.Name, failureLabel(lastErr), used.model)
		}
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

	if passth {
		p.copyRaw(w, resp, req.Stream, used.prov.Type, tr)
		return
	}

	if req.Stream {
		deltas := make(chan wire.Delta, 16)
		go readStream(used.prov.Type, resp.Body, deltas)
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
	norm, err := parseOutboundResponse(used.prov.Type, rb)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, inbound, 502, err.Error())
		return
	}
	norm.Model = req.Model
	tr.PromptTokens, tr.CompletionTokens, tr.CachedTokens = norm.PromptTokens, norm.CompletionTokens, norm.CachedTokens
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

func (p *Proxy) send(ctx context.Context, prov store.Provider, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	// tolerate bases pasted in SDK convention that already end in the version
	// segment: ".../v1" for openai/anthropic, ".../api" for ollama
	switch prov.Type {
	case "ollama":
		if strings.HasSuffix(base, "/api") {
			path = strings.TrimPrefix(path, "/api")
		}
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
	default:
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
	}
	return p.Client.Do(req)
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
	default:
		return "/v1/chat/completions"
	}
}

// ---- helpers ----

func snip(b []byte) string {
	s := string(b)
	if len(s) > snippetMax {
		s = s[:snippetMax] + "…"
	}
	return s
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
func providerTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64 // was effectively 2
	t.IdleConnTimeout = 120 * time.Second
	// MaxConnsPerHost stays 0 (unlimited): a cap here would queue requests
	// behind each other, which is the exact failure this is meant to avoid.
	t.MaxConnsPerHost = 0
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
}
