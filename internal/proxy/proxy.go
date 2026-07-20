// Package proxy implements the data plane: inbound dialect endpoints
// (/v1/chat/completions, /v1/messages, /api/chat), provider resolution,
// declarative transforms, and stream re-framing between dialects.
package proxy

import (
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
	Store  *store.Store
	Hub    *Hub
	Client *http.Client
	models modelCache
}

func New(s *store.Store) *Proxy {
	return &Proxy{Store: s, Hub: NewHub(), Client: &http.Client{Timeout: 10 * time.Minute},
		models: modelCache{entries: map[int64]modelCacheEntry{}}}
}

func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "openai") })
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "anthropic") })
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { p.handle(w, r, "ollama") })
	mux.HandleFunc("GET /v1/models", p.handleModels)
	mux.HandleFunc("GET /api/tags", p.handleTags)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"version": "0.6.0-cfrproxy"})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	for _, id := range p.AllModelIDs(r.Context()) {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "cfrproxy"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) handleTags(w http.ResponseWriter, r *http.Request) {
	var models []map[string]any
	for _, id := range p.AllModelIDs(r.Context()) {
		models = append(models, map[string]any{"name": id, "model": id, "modified_at": time.Now().UTC().Format(time.RFC3339)})
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request, inbound string) {
	start := time.Now()
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
	prov, model, err := p.ResolveModel(r.Context(), req.Model)
	if err != nil {
		httpErr(w, inbound, 503, err.Error())
		return
	}
	req.Model = model

	tr := &store.Trace{TS: start.UnixMilli(), Provider: prov.Name, Model: model, Inbound: inbound,
		Stream: req.Stream, ReqSnip: snip(body)}
	defer func() {
		tr.LatencyMS = time.Since(start).Milliseconds()
		p.Store.AddTrace(tr)
		p.Hub.Publish(*tr)
	}()

	// candidate chain: primary, then its configured fallback (single hop).
	// Transient failures (transport, 408/429/5xx) retry once, then fail over.
	cands := []candidate{{prov: prov, model: model}}
	if prov.Fallback != "" {
		if fprov, fmodel, ferr := p.ResolveModel(r.Context(), prov.Fallback); ferr == nil && fprov.ID != prov.ID {
			cands = append(cands, candidate{prov: fprov, model: fmodel, failover: true})
		}
	}

	var resp *http.Response
	var used candidate
	var respRules []transform.Rule
	var passth bool
	lastErr := ""
	for _, c := range cands {
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
		passthrough := inbound == c.prov.Type && len(reqRules) == 0 && len(respR) == 0 && !c.prov.InjectDocs
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
		for attempt := 0; attempt < 2 && resp == nil; attempt++ {
			if attempt > 0 {
				time.Sleep(1200 * time.Millisecond)
			}
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
				continue
			}
			resp = r2
		}
		if resp != nil {
			used, respRules, passth = c, respR, passthrough
			req = &creq
			break
		}
	}
	if resp == nil {
		tr.Status, tr.Err = 502, lastErr
		httpErr(w, inbound, 502, lastErr)
		return
	}
	defer resp.Body.Close()
	tr.Provider, tr.Model = used.prov.Name, used.model
	if used.failover {
		tr.Err = "failover from " + prov.Name + " (" + lastErr + ")"
	}

	if resp.StatusCode >= 400 { // non-transient provider error (auth, bad request)
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tr.Status = resp.StatusCode
		tr.Err = snip(eb)
		httpErr(w, inbound, resp.StatusCode, fmt.Sprintf("provider %s: %s", used.prov.Name, snip(eb)))
		return
	}

	if passth {
		p.copyRaw(w, resp, req.Stream, tr)
		return
	}

	if req.Stream {
		deltas := make(chan wire.Delta, 16)
		go readStream(used.prov.Type, resp.Body, deltas)
		if err := writeStream(inbound, w, req.Model, deltas); err != nil {
			tr.Status, tr.Err = 200, "stream aborted: "+err.Error()
			return
		}
		tr.Status = 200
		tr.RespSnip = "(streamed)"
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

// copyRaw streams the provider's bytes through untouched (passthrough mode).
func (p *Proxy) copyRaw(w http.ResponseWriter, resp *http.Response, stream bool, tr *store.Trace) {
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
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	tr.RespSnip = snip(rb)
	w.Write(rb)
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
		if strings.HasSuffix(base, "/v1") {
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
	if prov.Type == "openai" && !strings.HasSuffix(base, "/v1") && !strings.HasSuffix(base, "/api") {
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
