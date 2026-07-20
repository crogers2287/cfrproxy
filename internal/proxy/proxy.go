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
}

func New(s *store.Store) *Proxy {
	return &Proxy{Store: s, Hub: NewHub(), Client: &http.Client{Timeout: 10 * time.Minute}}
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

// exposed model list = provider/model combos + aliases, for harness pickers
func (p *Proxy) modelIDs() []string {
	var ids []string
	for _, prov := range p.Store.Providers() {
		if !prov.Enabled {
			continue
		}
		if prov.DefaultModel != "" {
			ids = append(ids, prov.Name+"/"+prov.DefaultModel)
		}
		for _, alias := range strings.Split(prov.Models, ",") {
			if a := strings.TrimSpace(alias); a != "" {
				ids = append(ids, a)
			}
		}
	}
	if len(ids) == 0 {
		ids = []string{"default"}
	}
	return ids
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	for _, id := range p.modelIDs() {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "cfrproxy"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (p *Proxy) handleTags(w http.ResponseWriter, r *http.Request) {
	var models []map[string]any
	for _, id := range p.modelIDs() {
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
	prov, model, err := p.Store.Resolve(req.Model)
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

	// docs injection
	if prov.InjectDocs && prov.DocMarkdown != "" {
		if req.System != "" {
			req.System = prov.DocMarkdown + "\n\n" + req.System
		} else {
			req.System = prov.DocMarkdown
		}
	}

	reqRules, respRules, err := p.rules(prov.ID, inbound)
	if err != nil {
		tr.Status, tr.Err = 500, err.Error()
		httpErr(w, inbound, 500, err.Error())
		return
	}

	// raw fast path: same dialect in and out, nothing to rewrite
	if inbound == prov.Type && len(reqRules) == 0 && len(respRules) == 0 && !prov.InjectDocs {
		p.passthrough(w, r.Context(), prov, model, body, req.Stream, inbound, tr)
		return
	}

	outBody, err := buildOutbound(prov.Type, req)
	if err != nil {
		tr.Status, tr.Err = 500, err.Error()
		httpErr(w, inbound, 500, err.Error())
		return
	}
	outBody = transform.Apply(outBody, reqRules)

	resp, err := p.send(r.Context(), prov, providerPath(prov.Type), outBody)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, inbound, 502, err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		tr.Status, tr.Err = resp.StatusCode, snip(eb)
		httpErr(w, inbound, resp.StatusCode, fmt.Sprintf("provider %s: %s", prov.Name, snip(eb)))
		return
	}

	if req.Stream {
		deltas := make(chan wire.Delta, 16)
		go readStream(prov.Type, resp.Body, deltas)
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
	norm, err := parseOutboundResponse(prov.Type, rb)
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

// passthrough forwards the raw body (model field rewritten) and copies the
// provider's response bytes straight through — the zero-translation fast path.
func (p *Proxy) passthrough(w http.ResponseWriter, ctx context.Context, prov store.Provider, model string, body []byte, stream bool, inbound string, tr *store.Trace) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err == nil {
		doc["model"] = model
		if b, err := json.Marshal(doc); err == nil {
			body = b
		}
	}
	resp, err := p.send(ctx, prov, providerPath(prov.Type), body)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, inbound, 502, err.Error())
		return
	}
	defer resp.Body.Close()
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
