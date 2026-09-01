package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

// Static-prefix capture, for warmup.
//
// Measured on fred (cfrproxy traces, 2026-08-19): of 101 requests over 2000
// prompt tokens, the 11 that missed the prefix cache entirely accounted for
// 395,094 of 587,585 prefill tokens — 67% of ALL prefill work — and their TTFB
// averaged 9,590 ms against 1,278 ms for a warm request. Established sessions
// already reuse their prefix well; it is the FIRST request of a session, and
// the first after a model reload, that pays full price.
//
// A warmup service can eliminate that, but only if it knows what to warm. The
// proxy is the one component that sees every prompt, so it records the static
// portion — system prompt plus tool schemas, the part that is byte-identical
// across every turn of a session — into a content-addressed manifest. The
// warmup daemon replays those manifests to keep the prefix resident.
//
// Only prefixes served by a local upstream are recorded. llama.cpp identifies
// itself through timings; fred is also local when its backend is vLLM, whose
// OpenAI responses do not expose cache timings.

const (
	// Below this, warming costs more than it saves: the KVarN cache retains an
	// intrinsic 128-token exact suffix and will not restore a shorter prefix at
	// all (observed reason "no_common_prefix" for a 37-token prompt).
	minPrefixBytes = 4000
	// Guard against pathological prompts filling the namespace.
	maxPrefixBytes = 4 << 20
)

// prefixSnapshot is the static head of a request: everything that renders
// before the first conversation turn and therefore stays byte-identical as the
// conversation grows.
type prefixSnapshot struct {
	provider string
	model    string
	system   string
	tools    []wire.Tool
}

// snapshotPrefix captures the static head of the request actually sent
// upstream — after docs/skill injection, so the manifest matches the bytes the
// model saw rather than the bytes the client sent.
func snapshotPrefix(creq *wire.Request, provName string) *prefixSnapshot {
	return &prefixSnapshot{provider: provName, model: creq.Model,
		system: creq.System, tools: creq.Tools}
}

// canonicalTools renders tool schemas deterministically. Ordering matters: a
// reordered tools array is a different prefix to the model, so a fingerprint
// that ignored order would collide and a warmup would prime the wrong bytes.
// The array order is preserved verbatim (it is what the model sees); only the
// JSON encoding is normalised.
func canonicalTools(tools []wire.Tool) []byte {
	if len(tools) == 0 {
		return []byte("[]")
	}
	type ct struct {
		Name   string          `json:"name"`
		Desc   string          `json:"description"`
		Params json.RawMessage `json:"parameters"`
	}
	out := make([]ct, 0, len(tools))
	for _, t := range tools {
		// Re-marshal the schema through a map so key order is Go's stable
		// alphabetical form — the same normalisation transform.Apply already
		// performs on the outbound body, so the fingerprint matches the wire.
		params := t.Params
		var m any
		if len(params) > 0 && json.Unmarshal(params, &m) == nil {
			if b, err := json.Marshal(m); err == nil {
				params = b
			}
		}
		out = append(out, ct{Name: t.Name, Desc: t.Description, Params: params})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// staticPrefixSHAs hashes the two halves of a request's static head. It is the
// single definition of "what a prefix IS" in this package: the manifest
// fingerprint is built from it, and so is the pool's prefix-affinity key, so
// the prefix a warmup replays and the prefix a route sticks to can never drift
// apart.
func staticPrefixSHAs(system string, tools []wire.Tool) (sysSHA, toolsSHA string) {
	return sha256hex([]byte(system)), sha256hex(canonicalTools(tools))
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// modelKey identifies the weights a prefix was warmed against. A KV cache is
// only valid for the exact model that produced it, so the model is the top
// level of the namespace.
func modelKey(provider, model string) string {
	return sha256hex([]byte(provider + "\x00" + model))[:16]
}

// cwdRe finds a working-directory declaration in a system prompt. Claude Code
// and Hermes both state one, and it is the cheapest reliable signal that a
// prefix is project-scoped rather than global.
var cwdRe = regexp.MustCompile(`(?i)(?:working directory|cwd|current working directory)\s*:?\s*(/[^\s"',)]+)`)

// scopeFor buckets a prefix into the cache namespace. "projects" when the
// prompt is anchored to a filesystem path, "global" otherwise. Session-level
// (L3) reuse is not represented here: it is owned by the inference server's own
// slot and RAM prompt cache, not by anything the proxy can replay.
func scopeFor(system string) (scope, key string) {
	if m := cwdRe.FindStringSubmatch(system); len(m) == 2 {
		return "projects", sha256hex([]byte(filepath.Clean(m[1])))[:12]
	}
	return "global", ""
}

// prefixManifest is what the warmup daemon consumes.
type prefixManifest struct {
	Schema   int    `json:"schema"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Client   string `json:"client"`
	Scope    string `json:"scope"`
	ScopeKey string `json:"scope_key,omitempty"`
	// Fingerprint covers provider, model, system text and canonical tools —
	// content only. Nothing volatile (no timestamps, no request ids, no
	// session ids) participates, so the same agent yields the same id forever.
	Fingerprint string          `json:"fingerprint"`
	SystemSHA   string          `json:"system_sha256"`
	ToolsSHA    string          `json:"tools_sha256"`
	System      string          `json:"system"`
	Tools       json.RawMessage `json:"tools"`
	SystemBytes int             `json:"system_bytes"`
	ToolCount   int             `json:"tool_count"`
	FirstSeen   string          `json:"first_seen"`
	LastSeen    string          `json:"last_seen"`
	// Observed cost of missing this prefix, straight from the upstream's own
	// timings — what the warmup is buying.
	LastPromptTokens int    `json:"last_prompt_tokens"`
	LastCacheSource  string `json:"last_cache_source"`
	LastCacheReason  string `json:"last_cache_reason"`
}

// prefixCacheRoot is the namespace root: <root>/<model-key>/<client>/<scope>/.
func prefixCacheRoot() string {
	if v := strings.TrimSpace(os.Getenv("CFRPROXY_PREFIX_CACHE")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cfrproxy", "cache")
}

// recordPrefix writes (or refreshes) the manifest for this request's static
// prefix. Best-effort and cheap on the hot path: an already-known prefix costs
// one Chtimes call, which is also what gives the daemon its LRU ordering.
func recordPrefix(snap *prefixSnapshot, tr *store.Trace) {
	if snap == nil || tr.Status != 200 {
		return
	}
	// Only local upstreams — the ones whose KV cache is ours. vLLM omits the
	// llama.cpp timings fields, so the explicitly local fred provider is enough
	// evidence; remote providers still need timings and remain excluded.
	// Stock llama.cpp exposes prompt timing and cached-token counts but not the
	// KVarN-specific cache_source/cache_reason fields. PromptMS is still a
	// reliable local-server signature (remote providers do not emit it).
	if snap.provider != "fred" && tr.CacheReason == "" && tr.CacheSource == "" && tr.PromptMS == 0 {
		return
	}
	// The warmup daemon replays these manifests; recording its own traffic
	// would refresh every entry's mtime on every pass and freeze the LRU.
	if tr.Client == warmupClient {
		return
	}
	toolsJSON := canonicalTools(snap.tools)
	if len(snap.system)+len(toolsJSON) < minPrefixBytes {
		return
	}
	if len(snap.system) > maxPrefixBytes {
		return
	}
	root := prefixCacheRoot()
	if root == "" || root == "off" {
		return
	}

	sysSHA, toolsSHA := staticPrefixSHAs(snap.system, snap.tools)
	fp := sha256hex([]byte(snap.provider + "\x00" + snap.model + "\x00" + sysSHA + "\x00" + toolsSHA))

	client := harnessClient(tr.Client, snap.system)
	if client == "" {
		client = "unknown"
	}
	scope, scopeKey := scopeFor(snap.system)
	dir := filepath.Join(root, modelKey(snap.provider, snap.model), sanitizeSeg(client), scope)
	path := filepath.Join(dir, fp[:24]+".json")

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := os.Stat(path); err == nil {
		// Known prefix: refresh mtime for LRU and move on. Rewriting a
		// multi-hundred-KB manifest on every turn would be pure write
		// amplification for data that has not changed.
		os.Chtimes(path, time.Now(), time.Now())
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	m := prefixManifest{
		Schema: 1, Provider: snap.provider, Model: snap.model, Client: client,
		Scope: scope, ScopeKey: scopeKey, Fingerprint: fp,
		SystemSHA: sysSHA, ToolsSHA: toolsSHA,
		System: snap.system, Tools: toolsJSON,
		SystemBytes: len(snap.system), ToolCount: len(snap.tools),
		FirstSeen: now, LastSeen: now,
		LastPromptTokens: tr.PromptTokens,
		LastCacheSource:  tr.CacheSource, LastCacheReason: tr.CacheReason,
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return
	}
	// atomic: the daemon may be reading this directory concurrently
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

func harnessClient(client, system string) string {
	head := system
	if len(head) > 8192 {
		head = head[:8192]
	}
	head = strings.ToLower(head)
	switch {
	case strings.Contains(head, "oh my pi coding harness"):
		return "omp"
	case strings.Contains(head, "you are codex"):
		return "codex"
	default:
		return client
	}
}

var unsafeSeg = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeSeg(s string) string {
	s = unsafeSeg.ReplaceAllString(s, "-")
	if s == "" {
		return "unknown"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
