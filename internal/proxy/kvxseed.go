package proxy

// KV-Rosetta seeding and dry-run probing (kv-rosetta REQ-113).
//
// The first conversation a harness opens on a model it has never served finds
// no attachment and pays the whole prefill; kvxd only captures a conversation
// AFTER it ran, and capture churn evicts it. Seeding makes the artifact exist
// first: cfrproxy renders the exact body it would forward for a recorded
// static prefix (system + tools, a one-token user turn) and kvxd prefills,
// captures, admits and PINS it. The dry-run probe is the same render + scan
// without touching a slot, so the smart router can know whether a local model
// would restore before choosing it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

const (
	kvxSeedPath        = "/v1/seed"
	kvxSeedTimeout     = 10 * time.Minute // one cold prefill of the prefix
	kvxProbeTimeout    = 2500 * time.Millisecond
	kvxProbeMinShared  = 1024 // below this a restore is not worth its own cost
	kvxSeedDefaultTurn = "seed"
)

// SeedPrefix is one recorded static prefix, ready to render.
type SeedPrefix struct {
	Client      string `json:"client"`
	Model       string `json:"model"` // the model it was recorded on
	Scope       string `json:"scope,omitempty"`
	Fingerprint string `json:"fingerprint"`
	LastSeen    string `json:"last_seen"`
	SystemBytes int    `json:"system_bytes"`
	ToolCount   int    `json:"tool_count"`
	Path        string `json:"path"`
	system      string
	tools       []wire.Tool
}

// billingHead matches Claude Code's version-stamped block when it was glued
// onto the front of a recorded system prompt (manifests written before
// isBillingHeader existed). The prefix a seed produces must be the one the
// proxy forwards NOW.
var billingHead = regexp.MustCompile(`^x-anthropic-billing-header:[^;]*;\s*[^;]*;\s*`)

// LoadSeedPrefixes reads the prefix manifests recordPrefix wrote, newest first,
// one per (client, system, tools). client "" = every harness.
func LoadSeedPrefixes(client string) ([]SeedPrefix, error) {
	root := prefixCacheRoot()
	if root == "" {
		return nil, fmt.Errorf("no prefix cache root")
	}
	var out []SeedPrefix
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") || strings.HasPrefix(d.Name(), "_") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var m prefixManifest
		if json.Unmarshal(b, &m) != nil || m.Client == "" || m.System == "" && len(m.Tools) <= 2 {
			return nil
		}
		if client != "" && !strings.EqualFold(m.Client, client) {
			return nil
		}
		system := billingHead.ReplaceAllString(m.System, "")
		key := m.Client + "\x00" + sha256hex([]byte(system)) + "\x00" + m.ToolsSHA
		if seen[key] {
			return nil
		}
		seen[key] = true
		var ct []struct {
			Name   string          `json:"name"`
			Desc   string          `json:"description"`
			Params json.RawMessage `json:"parameters"`
		}
		json.Unmarshal(m.Tools, &ct)
		sp := SeedPrefix{Client: m.Client, Model: m.Model, Scope: m.Scope, Fingerprint: m.Fingerprint,
			LastSeen: m.LastSeen, SystemBytes: len(system), ToolCount: len(ct), Path: path, system: system}
		for _, t := range ct {
			sp.tools = append(sp.tools, wire.Tool{Name: t.Name, Description: t.Desc, Params: t.Params})
		}
		out = append(out, sp)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out, err
}

// seedBody renders the body kvxd should seed for a prefix on a provider: the
// same outbound shape handleCore forwards (OpenAI dialect, provider thinking
// default applied) with a one-token user turn.
func (p *Proxy) seedBody(prov store.Provider, model string, sp SeedPrefix, userTurn string) ([]byte, error) {
	if userTurn == "" {
		userTurn = kvxSeedDefaultTurn
	}
	creq := &wire.Request{Model: model, System: sp.system, Tools: sp.tools, MaxTokens: 1,
		Messages: []wire.Msg{{Role: "user", Content: userTurn}}}
	out, err := buildOutbound("openai", creq)
	if err != nil {
		return nil, err
	}
	if lvl, force := reasoningFor(nil, prov); lvl != "" {
		if nb, changed := applyReasoning(out, "openai", lvl, force); changed {
			out = nb
		}
	}
	body := kvxRestoreBody(model, out)
	if body == nil {
		return nil, fmt.Errorf("prefix rendered to no messages")
	}
	var m map[string]json.RawMessage
	json.Unmarshal(body, &m)
	m["user_turn"] = mustJSON(userTurn)
	return json.Marshal(m)
}

// SeedResult is kvxd's answer for one prefix.
type SeedResult struct {
	Client  string             `json:"client"`
	Prefix  string             `json:"prefix"`
	Seeded  bool               `json:"seeded"`
	Already bool               `json:"already,omitempty"`
	Tokens  int                `json:"tokens,omitempty"`
	Slot    int                `json:"slot,omitempty"`
	Reason  string             `json:"reason,omitempty"`
	Stages  map[string]float64 `json:"stages,omitempty"`
	Seconds float64            `json:"seconds,omitempty"`
}

// KVXSeed seeds prefixes into one local model via kvxd. The model must be
// resident (kvxd never wakes one). Sequential: each seed owns an idle slot for
// the length of a prefill.
func (p *Proxy) KVXSeed(ctx context.Context, model string, prefixes []SeedPrefix, userTurn string) ([]SeedResult, error) {
	cfg := p.KVXRestoreConfig()
	prov, m, err := p.ResolveModel(ctx, model)
	if err != nil {
		return nil, err
	}
	// A pool name that is not itself a runtime cannot be seeded (kvxd holds
	// artifacts per runtime). A member that doubles as a pool key is fine.
	if spec := p.poolSpecFor(m); spec != nil && !memberOf(spec.Members, m) {
		return nil, fmt.Errorf("%q is a pool; seed a member (%s)", m, strings.Join(spec.Members, ", "))
	}
	var out []SeedResult
	for _, sp := range prefixes {
		res := SeedResult{Client: sp.Client, Prefix: sp.Fingerprint[:12]}
		body, err := p.seedBody(prov, m, sp, userTurn)
		if err != nil {
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, kvxSeedTimeout)
		rb, err := kvxPost(cctx, cfg.baseURL()+kvxSeedPath, body)
		cancel()
		if err != nil {
			res.Reason = err.Error()
			out = append(out, res)
			continue
		}
		var ans struct {
			Seeded  bool               `json:"seeded"`
			Already bool               `json:"already"`
			Tokens  int                `json:"tokens"`
			Slot    int                `json:"slot"`
			Reason  string             `json:"reason"`
			Stages  map[string]float64 `json:"stages"`
			Seconds float64            `json:"seconds"`
		}
		if err := json.Unmarshal(rb, &ans); err != nil {
			res.Reason = "bad answer: " + trimErr(err.Error())
		} else {
			res.Seeded, res.Already, res.Tokens, res.Slot, res.Reason, res.Stages, res.Seconds =
				ans.Seeded, ans.Already, ans.Tokens, ans.Slot, ans.Reason, ans.Stages, ans.Seconds
		}
		out = append(out, res)
	}
	return out, nil
}

func kvxPost(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := kvxRestoreClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, trimErr(string(rb)))
	}
	return rb, nil
}

// kvxWouldRestore asks kvxd, without touching a slot, how many tokens of this
// request's prompt an attachment for model already covers. 0 on any miss,
// error or when the feature is off. Used by the smart router before it
// commits a big new conversation to a local model.
func (p *Proxy) kvxWouldRestore(ctx context.Context, prov store.Provider, model string, req *wire.Request) int {
	cfg := p.KVXRestoreConfig()
	if !cfg.Enabled || req == nil || !strings.EqualFold(prov.Name, cfg.provider()) {
		return 0
	}
	out, err := buildOutbound("openai", req)
	if err != nil {
		return 0
	}
	if lvl, force := reasoningFor(nil, prov); lvl != "" {
		if nb, changed := applyReasoning(out, "openai", lvl, force); changed {
			out = nb
		}
	}
	body := kvxRestoreBody(model, out)
	if body == nil {
		return 0
	}
	var m map[string]json.RawMessage
	json.Unmarshal(body, &m)
	m["dry_run"] = json.RawMessage("true")
	body, _ = json.Marshal(m)
	cctx, cancel := context.WithTimeout(ctx, kvxProbeTimeout)
	defer cancel()
	rb, err := kvxPost(cctx, cfg.baseURL()+kvxRestorePath, body)
	if err != nil {
		return 0
	}
	var ans struct {
		WouldRestore bool `json:"would_restore"`
		Shared       int  `json:"shared_tokens"`
	}
	if json.Unmarshal(rb, &ans) != nil || !ans.WouldRestore || ans.Shared < kvxProbeMinShared {
		return 0
	}
	return ans.Shared
}
