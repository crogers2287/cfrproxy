package main

// MCP server (stdio, newline-delimited JSON-RPC 2.0): a round-table consensus
// harness over the proxy. Agent profiles (WebUI "Agents" tab) each pin a
// persona to a provider/model; the roundtable tool fans the question out,
// optionally runs a cross-critique round, then a moderator synthesizes.
// Register in any MCP harness:  claude mcp add roundtable -- cfrproxy mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

var mcpTools = []map[string]any{
	{
		"name":        "roundtable",
		"description": "Convene the agent round table: every enabled agent profile (each a different model behind the cfrproxy router) answers the question independently, optionally cross-critiques, then a moderator synthesizes agreements, disagreements, and a final recommendation. Use for decisions, designs, and reviews that benefit from multiple independent model perspectives.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string", "description": "The question or decision to deliberate"},
				"context":  map[string]any{"type": "string", "description": "Optional background context (code, constraints, prior decisions)"},
				"profiles": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Profile names to include (default: all enabled)"},
				"rounds":   map[string]any{"type": "integer", "description": "1 = answers+synthesis, 2 = adds a cross-critique round (default from settings)"},
			},
			"required": []string{"question"},
		},
	},
	{
		"name":        "consult",
		"description": "Ask a single agent profile (persona + model via cfrproxy) for its take.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"profile":  map[string]any{"type": "string", "description": "Agent profile name"},
				"question": map[string]any{"type": "string"},
				"context":  map[string]any{"type": "string"},
			},
			"required": []string{"profile", "question"},
		},
	},
	{
		"name":        "list_profiles",
		"description": "List the configured agent profiles (name, model, enabled).",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

type rtConfig struct {
	Moderator       string `json:"moderator"`
	Rounds          int    `json:"rounds"`
	MaxTokens       int    `json:"max_tokens"`
	CompressContext bool   `json:"compress_context"`
}

// compressionSummarizer reads the summarizer model from the compression
// settings — the round table reuses it independently of whether global
// compression is enabled.
func compressionSummarizer(s *store.Store) string {
	var c struct {
		Summarizer string `json:"summarizer"`
	}
	if raw := s.Setting("compression"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c.Summarizer
}

func loadRTConfig(s *store.Store) rtConfig {
	c := rtConfig{Rounds: 2, MaxTokens: 1200}
	if raw := s.Setting("roundtable"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	if c.Rounds < 1 {
		c.Rounds = 1
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 1200
	}
	return c
}

// usageAcc thread-safely accumulates token usage across the concurrent
// panel calls of one round table.
type usageAcc struct {
	mu     sync.Mutex
	pt, ct int
}

func (u *usageAcc) add(pt, ct int) {
	if u == nil {
		return
	}
	u.mu.Lock()
	u.pt += pt
	u.ct += ct
	u.mu.Unlock()
}

// perCallTimeout caps how long one panelist may take before it's dropped, so
// a single slow/overloaded model can't stall the whole round table.
const perCallTimeout = 100 * time.Second

// chatViaProxy sends one completion through the local proxy data plane. When
// acc is non-nil, the call's token usage is added to it.
func chatViaProxy(addr, model, system, user string, temperature string, maxTokens int, acc *usageAcc) (string, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if temperature != "" {
		var t float64
		if _, err := fmt.Sscanf(temperature, "%g", &t); err == nil {
			body["temperature"] = t
		}
	}
	b, _ := json.Marshal(body)
	client := &http.Client{Timeout: perCallTimeout}
	resp, err := client.Post(addr+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
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
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func selectProfiles(all []store.AgentProfile, names []string) []store.AgentProfile {
	if len(names) == 0 {
		var out []store.AgentProfile
		for _, a := range all {
			if a.Enabled {
				out = append(out, a)
			}
		}
		return out
	}
	var out []store.AgentProfile
	for _, want := range names {
		for _, a := range all {
			if strings.EqualFold(a.Name, want) {
				out = append(out, a)
			}
		}
	}
	return out
}

func runRoundtable(s *store.Store, addr, question, context string, names []string, rounds int) (string, error) {
	profiles, err := s.AgentProfiles()
	if err != nil {
		return "", err
	}
	panel := selectProfiles(profiles, names)
	if len(panel) == 0 {
		return "", fmt.Errorf("no enabled agent profiles — create some in the WebUI Agents tab")
	}
	cfg := loadRTConfig(s)
	if rounds <= 0 {
		rounds = cfg.Rounds
	}
	start := time.Now()
	acc := &usageAcc{}

	// Compress the shared context ONCE before fanning out, so N panelists
	// don't each receive the full (possibly huge) context. Round-table-only —
	// independent of the global compression toggle.
	compressNote := ""
	compressed := false
	if cfg.CompressContext && len(context) > 1500 {
		if sum := compressionSummarizer(s); sum != "" {
			prompt := "Compress the following context into a faithful, self-contained summary a reviewer can act on. Preserve every decision, fact, constraint, file path, identifier, number, and open question. Drop repetition and filler. Plain text, no preamble, no markdown.\n\nContext:\n" + context
			if c, err := chatViaProxy(addr, sum, "You are a precise technical summarizer.", prompt, "", 900, acc); err == nil && strings.TrimSpace(c) != "" {
				before := len(context)
				context = strings.TrimSpace(c)
				compressed = true
				compressNote = fmt.Sprintf("_Context compressed for the panel: ~%d → ~%d chars (via %s)._\n\n", before, len(context), sum)
			}
		}
	}

	q := question
	if context != "" {
		q = "Context:\n" + context + "\n\nQuestion:\n" + question
	}

	// Round 1: independent answers in parallel
	answers := make([]string, len(panel))
	var wg sync.WaitGroup
	for i, a := range panel {
		wg.Add(1)
		go func(i int, a store.AgentProfile) {
			defer wg.Done()
			sys := a.Persona + "\nYou are speaking on a live panel — a conversation, not a work session. You have your own expertise plus the question and any context below; that is everything you need and everything you get. Open with your verdict in one sentence, then give the 2-4 strongest concrete reasons for it. If a detail is missing, name your assumption in a short clause and reason from it — never pause for more. Take a clear side. Plain prose, under 300 words."
			ans, err := chatViaProxy(addr, a.Model, sys, q, a.Temperature, cfg.MaxTokens, acc)
			if err != nil {
				ans = "(unavailable: " + err.Error() + ")"
			}
			answers[i] = ans
		}(i, a)
	}
	wg.Wait()

	// Round 2: cross-critique, each sees the others
	if rounds > 1 {
		revised := make([]string, len(panel))
		for i, a := range panel {
			wg.Add(1)
			go func(i int, a store.AgentProfile) {
				defer wg.Done()
				var others strings.Builder
				for j, b := range panel {
					if j != i {
						fmt.Fprintf(&others, "%s said:\n%s\n\n", b.Name, answers[j])
					}
				}
				sys := a.Persona + "\nThe other panelists have spoken; their positions are below. This is still a live exchange — respond from judgment, not research. Say plainly where someone shifted your view, where someone is wrong and exactly why, and land on your final verdict in one clear sentence. Concede real points; hold your ground on the rest. Plain prose, under 250 words."
				ans, err := chatViaProxy(addr, a.Model, sys, q+"\n\nOther panelists:\n"+others.String(), a.Temperature, cfg.MaxTokens, acc)
				if err != nil {
					ans = answers[i] // keep round-1 answer on failure
				}
				revised[i] = ans
			}(i, a)
		}
		wg.Wait()
		answers = revised
	}

	// Moderator synthesis
	moderator := cfg.Moderator
	if moderator == "" {
		moderator = panel[0].Model
	}
	var transcript strings.Builder
	for i, a := range panel {
		fmt.Fprintf(&transcript, "## %s (%s)\n%s\n\n", a.Name, a.Model, answers[i])
	}
	modSys := "You are the panel's moderator. Using only what the panelists actually said in the transcript below, write four short sections under these exact headings: Consensus — what they genuinely agree on. Disagreements — who splits from the group and why it matters. Recommendation — the single strongest answer the panel supports; commit to one, no hedging. Dissent worth keeping — one minority point that should survive, or omit this heading if there is none. Represent each panelist faithfully and add no position that no one took. Plain prose, under 400 words."
	synthesis, err := chatViaProxy(addr, moderator, modSys, q+"\n\nPanel transcript:\n"+transcript.String(), "", cfg.MaxTokens, acc)
	if err != nil {
		synthesis = "(moderator unavailable: " + err.Error() + ")"
	}

	var out strings.Builder
	out.WriteString("# Round Table\n\n" + compressNote + synthesis + "\n\n---\n\n# Panel positions\n\n" + transcript.String())

	// record the deliberation so the WebUI can show round-table history
	names2 := make([]string, len(panel))
	for i, a := range panel {
		names2[i] = a.Name
	}
	qsnip := question
	if len(qsnip) > 500 {
		qsnip = qsnip[:500] + "…"
	}
	acc.mu.Lock()
	pt, ct := acc.pt, acc.ct
	acc.mu.Unlock()
	s.AddRoundtableLog(&store.RoundtableLog{
		TS: start.UnixMilli(), Question: qsnip, Profiles: strings.Join(names2, ", "),
		Rounds: rounds, Compressed: compressed, Moderator: moderator,
		LatencyMS: time.Since(start).Milliseconds(), PromptTokens: pt, CompletionTokens: ct,
		Output: out.String(),
	})
	return out.String(), nil
}

func cmdMCP(args []string) {
	data := defaultDataDir()
	addr := strings.TrimRight(defaultAddr(), "/")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data":
			if i+1 < len(args) {
				data = args[i+1]
				i++
			}
		case "--addr":
			if i+1 < len(args) {
				addr = strings.TrimRight(args[i+1], "/")
				i++
			}
		}
	}
	s := openStore(data)
	defer s.Close()

	enc := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil { // notification
			continue
		}
		switch req.Method {
		case "initialize":
			enc.Encode(rpcResult(req.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "cfrproxy-roundtable", "version": "1.0.0"},
			}))
		case "ping":
			enc.Encode(rpcResult(req.ID, map[string]any{}))
		case "tools/list":
			enc.Encode(rpcResult(req.ID, map[string]any{"tools": mcpTools}))
		case "tools/call":
			var p struct {
				Name string `json:"name"`
				Args struct {
					Question string   `json:"question"`
					Context  string   `json:"context"`
					Profiles []string `json:"profiles"`
					Rounds   int      `json:"rounds"`
					Profile  string   `json:"profile"`
				} `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			text, err := func() (string, error) {
				switch p.Name {
				case "roundtable":
					return runRoundtable(s, addr, p.Args.Question, p.Args.Context, p.Args.Profiles, p.Args.Rounds)
				case "consult":
					profiles, err := s.AgentProfiles()
					if err != nil {
						return "", err
					}
					sel := selectProfiles(profiles, []string{p.Args.Profile})
					if len(sel) == 0 {
						return "", fmt.Errorf("profile %q not found", p.Args.Profile)
					}
					a := sel[0]
					q := p.Args.Question
					if p.Args.Context != "" {
						q = "Context:\n" + p.Args.Context + "\n\nQuestion:\n" + q
					}
					return chatViaProxy(addr, a.Model, a.Persona, q, a.Temperature, loadRTConfig(s).MaxTokens, nil)
				case "list_profiles":
					profiles, err := s.AgentProfiles()
					if err != nil {
						return "", err
					}
					var b strings.Builder
					for _, a := range profiles {
						state := "enabled"
						if !a.Enabled {
							state = "disabled"
						}
						fmt.Fprintf(&b, "- %s → %s (%s)\n", a.Name, a.Model, state)
					}
					if b.Len() == 0 {
						return "no agent profiles configured (WebUI → Agents tab)", nil
					}
					return b.String(), nil
				default:
					return "", fmt.Errorf("unknown tool %q", p.Name)
				}
			}()
			if err != nil {
				enc.Encode(rpcResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "Error: " + err.Error()}},
					"isError": true,
				}))
			} else {
				enc.Encode(rpcResult(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				}))
			}
		default:
			enc.Encode(rpcError(req.ID, -32601, "method not found: "+req.Method))
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
