package main

// Web search for round-table panelists.
//
// Panelists are heterogeneous (Claude, Codex, Gemini, Grok, local models), and
// each vendor's *native* search is a different server-side tool that does not
// survive dialect translation. So search is a plain function tool that cfrproxy
// executes itself: any model that can call a tool can research, and they all
// see identical results — which matters when the point of the panel is to
// compare reasoning rather than compare search backends.
//
// The backend is SearXNG (self-hosted metasearch): no API key, no per-query
// cost, and it aggregates several engines.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// searxURL resolves the SearXNG base URL: explicit config wins, then the
// SEARXNG_ENDPOINT environment variable, then the common local default.
func searxURL(configured string) string {
	if s := strings.TrimSpace(configured); s != "" {
		return strings.TrimRight(s, "/")
	}
	if s := strings.TrimSpace(os.Getenv("SEARXNG_ENDPOINT")); s != "" {
		return strings.TrimRight(s, "/")
	}
	return "http://127.0.0.1:9090"
}

// webSearchTool is the OpenAI-style function definition handed to panelists.
var webSearchTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "web_search",
		"description": "Search the web for current information. Use for facts that may have changed, recent events, version numbers, documentation, benchmarks, or anything you are not confident about from memory. Returns titles, URLs and snippets.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query. Be specific; prefer distinctive terms over full sentences.",
				},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
	},
}

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// runWebSearch queries SearXNG and returns a compact plain-text digest.
//
// The digest is deliberately small: it is fed straight back into the panelist's
// context, and every panelist pays for it on every subsequent turn of the same
// conversation.
func runWebSearch(base, query string, maxResults int) (string, error) {
	if maxResults <= 0 {
		maxResults = 6
	}
	u := fmt.Sprintf("%s/search?q=%s&format=json", base, url.QueryEscape(query))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("searxng unreachable at %s: %w", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("searxng HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []searchResult `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("searxng returned non-JSON (is format=json enabled in settings.yml?)")
	}
	if len(out.Results) == 0 {
		return "No results found for that query.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n\n", query)
	for i, r := range out.Results {
		if i >= maxResults {
			break
		}
		snippet := strings.TrimSpace(r.Content)
		if len(snippet) > 400 {
			snippet = snippet[:400] + "…"
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
		if snippet != "" {
			fmt.Fprintf(&b, "   %s\n", snippet)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), nil
}

// maxToolRounds caps how many times one panelist may call a tool before it must
// answer. A panelist that keeps searching instead of concluding would otherwise
// stall the whole panel (perCallTimeout is shared across the loop).
const maxToolRounds = 4

// chatWithTools is chatViaProxy plus a tool-calling loop offering web_search.
//
// It speaks the OpenAI tool protocol because that is what the proxy's inbound
// /v1/chat/completions accepts, and the proxy translates onward per provider —
// so a panelist on Anthropic and one on a local model both get the same tool.
//
// Falls back to the plain answer whenever the model doesn't ask for a tool,
// which is also what happens for models with no tool support: they simply
// answer, and the panel still works.
func chatWithTools(addr, model, system, user, temperature string, maxTokens int, timeout time.Duration, acc *usageAcc, searx string, log func(string)) (string, error) {
	msgs := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
	deadline := time.Now().Add(timeout)

	for round := 0; ; round++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", fmt.Errorf("timed out during tool use")
		}
		body := map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"messages":   msgs,
		}
		// Stop offering the tool once the budget is spent, so the final call is
		// forced to produce prose rather than another search.
		if round < maxToolRounds {
			body["tools"] = []any{webSearchTool, context7Tool}
		}
		if temperature != "" {
			var t float64
			if _, err := fmt.Sscanf(temperature, "%g", &t); err == nil {
				body["temperature"] = t
			}
		}
		b, _ := json.Marshal(body)
		client := &http.Client{Timeout: remaining}
		resp, err := client.Post(addr+"/v1/chat/completions", "application/json", strings.NewReader(string(b)))
		if err != nil {
			return "", err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		var out struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rb, &out); err != nil {
			return "", fmt.Errorf("bad response: %s", string(rb[:min(len(rb), 200)]))
		}
		if out.Error != nil {
			return "", fmt.Errorf("%s", out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("empty response")
		}
		acc.add(out.Usage.PromptTokens, out.Usage.CompletionTokens)
		msg := out.Choices[0].Message

		if len(msg.ToolCalls) == 0 {
			return strings.TrimSpace(msg.Content), nil
		}

		// echo the assistant's tool_calls back, then one result per call
		asst := map[string]any{"role": "assistant", "content": msg.Content}
		var tcs []map[string]any
		for _, tc := range msg.ToolCalls {
			tcs = append(tcs, map[string]any{
				"id": tc.ID, "type": "function",
				"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		asst["tool_calls"] = tcs
		msgs = append(msgs, asst)

		for _, tc := range msg.ToolCalls {
			result := fmt.Sprintf("Unknown tool %q.", tc.Function.Name)
			var args struct {
				Query   string `json:"query"`
				Library string `json:"library"`
				Topic   string `json:"topic"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			switch tc.Function.Name {
			case "web_search":
				if strings.TrimSpace(args.Query) == "" {
					result = "No query supplied."
				} else if r, err := runWebSearch(searx, args.Query, 6); err != nil {
					// a tool failure must not kill the panelist — hand the error
					// back and let it answer from what it already knows
					result = "Search failed: " + err.Error() + ". Answer from your own knowledge and say so."
				} else {
					result = r
					if log != nil {
						log("web: " + args.Query)
					}
				}
			case "library_docs":
				if strings.TrimSpace(args.Library) == "" {
					result = "No library supplied."
				} else if r, err := runContext7(args.Library, args.Topic, 4000); err != nil {
					result = "Docs lookup failed: " + err.Error() + ". Fall back to web_search or your own knowledge, and say which."
				} else {
					result = r
					q := "docs: " + args.Library
					if args.Topic != "" {
						q += " (" + args.Topic + ")"
					}
					if log != nil {
						log(q)
					}
				}
			}
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": tc.ID, "content": result,
			})
		}
	}
}

// searchLog records the queries a panel actually ran, so the report can show
// what the answers were grounded in rather than leaving the user guessing
// whether anyone searched at all.
type searchLog struct {
	mu sync.Mutex
	qs []string
}

func (l *searchLog) add(q string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if len(l.qs) < 40 { // bound: a runaway panel can't grow this without limit
		l.qs = append(l.qs, q)
	}
	l.mu.Unlock()
}

func (l *searchLog) list() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.qs))
	copy(out, l.qs)
	return out
}

// panelCall runs one panelist turn with research tools available.
//
// Research is unconditional: a panel that recommends a library API or quotes a
// benchmark from memory is confidently wrong often enough to be worse than no
// panel. Models that can't call tools simply answer, so the panel still works.
func panelCall(cfg rtConfig, addr, model, system, user, temperature string, maxTokens int, timeout time.Duration, acc *usageAcc, searched *searchLog) (string, error) {
	return chatWithTools(addr, model, system, user, temperature, maxTokens, timeout, acc, searxURL(cfg.SearxURL), searched.add)
}

// researchBrief is appended to every panelist's system prompt.
const researchBrief = "\nYou have two research tools and are expected to use them rather than rely on memory for anything time-sensitive. " +
	"library_docs: current documentation for any library, framework, SDK or CLI — use it for API syntax, config, and version-specific behaviour, even for libraries you know well, because your memory may predate the current release. " +
	"web_search: everything else that may have changed — releases, prices, benchmarks, incidents, who-does-what-now. " +
	"Look things up when a claim would change your verdict; cite the source inline. Do not stall — finish your answer in this turn."

// context7Tool gives panelists authoritative, version-current library
// documentation. General web search is good at "what happened" but poor at
// "what is this API now" — it surfaces blog posts and Stack Overflow answers
// pinned to whatever version was current when they were written, which is
// exactly how a panel ends up confidently recommending a removed API.
var context7Tool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "library_docs",
		"description": "Fetch current documentation for a software library, framework, SDK or CLI (via Context7). Use this — not web_search — for API syntax, configuration, migration steps, and anything version-specific. Prefer it even for libraries you know well, since your memory may predate the current release.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"library": map[string]any{
					"type":        "string",
					"description": "Library name, e.g. \"next.js\", \"fastapi\", \"tailwind css\".",
				},
				"topic": map[string]any{
					"type":        "string",
					"description": "Optional area to focus on, e.g. \"routing\", \"authentication\", \"migration to v3\".",
				},
			},
			"required":             []string{"library"},
			"additionalProperties": false,
		},
	},
}

const context7Base = "https://context7.com/api/v1"

// resolveLibrary maps a human library name to a Context7 library id, picking
// the best-scored match.
func resolveLibrary(name string) (string, string, error) {
	u := context7Base + "/search?query=" + url.QueryEscape(name)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("context7 search HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			ID          string  `json:"id"`
			Title       string  `json:"title"`
			Description string  `json:"description"`
			Score       float64 `json:"score"`
			TrustScore  float64 `json:"trustScore"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("context7 search returned non-JSON")
	}
	if len(out.Results) == 0 {
		return "", "", fmt.Errorf("no library matched %q", name)
	}
	best := out.Results[0]
	for _, r := range out.Results[1:] {
		// results arrive score-ordered; prefer a clearly more trusted source
		// only when its relevance is comparable
		if r.Score > best.Score || (r.TrustScore > best.TrustScore+2 && r.Score > best.Score*0.85) {
			best = r
		}
	}
	return best.ID, best.Title, nil
}

// runContext7 resolves a library then fetches its docs as plain text.
func runContext7(library, topic string, tokens int) (string, error) {
	if tokens <= 0 {
		tokens = 4000
	}
	id, title, err := resolveLibrary(library)
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/%s?type=txt&tokens=%d", context7Base, strings.TrimPrefix(id, "/"), tokens)
	if t := strings.TrimSpace(topic); t != "" {
		u += "&topic=" + url.QueryEscape(t)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("context7 docs HTTP %d for %s", resp.StatusCode, id)
	}
	doc := strings.TrimSpace(string(body))
	if doc == "" {
		return "", fmt.Errorf("context7 returned no documentation for %s", id)
	}
	header := fmt.Sprintf("Documentation for %s (%s)", title, id)
	if topic != "" {
		header += ", topic: " + topic
	}
	return header + "\n\n" + doc, nil
}
