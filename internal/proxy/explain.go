package proxy

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// Explain is a dry run of the routing decisions handleCore would make for a
// model id, without sending anything upstream. It exists because the
// questions operators actually ask — "why did this land on the wrong model",
// "why is this 403 on my share endpoint", "what would it fall back to" — were
// only answerable by reading proxy.go: model_map, scoped mounts, share-endpoint
// policy, pools, per-provider and global fallback chains all rewrite the
// answer, and none of them say so in the response.
//
// Served as `cfrproxy explain <model>` and GET /admin/api/explain.
type ExplainRequest struct {
	Model    string `json:"model"`
	Endpoint string `json:"endpoint,omitempty"` // share endpoint name (the /e/{name} mount)
	Scope    string `json:"scope,omitempty"`    // provider name of a /p/{provider} mount
	Inbound  string `json:"inbound,omitempty"`  // openai | anthropic | ollama | responses
	Image    bool   `json:"image,omitempty"`    // the request carries an image
	// Smart-router inputs (model "auto" with smart routing on): the request
	// shape to dry-run, and an optional tier to bypass the classifier.
	Tokens int    `json:"tokens,omitempty"` // estimated prompt tokens
	Tools  int    `json:"tools,omitempty"`  // tools attached
	Depth  int    `json:"depth,omitempty"`  // messages so far
	Tier   string `json:"tier,omitempty"`   // routine | careful | hard
	Text   string `json:"text,omitempty"`   // last user message, for the classifier/heuristic
}

type ExplainStep struct {
	Stage  string `json:"stage"`
	Detail string `json:"detail"`
}

type ExplainCandidate struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Why      string `json:"why"`
	Facts    string `json:"facts,omitempty"` // smart router registry row
}

type ExplainResult struct {
	Requested  string             `json:"requested"`
	Resolved   string             `json:"resolved,omitempty"` // provider/model that would be tried first
	Status     int                `json:"status"`             // 200, or the HTTP status the request would be refused with
	Error      string             `json:"error,omitempty"`
	Steps      []ExplainStep      `json:"steps"`
	Candidates []ExplainCandidate `json:"candidates,omitempty"`
}

func (r *ExplainResult) step(stage, format string, args ...any) {
	r.Steps = append(r.Steps, ExplainStep{Stage: stage, Detail: fmt.Sprintf(format, args...)})
}

// Text renders the result the way `cfrproxy explain` prints it.
func (r ExplainResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "requested: %s\n", r.Requested)
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "  %-10s %s\n", s.Stage+":", s.Detail)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "result: HTTP %d — %s\n", r.Status, r.Error)
		return b.String()
	}
	fmt.Fprintf(&b, "result: %s\n", r.Resolved)
	if len(r.Candidates) > 1 {
		b.WriteString("chain:\n")
		for i, c := range r.Candidates {
			if c.Facts != "" {
				fmt.Fprintf(&b, "  %d. %-40s %-32s %s\n", i+1, c.Provider+"/"+c.Model, c.Why, c.Facts)
				continue
			}
			fmt.Fprintf(&b, "  %d. %s/%s  (%s)\n", i+1, c.Provider, c.Model, c.Why)
		}
	}
	return b.String()
}

func (p *Proxy) Explain(ctx context.Context, q ExplainRequest) ExplainResult {
	res := ExplainResult{Requested: q.Model, Status: 200}
	inbound := q.Inbound
	if inbound == "" {
		inbound = "openai"
	}
	reqModel := strings.TrimSpace(q.Model)

	var ep *store.Endpoint
	if q.Endpoint != "" {
		e, ok := p.Store.EndpointByName(q.Endpoint)
		if !ok || !e.Enabled {
			res.Status, res.Error = 404, fmt.Sprintf("no such share endpoint %q (or it is disabled)", q.Endpoint)
			return res
		}
		ep = &e
		policy := "any model"
		if e.ForceModel != "" {
			policy = "force_model=" + e.ForceModel
		} else if e.Models != "" {
			policy = "allow-list " + e.Models
		}
		extra := ""
		if e.NoFallback {
			extra += ", fallback disabled"
		}
		if e.Caveman {
			extra += ", caveman on"
		}
		if e.ContextLength > 0 {
			extra += fmt.Sprintf(", context capped at %d", e.ContextLength)
		}
		if e.ReasoningEffort != "" {
			extra += ", thinking " + e.ReasoningEffort
			if e.ReasoningForce {
				extra += " (forced)"
			}
		}
		res.step("endpoint", "/e/%s: %s%s", e.Name, policy, extra)
	}
	if q.Scope != "" {
		before := reqModel
		reqModel = q.Scope + "/" + p.stripProviderPrefix(reqModel)
		res.step("scope", "/p/%s mount qualifies %q → %q", q.Scope, before, reqModel)
	}
	if ep != nil {
		if ep.ForceModel != "" {
			res.step("policy", "force_model overrides the request → %s", ep.ForceModel)
			reqModel = ep.ForceModel
		} else if !p.modelAllowed(*ep, reqModel) {
			res.Status = 403
			res.Error = fmt.Sprintf("model not permitted on this endpoint: %s (allow-list: %s)", reqModel, ep.Models)
			res.step("policy", "%q matches none of the allow-list patterns [%s]", reqModel, ep.Models)
			return res
		} else if ep.Models != "" {
			res.step("policy", "%q is on the allow-list", reqModel)
		}
	}
	if mapped := p.Store.ModelMapLookup(reqModel, MatchMapPattern); mapped != "" {
		res.step("model_map", "%q → %q", reqModel, mapped)
		reqModel = mapped
	}
	switch base := strings.SplitN(reqModel, ":", 2)[0]; base {
	case "auto", "auto-plan":
		cfg := p.AutoRouterConfig()
		if cfg.Enabled && cfg.Smart.on() {
			return p.explainSmart(ctx, res, q, cfg)
		}
		res.step("router", "%s: classifier %s decides the bucket per request; enabled=%v", base, cfg.Classifier, cfg.Enabled)
		keys := make([]string, 0, len(cfg.Routes))
		for k := range cfg.Routes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.step("router", "bucket %-9s → %v", k, cfg.Routes[k])
		}
		res.Resolved = reqModel
		res.step("note", "a virtual router resolves per request; explain one of its targets to see that chain")
		return res
	case "fusion":
		res.Resolved = reqModel
		res.step("fusion", "participants draft in parallel and a judge synthesizes; see /admin/api/fusions")
		return res
	}

	prov, model, err := p.ResolveModel(ctx, reqModel)
	if err != nil {
		res.Status, res.Error = 400, err.Error()
		res.step("resolve", "%v", err)
		return res
	}
	how := "provider/model prefix"
	if !strings.Contains(reqModel, "/") {
		how = "alias or fuzzy match across enabled providers"
	}
	res.step("resolve", "%q → %s/%s (%s; provider type %s, base %s)", reqModel, prov.Name, model, how, prov.Type, prov.BaseURL)
	res.Resolved = prov.Name + "/" + model

	noFallback := prov.NoFallback || (ep != nil && ep.NoFallback)
	cands := []candidate{{prov: prov, model: model}}
	if spec := p.poolSpecFor(model); spec != nil {
		mode := "least-busy"
		if spec.Affinity {
			mode = "prefix affinity, then load"
		}
		res.step("pool", "%q is a pool of %v (%s; sibling failover=%v)", model, spec.Members, mode, spec.Failover)
		if spec.Failover {
			for _, m := range spec.Members {
				if m != model {
					cands = append(cands, candidate{prov: prov, model: m, sibling: true})
				}
			}
		}
	}
	providerChain := p.ProviderFallbackConfig().Enabled
	if prov.Fallback != "" {
		if !providerChain {
			res.step("fallback", "provider %s names fallback %q but provider_fallback is off (default) — not walked", prov.Name, prov.Fallback)
		} else if noFallback {
			res.step("fallback", "provider %s names fallback %q but fallback is disabled here — not walked", prov.Name, prov.Fallback)
		}
	}
	seen := map[int64]bool{prov.ID: true}
	cur := prov
	for hop := 0; providerChain && !noFallback && hop < 3 && cur.Fallback != ""; hop++ {
		fprov, fmodel, ferr := p.ResolveModel(ctx, cur.Fallback)
		if ferr != nil || seen[fprov.ID] {
			break
		}
		seen[fprov.ID] = true
		cands = append(cands, candidate{prov: fprov, model: fmodel, failover: true})
		cur = fprov
	}
	providerHops := len(cands)
	if noFallback {
		res.step("fallback", "disabled for this request — the caller gets %s's real error instead of a reroute", prov.Name)
	} else {
		if q.Image {
			before := len(cands)
			cands = p.appendVisionFallback(ctx, cands, ep, true)
			if !p.visionCapableFor(ctx, prov, model) {
				res.step("vision", "%s/%s is not known to be vision-capable; the vision chain goes FIRST", prov.Name, model)
			}
			res.step("vision", "%d vision-chain candidate(s) appended", len(cands)-before)
		}
		gf := p.GlobalFallbackConfig()
		before := len(cands)
		cands = p.appendGlobalFallback(ctx, cands, ep)
		res.step("global", "global chain enabled=%v targets=%v → %d candidate(s) appended", gf.Enabled, gf.Targets, len(cands)-before)
	}
	for i, c := range cands {
		why := "primary"
		switch {
		case i == 0:
		case c.sibling:
			why = "pool sibling"
		case i < providerHops:
			why = "provider fallback"
		case c.vision:
			why = "vision chain"
		default:
			why = "global chain"
		}
		res.Candidates = append(res.Candidates, ExplainCandidate{Provider: c.prov.Name, Model: c.model, Why: why})
	}
	if reqRules, respRules, err := p.rules(prov.ID, inbound); err != nil {
		res.step("transforms", "error: %v", err)
	} else if len(reqRules)+len(respRules) > 0 {
		res.step("transforms", "%d request and %d response rule(s) apply for %s/%s", len(reqRules), len(respRules), prov.Name, inbound)
	}
	if prov.Caveman || (ep != nil && ep.Caveman) {
		res.step("caveman", "tool results older than the newest few are compressed for this route")
	}
	if prov.ContextLength > 0 {
		res.step("context", "provider advertises %d tokens", prov.ContextLength)
	}
	if lvl, force := reasoningFor(ep, prov); lvl != "" {
		how := "when the client sends none"
		if force {
			how = "overriding whatever the client sends"
		}
		src := "provider " + prov.Name
		if ep != nil && ep.ReasoningEffort != "" {
			src = "share /e/" + ep.Name
		}
		res.step("thinking", "level %s from %s, %s", lvl, src, how)
	}
	return res
}

// explainSmart dry-runs the smart selector for the request shape in q. The
// classifier is only called when the caller gave text and no tier; with
// neither, the heuristic grades an empty request as routine.
func (p *Proxy) explainSmart(ctx context.Context, res ExplainResult, q ExplainRequest, cfg AutoRouterConfig) ExplainResult {
	pr := RouteProfile{Tokens: q.Tokens, Tools: q.Tools, Depth: q.Depth, Image: q.Image}
	req := &wire.Request{}
	if q.Text != "" {
		req.Messages = []wire.Msg{{Role: "user", Content: q.Text}}
	}
	switch {
	case parseTier(q.Tier) != "" && strings.EqualFold(q.Tier, parseTier(q.Tier)):
		pr.Tier, pr.Source = parseTier(q.Tier), "override"
	case q.Text != "":
		pr = p.classifyTier(ctx, req, cfg, pr)
	default:
		pr.Tier, pr.Source = heuristicTier(req, pr), "heuristic"
	}
	res.step("router", "smart: classifier %q grades the tier; sticky=%v local_max_tokens=%d", cfg.Classifier, cfg.sticky(), cfg.Smart.localMaxTokens())
	res.step("profile", "%s", pr)
	d := p.smartSelect(ctx, cfg.Smart, pr, "")
	res.step("tier", "walking %q: %s", d.Tier, strings.Join(cfg.Tiers(d.Tier), ", "))
	for _, c := range d.Candidates {
		if c.Provider == "" {
			res.Candidates = append(res.Candidates, ExplainCandidate{Provider: "?", Model: c.Entry, Why: c.Verdict, Facts: "-"})
			continue
		}
		res.Candidates = append(res.Candidates, ExplainCandidate{Provider: c.Provider, Model: c.Model, Why: c.Verdict, Facts: c.Facts()})
	}
	if d.Chosen == "" {
		def := cfg.Routes["default"]
		if def == "" {
			res.Status, res.Error = 503, "no candidate qualified and routes.default is unset"
			return res
		}
		res.step("result", "no candidate qualified → routes.default %s", def)
		res.Resolved = def
		return res
	}
	res.Resolved = d.Chosen
	return res
}

// Tiers returns the list a tier name walks (vision list for "vision").
func (c AutoRouterConfig) Tiers(tier string) []string {
	if c.Smart == nil {
		return nil
	}
	if tier == "vision" {
		return c.Smart.Vision
	}
	return c.Smart.Tiers[tier]
}
