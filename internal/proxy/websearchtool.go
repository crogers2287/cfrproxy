package proxy

// Emulation of Anthropic's server-side web_search tool.
//
// Claude Code performs a WebSearch by sending a dedicated sub-request whose
// tools list holds `{"type":"web_search_20250305","name":"web_search"}` and
// whose system prompt says "You are an assistant for performing a web search
// tool use". Anthropic's API executes that tool itself; no provider behind
// this proxy can, so the request used to reach a local model as plain text
// and Claude Code reported "no sources" (trace 169443).
//
// Here the proxy runs the tool: the model gets a function tool `web_search`,
// the proxy executes it against SearXNG (already used by the round table),
// feeds results back, and once the model answers, returns the Anthropic
// shape Claude Code expects — server_tool_use + web_search_tool_result blocks
// with the real URLs, then the text. The model is whatever the request would
// have routed to (`auto` included), overridable per setting.
//
// Setting "web_search": {"enabled":true,"searx":"http://127.0.0.1:9090",
// "model":"","max_results":6,"max_uses":5}. Enabled by default.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

type WebSearchConfig struct {
	Enabled    *bool  `json:"enabled,omitempty"`
	Searx      string `json:"searx,omitempty"`
	Model      string `json:"model,omitempty"` // provider/model that answers search sub-requests; "" = as routed
	MaxResults int    `json:"max_results,omitempty"`
	MaxUses    int    `json:"max_uses,omitempty"`
}

func (c WebSearchConfig) on() bool { return c.Enabled == nil || *c.Enabled }
func (c WebSearchConfig) searxBase() string {
	if s := strings.TrimSpace(c.Searx); s != "" {
		return strings.TrimRight(s, "/")
	}
	if s := strings.TrimSpace(os.Getenv("SEARXNG_ENDPOINT")); s != "" {
		return strings.TrimRight(s, "/")
	}
	return "http://127.0.0.1:9090"
}
func (c WebSearchConfig) maxResults() int {
	if c.MaxResults > 0 {
		return c.MaxResults
	}
	return 6
}
func (c WebSearchConfig) maxUses(requested int) int {
	n := c.MaxUses
	if n <= 0 {
		n = 5
	}
	if requested > 0 && requested < n {
		n = requested
	}
	return n
}

func (p *Proxy) WebSearchConfig() WebSearchConfig {
	var c WebSearchConfig
	if raw := p.Store.Setting("web_search"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

var webSearchFuncTool = wire.Tool{
	Name:        "web_search",
	Description: "Search the web. Returns the top results (title, url, snippet). Call it with a focused query; call again with a refined query if the results do not answer the question.",
	Params:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the search query"}},"required":["query"]}`),
}

type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// searx runs one query against SearXNG's JSON API.
func (p *Proxy) searx(ctx context.Context, base, query string, n int) ([]searxResult, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json", base, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng unreachable at %s: %w", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("searxng HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []searxResult `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("searxng returned non-JSON (format=json disabled?)")
	}
	if len(out.Results) > n {
		out.Results = out.Results[:n]
	}
	for i := range out.Results {
		if s := strings.TrimSpace(out.Results[i].Content); len(s) > 400 {
			out.Results[i].Content = s[:400] + "…"
		}
	}
	return out.Results, nil
}

func formatResults(query string, rs []searxResult) string {
	if len(rs) == 0 {
		return fmt.Sprintf("No results for %q.", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n\n", query)
	for i, r := range rs {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
		if s := strings.TrimSpace(r.Content); s != "" {
			fmt.Fprintf(&b, "   %s\n", s)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

type webSearchRound struct {
	Query   string
	Results []searxResult
}

// searchBlocks renders the rounds as the Anthropic content blocks Claude Code
// reads its sources from.
func searchBlocks(rounds []webSearchRound) []json.RawMessage {
	var out []json.RawMessage
	for i, r := range rounds {
		id := fmt.Sprintf("srvtoolu_%d_%d", time.Now().UnixNano(), i)
		use, _ := json.Marshal(map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": map[string]any{"query": r.Query}})
		var results []map[string]any
		for _, x := range r.Results {
			results = append(results, map[string]any{"type": "web_search_result", "url": x.URL, "title": x.Title,
				"encrypted_content": "", "page_age": nil})
		}
		if results == nil {
			results = []map[string]any{}
		}
		res, _ := json.Marshal(map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": results})
		out = append(out, use, res)
	}
	return out
}

// webSearchTarget picks the model that answers the sub-request: the setting's
// override, else whatever the request would have routed to.
func (p *Proxy) webSearchTarget(ctx context.Context, req *wire.Request, reqModel string, cfg WebSearchConfig) (store.Provider, string, string) {
	note := ""
	target := strings.TrimSpace(cfg.Model)
	if target == "" {
		target = reqModel
		var rcfg AutoRouterConfig
		have := false
		switch {
		case target == "auto" || target == "cfr-auto" || target == "auto-plan" || strings.HasSuffix(target, "/auto") || strings.HasSuffix(target, "/auto-plan"):
			rcfg, have = p.AutoRouterConfig(), true
		case strings.HasPrefix(target, "auto:"):
			rcfg, have = p.NamedRouterConfig(target[len("auto:"):])
		case strings.HasPrefix(target, "auto-plan:"):
			rcfg, have = p.NamedRouterConfig(target[len("auto-plan:"):])
		}
		if have {
			if routed, bucket := p.AutoRouteWith(ctx, req, rcfg); routed != "" {
				if i := strings.Index(bucket, " conv:"); i >= 0 {
					bucket = bucket[:i]
				}
				note = "auto→" + bucket + "→" + routed
				target = routed
			}
		}
	}
	prov, model, err := p.ResolveModel(ctx, target)
	if err != nil {
		return store.Provider{}, "", note
	}
	return prov, model, note
}

// handleWebSearch serves a request that carries the server tool. Returns
// false when the emulation is off, so handleCore continues as before.
func (p *Proxy) handleWebSearch(w http.ResponseWriter, r *http.Request, inbound string, req *wire.Request, reqModel string, start time.Time) bool {
	cfg := p.WebSearchConfig()
	if req.WebSearch == nil || !cfg.on() {
		return false
	}
	ctx := r.Context()
	prov, model, routeNote := p.webSearchTarget(ctx, req, reqModel, cfg)
	if model == "" {
		httpErr(w, inbound, 503, "web search: no model to answer the search sub-request")
		return true
	}
	tr := &store.Trace{TS: start.UnixMilli(), Provider: prov.Name, Model: model, Inbound: inbound, Stream: req.Stream,
		ReqSnip: snip([]byte(lastUserText(req, 400))), Client: clientLabel(r)}
	defer func() {
		tr.LatencyMS = time.Since(start).Milliseconds()
		p.Store.AddTrace(tr)
		p.Hub.Publish(*tr)
		logTrace(r, tr)
	}()

	creq := *req
	creq.Model = model
	creq.Stream = false
	creq.Tools = append([]wire.Tool{webSearchFuncTool}, req.Tools...)
	creq.WebSearch = nil
	if creq.MaxTokens < 1024 {
		creq.MaxTokens = 2048
	}
	creq.Messages = append([]wire.Msg(nil), req.Messages...)
	lvl, force := reasoningFor(nil, prov)

	var rounds []webSearchRound
	var text string
	var pt, ct int
	maxUses := cfg.maxUses(req.WebSearch.MaxUses)
	used := 0
	callModel := func() (*wire.Response, error) {
		body, err := buildOutbound(prov.Type, &creq)
		if err != nil {
			return nil, err
		}
		if lvl != "" {
			if nb, changed := applyReasoning(body, prov.Type, lvl, force); changed {
				body = nb
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		resp, err := p.send(cctx, prov, providerPath(prov.Type), body)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%s: HTTP %d %s", prov.Name, resp.StatusCode, trimErr(string(rb)))
		}
		norm, err := parseOutboundResponse(prov.Type, rb)
		if err != nil {
			return nil, err
		}
		pt += norm.PromptTokens
		ct += norm.CompletionTokens
		return norm, nil
	}
	runSearch := func(query string) (string, error) {
		rs, err := p.searx(ctx, cfg.searxBase(), query, cfg.maxResults())
		if err != nil {
			return "", err
		}
		rounds = append(rounds, webSearchRound{Query: query, Results: rs})
		used++
		return formatResults(query, rs), nil
	}
	var loopErr error
	for round := 0; round <= maxUses; round++ {
		norm, err := callModel()
		if err != nil {
			loopErr = err
			break
		}
		var calls []wire.ToolCall
		for _, tc := range norm.ToolCalls {
			if tc.Name == "web_search" {
				calls = append(calls, tc)
			}
		}
		if len(calls) == 0 {
			text = norm.Content
			if used == 0 && round == 0 {
				// The model answered from memory. A search request without a
				// search is exactly the "no sources" failure; run the user's
				// question itself once and let the model answer from results.
				q := strings.TrimSpace(lastUserText(req, 300))
				if q != "" {
					if res, err := runSearch(q); err != nil {
						loopErr = err
					} else {
						creq.Messages = append(creq.Messages, wire.Msg{Role: "user", Content: "Web search results for your reference. Answer the question using them and cite the URLs you relied on:\n\n" + res})
						if n2, err := callModel(); err == nil && n2.Content != "" {
							text = n2.Content
						}
					}
				}
			}
			break
		}
		if used >= maxUses {
			text = norm.Content
			break
		}
		creq.Messages = append(creq.Messages, wire.Msg{Role: "assistant", Content: norm.Content, ToolCalls: calls})
		for _, tc := range calls {
			var args struct {
				Query string `json:"query"`
			}
			json.Unmarshal([]byte(tc.Args), &args)
			q := strings.TrimSpace(args.Query)
			if q == "" {
				q = strings.TrimSpace(lastUserText(req, 300))
			}
			res, err := runSearch(q)
			if err != nil {
				loopErr = err
				res = "search backend error: " + err.Error()
			}
			creq.Messages = append(creq.Messages, wire.Msg{Role: "tool", ToolCallID: tc.ID, Content: res})
			if used >= maxUses {
				break
			}
		}
		if loopErr != nil && used == 0 {
			break
		}
	}
	if text == "" && loopErr != nil {
		text = "Web search could not be completed: " + loopErr.Error()
	}
	if len(rounds) > 0 {
		var b strings.Builder
		b.WriteString(text)
		b.WriteString("\n\nSources:\n")
		seen := map[string]bool{}
		for _, rd := range rounds {
			for _, x := range rd.Results {
				if x.URL == "" || seen[x.URL] {
					continue
				}
				seen[x.URL] = true
				fmt.Fprintf(&b, "- %s — %s\n", strings.TrimSpace(x.Title), x.URL)
			}
		}
		text = b.String()
	}
	tr.PromptTokens, tr.CompletionTokens, tr.Status = pt, ct, 200
	tr.Note = sessionTags(strings.TrimSpace(fmt.Sprintf("web_search×%d %s", len(rounds), routeNote)), req)
	if loopErr != nil {
		tr.Err = "web_search: " + trimErr(loopErr.Error())
		slog.Warn("web search emulation", "err", loopErr, "model", model)
	}
	blocks := searchBlocks(rounds)
	out := &wire.Response{Model: model, Content: text, Blocks: blocks, FinishReason: "stop", PromptTokens: pt, CompletionTokens: ct}
	if req.Stream {
		wire.WriteAnthropicOneShotStream(w, req.Model, blocks, text, pt, ct)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(buildInboundResponse(inbound, out))
	return true
}
