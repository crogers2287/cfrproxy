package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// maxVisionHops caps how many vision-capable targets one request may try.
// Image requests carry a large body, so each failed hop is expensive.
const maxVisionHops = 3

// VisionFallback is the image-specific failover chain.
//
// Where GlobalFallback covers "this provider can't serve at all" (quota, rate
// limit, unreachable), this covers the narrower case of "this provider can't
// serve THIS IMAGE": a text-only model addressed with a picture, a multimodal
// endpoint whose file round-trip timed out, or an image the provider rejects
// as too large or malformed.
//
// It only ever applies to requests that actually carry an image, so a text
// conversation never pays for it.
type VisionFallback struct {
	Enabled bool     `json:"enabled"`
	Targets []string `json:"targets"` // ordered "provider/model" refs, vision-capable
}

// DefaultVisionModels are the model-id globs known to ACCEPT image input.
//
// Matched as `*token*` substrings rather than vendor prefixes, because
// providers rename models: this deployment's OpenCode mount serves
// `claude-opencode-sonnet-5` (really Sonnet, sees images) right next to
// `claude-opencode-go-deepseek-v4-flash` (text only). A bare `claude-*` prefix
// would call both of them vision-capable and reintroduce the exact silent
// hallucination this gate exists to stop.
//
// Unknown models are treated as NON-vision, and that asymmetry is deliberate.
// Misjudging a vision model costs one hop to a vision target that answers the
// question correctly anyway. Misjudging a text-only model produces a fluent,
// confident description of a picture the model never received — the failure
// mode that is impossible to detect downstream and worse than an error.
//
// Override with the "vision_models" setting (comma globs). Set it to "-" to
// disable proactive routing and go back to pure on-error failover.
var DefaultVisionModels = []string{
	// OpenAI multimodal
	"*gpt-4o*", "*gpt-4.1*", "*gpt-4-turbo*", "*gpt-5*",
	"o1", "o1-*", "o3", "o3-*", "o4", "o4-*",
	// Anthropic — Claude 3 and later are multimodal across the line
	"*claude-3*", "*opus*", "*sonnet*", "*haiku*",
	// Google
	"*gemini*", "*gemma3*", "*gemma-3*",
	// xAI
	"*grok-2-vision*", "*grok-3-vision*", "*grok-4*", "*grok-5*",
	// open multimodal families (local llama.cpp/ollama builds land here)
	"*-vl", "*-vl-*", "*vision*", "*llava*", "*moondream*",
	"*minicpm-v*", "*internvl*", "*pixtral*", "*deepseek-vl*",
}

// visionMetaCache holds per-model vision capability as DECLARED by a provider,
// which is authoritative where the glob list is only a guess. llama-swap
// publishes `meta.llamaswap.isVision` on /v1/models; a local build called
// something like "qwythos-9b" matches no naming convention on earth, but its
// own server knows perfectly well that it has a vision projector attached.
type visionMetaCache struct {
	mu      sync.Mutex
	entries map[string]bool // "provider/model" (lowercased) → sees images
}

func visionMetaKey(prov, model string) string {
	return strings.ToLower(prov + "/" + model)
}

func (p *Proxy) recordVisionMeta(prov, model string, sees bool) {
	p.vision.mu.Lock()
	p.vision.entries[visionMetaKey(prov, model)] = sees
	p.vision.mu.Unlock()
}

func (p *Proxy) lookupVisionMeta(prov, model string) (sees, known bool) {
	p.vision.mu.Lock()
	defer p.vision.mu.Unlock()
	v, ok := p.vision.entries[visionMetaKey(prov, model)]
	return v, ok
}

// truthy normalises the JSON value of a capability flag, which providers spell
// inconsistently — llama-swap sends a real bool for isVision but a string for
// context, so assuming either type alone loses data.
func truthy(v any) (val, ok bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	case float64:
		return t != 0, true
	}
	return false, false
}

// visionCapableFor answers "can THIS provider's copy of this model read an
// image", preferring the provider's own declaration over the name heuristic.
//
// Order matters, and it is capability-first rather than cheapest-first: a
// declaration OUTRANKS the name. A model called "…-vl" that its own server
// reports as isVision:false really cannot see, and trusting the name there
// would hand it a picture — the exact failure this gate exists to prevent.
// Names are consulted only where the provider says nothing, and the network
// scan happens last so the common paths stay free.
func (p *Proxy) visionCapableFor(ctx context.Context, prov store.Provider, model string) bool {
	if strings.TrimSpace(p.Store.Setting("vision_models")) == "-" {
		return true // kill-switch: proactive routing disabled
	}
	if sees, known := p.lookupVisionMeta(prov.Name, model); known {
		return sees
	}
	if p.visionCapable(model) {
		return true
	}
	// The name told us nothing. One scan (60s-cached) populates capability for
	// every model this provider serves, so a locally-named vision model like
	// "qwythos-9b" is not routed away from on the very first image it is sent.
	p.ModelsCached(ctx, prov)
	sees, known := p.lookupVisionMeta(prov.Name, model)
	return known && sees
}

// visionCapable reports whether a model id is known to accept image input.
// Matches the bare upstream id (provider scope stripped) against the
// configured/default glob list.
func (p *Proxy) visionCapable(model string) bool {
	raw := strings.TrimSpace(p.Store.Setting("vision_models"))
	if raw == "-" {
		// kill-switch: treat every model as capable, which disables proactive
		// routing without touching the on-error vision chain
		return true
	}
	pats := DefaultVisionModels
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

// VisionCapable is visionCapable for callers outside the package — the
// `cfrproxy vision` diagnostic uses it so operators can see exactly how a model
// id is classified before an image ever depends on the answer.
func (p *Proxy) VisionCapable(model string) bool { return p.visionCapable(model) }

// VisionCapableFor is visionCapableFor for callers outside the package. It
// consults the provider's own declaration, so the diagnostic reports what the
// router will actually do rather than what the name heuristic alone suggests.
func (p *Proxy) VisionCapableFor(ctx context.Context, prov store.Provider, model string) bool {
	return p.visionCapableFor(ctx, prov, model)
}

// VisionModelPatterns returns the effective capability globs and whether they
// came from the "vision_models" setting rather than the built-in defaults.
func (p *Proxy) VisionModelPatterns() (pats []string, custom bool, disabled bool) {
	raw := strings.TrimSpace(p.Store.Setting("vision_models"))
	if raw == "-" {
		return nil, false, true
	}
	if raw != "" {
		return splitList(raw), true, false
	}
	return DefaultVisionModels, false, false
}

// dialectCarriesImages reports whether buildOutbound preserves image parts when
// translating into this provider dialect.
//
// wire.Msg.Images is only re-emitted by the OpenAI and Responses builders. The
// others still flatten to text, so an image routed through them would arrive as
// a blind question — those targets must be skipped instead. Extend this the
// moment a builder learns to carry images, not before: being wrong here is
// silent and produces confident nonsense rather than an error.
func dialectCarriesImages(ptype string) bool {
	switch ptype {
	case "responses":
		return true
	case "anthropic", "ollama", "commandcode":
		return false
	default: // openai
		return true
	}
}

func (p *Proxy) VisionFallbackConfig() VisionFallback {
	var c VisionFallback
	if raw := p.Store.Setting("vision_fallback"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// bodyHasImage reports whether the RAW inbound body carries an image part.
//
// This deliberately inspects the raw bytes rather than wire.Request: the
// internal representation flattens content-part arrays to text via
// oaiContentText, which keeps only {"type":"text"} parts and drops images
// entirely. Asking wire.Request whether it has an image would always say no.
//
// Recognised shapes:
//
//	OpenAI     {"type":"image_url","image_url":{"url":"data:..."}}
//	OpenAI     {"type":"input_image", ...}
//	Anthropic  {"type":"image","source":{...}}
//	Ollama     {"role":"user","images":["<base64>"]}
func bodyHasImage(body []byte) bool {
	// Cheap prefilter: a text-only conversation never reaches the JSON walk.
	// A false positive here (the substring appearing inside base64 or prose)
	// only costs one parse that then returns false.
	if !bytes.Contains(body, []byte("image")) {
		return false
	}
	var doc any
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	return walkForImage(doc, 0)
}

func walkForImage(v any, depth int) bool {
	if depth > 12 {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["type"].(string); ok {
			switch s {
			case "image_url", "input_image", "image":
				return true
			}
		}
		// Ollama attaches images as a sibling array rather than a content part.
		if imgs, ok := t["images"].([]any); ok && len(imgs) > 0 {
			return true
		}
		for _, vv := range t {
			if walkForImage(vv, depth+1) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if walkForImage(vv, depth+1) {
				return true
			}
		}
	}
	return false
}

// visionFailure detects a provider rejection that is specifically about the
// image, as opposed to a generic bad request. These are worth continuing down
// the vision chain because a different model can still serve them.
//
// The canonical case this was written for: Alibaba/Qwen's OpenAI-compatible
// endpoint has to materialise inline base64 as a hosted file before the model
// reads it, and returns
//
//	{"code":"invalid_parameter_error","message":"Download multimodal file timed out"}
//
// as an HTTP 400 — which the harness classifies as non-retryable and aborts on.
func visionFailure(body []byte) bool {
	// Matching runs on the raw body, so a provider message containing double
	// quotes arrives JSON-escaped (`\"image_url\"`). Unescape them first, or
	// every quoted pattern below silently never matches.
	s := strings.ReplaceAll(strings.ToLower(string(body)), `\"`, `"`)
	for _, m := range []string{
		"download multimodal file timed out",
		"multimodal file",
		"failed to download image",
		"image download",
		"does not support image",
		"image input is not supported",
		"image is not supported",
		"vision is not supported",
		"no vision support",
		"unsupported image",
		"invalid image",
		"image too large",
		"image exceeds",
		"image_url is not supported",
		"content type image",
		// Rust/serde-backed gateways (OpenCode's Console among them) reject an
		// image part as an unknown enum variant rather than saying "no vision":
		//   unknown variant `image_url`, expected `text`
		"unknown variant `image_url`",
		"unknown variant `image`",
		`unknown variant "image_url"`,
		`unknown variant "image"`,
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// appendVisionFallback extends cands with the configured vision chain.
//
// No-ops entirely unless the request carries an image, so this never perturbs
// routing for text traffic. Dedupe is by provider+model to match
// appendGlobalFallback, and share endpoints keep their allow-list.
func (p *Proxy) appendVisionFallback(ctx context.Context, cands []candidate, ep *store.Endpoint, hasImage bool) []candidate {
	cfg := p.VisionFallbackConfig()
	if !cfg.Enabled || len(cfg.Targets) == 0 {
		return cands
	}
	if !hasImage {
		return cands
	}
	seen := make(map[string]bool, len(cands)+len(cfg.Targets))
	for _, c := range cands {
		seen[pairKey(c.prov.ID, c.model)] = true
	}
	added := 0
	for _, t := range cfg.Targets {
		if added >= maxVisionHops {
			break
		}
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		fprov, fmodel, err := p.ResolveModel(ctx, t)
		if err != nil || !fprov.Enabled {
			continue
		}
		k := pairKey(fprov.ID, fmodel)
		if seen[k] {
			continue
		}
		if ep != nil && ep.ForceModel == "" && !p.modelAllowed(*ep, t) {
			continue
		}
		seen[k] = true
		cands = append(cands, candidate{prov: fprov, model: fmodel, failover: true, vision: true})
		added++
	}
	return cands
}

// ---- advertised context length ----

// contextMetaCache holds per-model context windows as declared by a provider
// (llama-swap publishes `meta.llamaswap.context`), same idea as visionMetaCache.
type contextMetaCache struct {
	mu      sync.Mutex
	entries map[string]int // "provider/model" (lowercased) → context tokens
}

func (p *Proxy) recordContextMeta(prov, model string, n int) {
	p.ctxmeta.mu.Lock()
	p.ctxmeta.entries[visionMetaKey(prov, model)] = n
	p.ctxmeta.mu.Unlock()
}

func (p *Proxy) lookupContextMeta(prov, model string) int {
	p.ctxmeta.mu.Lock()
	defer p.ctxmeta.mu.Unlock()
	return p.ctxmeta.entries[visionMetaKey(prov, model)]
}

// asInt normalises a JSON number that providers may send as a string.
// llama-swap sends context as "8192", not 8192.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// DefaultContextLength is the last-resort context window advertised when
// nothing else is known. Override with the "default_context_length" setting.
const DefaultContextLength = 0 // 0 = advertise nothing rather than guess

// ContextLengthFor resolves the context window cfrproxy advertises for a
// provider+model, most specific source first:
//
//  1. the provider's explicit context_length override — the operator's word
//  2. what the upstream itself declared on /v1/models
//  3. the curated catalog of published windows (see modelcontext.go)
//  4. the "default_context_length" setting
//  5. nothing (0), so the field is omitted rather than fabricated
//
// The catalog sits below the upstream so a provider that reports its own real
// window (llama-swap does, per model) always wins, and above the blanket
// setting because a per-model published number beats one global guess.
//
// Harnesses size compaction from this. Claude Code and Hermes otherwise guess
// from the model id, and cfrproxy's ids are frequently renamed by the upstream
// ("claude-opencode-go-deepseek-v4-flash"), so the guess lands on a default
// that is wrong in both directions.
// ContextLimitFor resolves the window to enforce for one request, honouring a
// share endpoint's cap.
//
// The cap is deliberately one-directional: it can only LOWER the window derived
// from the upstream, never raise it. A share that could advertise more than the
// backend holds would let a harness build prompts no slot can accept — a real
// failure we have already paid for once, where a model whose slot holds 98,304
// tokens was advertised as 256,000 and an agent ran 3h36m before dying.
//
// The one case where the cap supplies rather than limits is an upstream that
// reports nothing at all (derived == 0). There is no number to contradict, and
// a documented operator value beats the harness inventing one from the model id.
func (p *Proxy) ContextLimitFor(ep *store.Endpoint, prov store.Provider, model string) int {
	derived := p.ContextLengthFor(prov, model)
	if ep == nil || ep.ContextLength <= 0 {
		return derived
	}
	if derived <= 0 || ep.ContextLength < derived {
		return ep.ContextLength
	}
	return derived
}

func (p *Proxy) ContextLengthFor(prov store.Provider, model string) int {
	if prov.ContextLength > 0 {
		return prov.ContextLength
	}
	if n := p.lookupContextMeta(prov.Name, model); n > 0 {
		return n
	}
	if n := catalogContextFor(model); n > 0 {
		return n
	}
	if raw := strings.TrimSpace(p.Store.Setting("default_context_length")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultContextLength
}

// advertisedContext resolves the context window for a listing entry. Scoped
// mounts (/p/{provider}/v1/models) carry bare ids and already know the
// provider; the global list carries "provider/model" and has to split it.
// Virtual ids (auto, fusion, auto:<router>) belong to no provider and return 0.
// AdvertisedContext exposes advertisedContext to the admin API so the UI can
// display the window a model actually reports.
func (p *Proxy) AdvertisedContext(scope, id string) int { return p.advertisedContext(scope, id) }

func (p *Proxy) advertisedContext(scope, id string) int {
	if scope != "" {
		prov, ok := p.Store.ProviderByName(scope)
		if !ok {
			return 0
		}
		return p.ContextLengthFor(prov, id)
	}
	i := strings.IndexByte(id, '/')
	if i <= 0 {
		// An unqualified id can still be an operator-declared model_map alias,
		// which resolves to a real provider/model. It has to advertise THAT
		// model's window: a harness left to guess is exactly how REQ-086's
		// 395k-into-262k overflow happened, and REQ-089 found the same class of
		// bug again in llama-swap's own metadata.
		if mapped := p.Store.ModelMapLookup(id, MatchMapPattern); mapped != "" {
			if j := strings.IndexByte(mapped, '/'); j > 0 {
				if prov, ok := p.Store.ProviderByName(mapped[:j]); ok {
					return p.ContextLengthFor(prov, mapped[j+1:])
				}
			}
		}
		return 0
	}
	prov, ok := p.Store.ProviderByName(id[:i])
	if !ok {
		return 0
	}
	return p.ContextLengthFor(prov, id[i+1:])
}

// lastByteReader stamps the moment the upstream last produced data, so
// post-processing can be measured as everything that happened after it.
//
// Timing the end of the relay instead does not work: by then cfrproxy has
// already done its translation and flush, so the delta is always ~0. And the
// raw-passthrough path returns before any such marker is reached at all.
// Wrapping the body covers every path with one change.
type lastByteReader struct {
	r    io.ReadCloser
	mu   sync.Mutex
	last time.Time
}

func newLastByteReader(r io.ReadCloser) *lastByteReader { return &lastByteReader{r: r} }

func (l *lastByteReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.mu.Lock()
		l.last = time.Now()
		l.mu.Unlock()
	}
	return n, err
}

func (l *lastByteReader) Close() error { return l.r.Close() }

func (l *lastByteReader) lastByte() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}
