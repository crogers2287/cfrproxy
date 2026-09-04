package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// KV-Rosetta request-time restore.
//
// kvxd (the KV-Rosetta daemon) restores a saved prompt prefix into a llama.cpp
// slot only while that slot is idle AND empty. After a model's first request
// no slot is ever empty again — llama.cpp keeps the last conversation's cache
// in it — so on a busy fleet a NEW conversation never gets a restore:
// llama.cpp evicts an LRU slot and prefills the whole prompt cold (observed
// live: a 30,335-token first request with `cached: —`).
//
// The fix is request-time. When cfrproxy is about to forward a request that
// starts a new conversation on the local llama-swap provider, it first asks
// kvxd to restore the best matching attachment into the slot llama.cpp is
// about to evict anyway, then forwards. kvxd applies the upstream's chat
// template to the messages and tools exactly as they will be sent, tokenizes,
// finds the admitted attachment that is the longest prefix of that sequence,
// restores it into an idle slot and answers. It never wakes a model and never
// touches a busy slot.
//
// Off by default: the `kvx_restore` setting turns it on. A kvxd failure,
// timeout or connection refusal never fails the user request — it costs at
// most the configured timeout and is recorded on the trace note as
// `kvx→restored N (slot S)`, `kvx→miss: <reason>`, `kvx→timeout` or
// `kvx→error: <err>`.

const (
	kvxRestorePath           = "/v1/restore-for-prompt"
	kvxRestoreDefaultURL     = "http://127.0.0.1:8431"
	kvxRestoreDefaultTimeout = 3000 * time.Millisecond
	// kvxRestoreDefaultProvider is the local llama-swap provider. The same
	// name is what prefixcache.go treats as "local"; cloud providers must
	// never trigger a restore call.
	kvxRestoreDefaultProvider = "fred"
)

// KVXRestore is the `kvx_restore` setting.
type KVXRestore struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms"`
	// Provider names the local llama-swap provider whose requests may trigger
	// a restore. Empty means "fred".
	Provider string `json:"provider"`
}

func (c KVXRestore) timeout() time.Duration {
	if c.TimeoutMS <= 0 {
		return kvxRestoreDefaultTimeout
	}
	return time.Duration(c.TimeoutMS) * time.Millisecond
}

func (c KVXRestore) baseURL() string {
	if u := strings.TrimRight(strings.TrimSpace(c.URL), "/"); u != "" {
		return u
	}
	return kvxRestoreDefaultURL
}

func (c KVXRestore) provider() string {
	if c.Provider != "" {
		return c.Provider
	}
	return kvxRestoreDefaultProvider
}

// KVXRestoreConfig reads the `kvx_restore` setting. Unset or unparseable
// yields the zero value: disabled.
func (p *Proxy) KVXRestoreConfig() KVXRestore {
	var c KVXRestore
	if raw := p.Store.Setting("kvx_restore"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// kvxUnpooledWhy classifies a request to an UNPOOLED model the way routePool
// classifies a pooled one, so the restore gate has one notion of "a
// conversation this proxy already sent": "conversation" when the request's
// fingerprint (system prompt + first user turn, conversationFingerprint) is
// already bound to this model, "new" otherwise — and binds it, so the next
// turn is recognised. The binding lives in the pool affinity table under the
// pool's own conv key (poolConvKey), with the pool's TTL; a request with no
// fingerprint (nothing to pin) yields "".
func kvxUnpooledWhy(model string, req *wire.Request) string {
	key := poolConvKey(req)
	if key == "" {
		return ""
	}
	if m, _, ok := poolAffinity.get(key, poolAffinityTTL); ok && m == model {
		return "conversation"
	}
	poolAffinity.put(key, model, "kvx")
	return "new"
}

// kvxRestoreWanted decides whether this candidate send should be preceded by
// a restore call. All of these must hold:
//   - the setting is on,
//   - the candidate is the primary (not a failover hop, not a pool sibling)
//     and its provider is the local llama-swap one,
//   - the request starts a NEW conversation: `why` is the pool's `cold/*` or
//     `prefix` for a pooled model, or kvxUnpooledWhy's `new` for an unpooled
//     one. Never `conversation` (its slot is already warm), never a plain
//     least-busy pool's `least-busy` (no conversation notion there),
//   - the request carries a system prompt and/or tools — otherwise there is
//     nothing an attachment could cover.
func kvxRestoreWanted(cfg KVXRestore, primary store.Provider, c candidate, why string, system string, tools int) bool {
	if !cfg.Enabled || c.failover || c.sibling || c.prov.ID != primary.ID || c.prov.Name != cfg.provider() {
		return false
	}
	switch why {
	case "cold/slots", "cold/inflight", "prefix", "new":
	default:
		return false
	}
	return strings.TrimSpace(system) != "" || tools > 0
}

// kvxRestoreBody builds the restore-for-prompt request from the OpenAI body
// that is about to go upstream, so kvxd sees the messages and tools byte-for-
// byte as llama.cpp will (docs/skills injected, caveman applied, transform
// rules run). Returns nil when the body carries no messages.
//
// The chat template's render depends on more than messages+tools: on
// Qwen3.8-Flash-Next, a request WITHOUT `reasoning_effort` renders a
// "Reasoning effort is set to xhigh…" system preamble that a request WITH it
// does not, and the two renders share only 3 tokens. cfrproxy sets
// reasoning_effort on every forwarded request (the thinking=… transform), so
// the captured attachments carry the no-preamble head — and a restore probe
// rendered without the field could never match them (eight consecutive live
// misses, 2026-09-04). Every template-affecting top-level field is therefore
// forwarded verbatim; generation params (max_tokens, temperature, stream, …)
// are not, since they never reach the template.
func kvxRestoreBody(model string, outBody []byte) []byte {
	var out map[string]json.RawMessage
	if json.Unmarshal(outBody, &out) != nil || isJSONNull(out["messages"]) {
		return nil
	}
	req := map[string]json.RawMessage{"messages": out["messages"]}
	if m, err := json.Marshal(model); err == nil {
		req["model"] = m
	}
	for _, k := range kvxTemplateFields {
		if v, ok := out[k]; ok && !isJSONNull(v) {
			req[k] = v
		}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	return b
}

// kvxTemplateFields are the top-level request fields, besides messages, that
// llama.cpp's /apply-template consumes. kvxd forwards them to the upstream
// so the probe renders exactly as the captured request did.
var kvxTemplateFields = []string{"tools", "reasoning_effort", "chat_template_kwargs", "enable_thinking", "reasoning_format", "thinking"}

func isJSONNull(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) == 0 || bytes.Equal(v, []byte("null"))
}

// kvxRestoreClient is separate from the upstream client: the per-call
// context deadline is the only timeout, and the upstream transport's pooling
// settings are irrelevant for a loopback daemon.
var kvxRestoreClient = &http.Client{}

// kvxRestore calls kvxd synchronously and returns the trace note. It never
// returns an error: every failure mode is a note, and the caller forwards the
// request regardless.
func kvxRestore(ctx context.Context, cfg KVXRestore, model string, outBody []byte) string {
	body := kvxRestoreBody(model, outBody)
	if body == nil {
		return "kvx→miss: no messages in outbound body"
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.baseURL()+kvxRestorePath, bytes.NewReader(body))
	if err != nil {
		return "kvx→error: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := kvxRestoreClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
			return "kvx→timeout"
		}
		return "kvx→error: " + trimErr(err.Error())
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "kvx→timeout"
		}
		return "kvx→error: " + trimErr(err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("kvx→error: HTTP %d %s", resp.StatusCode, trimErr(string(rb)))
	}
	var ans struct {
		Restored     bool   `json:"restored"`
		CoversTokens int    `json:"covers_tokens"`
		Slot         int    `json:"slot"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(rb, &ans); err != nil {
		return "kvx→error: bad answer: " + trimErr(err.Error())
	}
	if !ans.Restored {
		reason := strings.TrimSpace(ans.Reason)
		if reason == "" {
			reason = "no reason given"
		}
		return "kvx→miss: " + trimErr(reason)
	}
	return fmt.Sprintf("kvx→restored %s (slot %d)", commaInt(ans.CoversTokens), ans.Slot)
}

// trimErr keeps a note readable: one line, bounded length.
func trimErr(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// commaInt renders 29601 as "29,601", matching the pool note's style.
func commaInt(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
