package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/wire"
)

func bigSystem(tag string) string {
	return "You are an agent.\n" + strings.Repeat("Reference paragraph "+tag+". ", 400)
}

func llamaTrace() *store.Trace {
	return &store.Trace{Status: 200, Client: "claude-code", CacheSource: "ram",
		CacheReason: "committed", PromptTokens: 16059}
}

// The fingerprint must be a pure function of content. Two requests a day apart,
// from different sessions, must land on the SAME manifest — otherwise the
// warmup daemon primes a prefix nobody will ask for again.
func TestPrefixFingerprintIsStableAndContentOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)

	sys := bigSystem("a")
	tools := []wire.Tool{{Name: "read", Description: "read a file",
		Params: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}}
	snap := &prefixSnapshot{provider: "fred", model: "huihui", system: sys, tools: tools}

	recordPrefix(snap, llamaTrace())
	first := listManifests(t, root)
	if len(first) != 1 {
		t.Fatalf("expected 1 manifest, got %d: %v", len(first), first)
	}

	// same content, different trace/session → same file, no second manifest
	recordPrefix(snap, llamaTrace())
	if got := listManifests(t, root); len(got) != 1 {
		t.Fatalf("content-identical prefix created %d manifests", len(got))
	}

	// different system text → distinct manifest
	recordPrefix(&prefixSnapshot{provider: "fred", model: "huihui",
		system: bigSystem("b"), tools: tools}, llamaTrace())
	if got := listManifests(t, root); len(got) != 2 {
		t.Fatalf("changed system did not fork the manifest: %d", len(got))
	}
}

// Tool ORDER is part of the prompt the model sees, so it must be part of the
// identity. A fingerprint that sorted tools would warm the wrong byte sequence.
func TestPrefixFingerprintTracksToolOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)

	a := wire.Tool{Name: "alpha", Description: "a", Params: json.RawMessage(`{"type":"object"}`)}
	b := wire.Tool{Name: "beta", Description: "b", Params: json.RawMessage(`{"type":"object"}`)}
	sys := bigSystem("t")

	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: sys,
		tools: []wire.Tool{a, b}}, llamaTrace())
	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: sys,
		tools: []wire.Tool{b, a}}, llamaTrace())

	if got := listManifests(t, root); len(got) != 2 {
		t.Fatalf("tool reorder must change identity, got %d manifests", len(got))
	}
}

// Property order inside a tool schema must NOT change identity: cfrproxy already
// re-sorts those keys alphabetically on the outbound body, so two clients that
// authored the same schema differently produce the same wire bytes.
func TestPrefixFingerprintIgnoresSchemaKeyOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)
	sys := bigSystem("k")

	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: sys,
		tools: []wire.Tool{{Name: "x", Params: json.RawMessage(`{"a":1,"b":2}`)}}}, llamaTrace())
	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: sys,
		tools: []wire.Tool{{Name: "x", Params: json.RawMessage(`{"b":2,"a":1}`)}}}, llamaTrace())

	if got := listManifests(t, root); len(got) != 1 {
		t.Fatalf("schema key order leaked into identity: %d manifests", len(got))
	}
}

// Non-llama.cpp upstreams have no KV cache we control; recording them would
// fill the namespace with prefixes the warmup can never usefully replay.
func TestPrefixSkipsNonLlamaCppAndTinyPrompts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)
	sys := bigSystem("s")

	// remote provider: no cache_reason/cache_source
	recordPrefix(&prefixSnapshot{provider: "grok", model: "grok-4.6", system: sys},
		&store.Trace{Status: 200, Client: "openai-sdk", PromptTokens: 40000})
	// llama.cpp but far too small to be worth warming
	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: "hi"}, llamaTrace())
	// non-200
	recordPrefix(&prefixSnapshot{provider: "fred", model: "m", system: sys},
		&store.Trace{Status: 500, CacheReason: "committed"})

	if got := listManifests(t, root); len(got) != 0 {
		t.Fatalf("recorded prefixes it should have skipped: %v", got)
	}
}

// Namespace layout is part of the contract with the warmup daemon.
func TestPrefixNamespaceLayout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)

	// a project-scoped prompt (declares a working directory)
	recordPrefix(&prefixSnapshot{provider: "fred", model: "huihui",
		system: "Working directory: /home/crogers2287/cfrproxy\n" + bigSystem("p")}, llamaTrace())
	// a global one
	recordPrefix(&prefixSnapshot{provider: "fred", model: "huihui",
		system: bigSystem("g")}, llamaTrace())

	var scopes []string
	for _, p := range listManifests(t, root) {
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 4 {
			t.Fatalf("expected <model>/<client>/<scope>/<fp>.json, got %q", rel)
		}
		if parts[1] != "claude-code" {
			t.Errorf("client segment = %q", parts[1])
		}
		scopes = append(scopes, parts[2])
	}
	joined := strings.Join(scopes, ",")
	if !strings.Contains(joined, "projects") || !strings.Contains(joined, "global") {
		t.Fatalf("scopes = %v, want both projects and global", scopes)
	}
}

// The manifest must carry enough to replay the prefix byte-for-byte.
func TestPrefixManifestIsReplayable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CFRPROXY_PREFIX_CACHE", root)
	sys := bigSystem("r")
	tools := []wire.Tool{{Name: "grep", Description: "search",
		Params: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}
	recordPrefix(&prefixSnapshot{provider: "fred", model: "huihui", system: sys, tools: tools}, llamaTrace())

	files := listManifests(t, root)
	if len(files) != 1 {
		t.Fatalf("manifests: %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var m prefixManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.System != sys {
		t.Error("system prompt not replayable verbatim")
	}
	if m.ToolCount != 1 || !strings.Contains(string(m.Tools), "grep") {
		t.Errorf("tools not replayable: %s", m.Tools)
	}
	if m.Provider != "fred" || m.Model != "huihui" || m.Client != "claude-code" {
		t.Errorf("routing info missing: %+v", m)
	}
	if m.LastPromptTokens != 16059 || m.LastCacheSource != "ram" {
		t.Errorf("observed-cost fields missing: %+v", m)
	}
	// no volatile value may participate in identity
	if strings.Contains(m.Fingerprint, m.FirstSeen) || len(m.Fingerprint) != 64 {
		t.Errorf("fingerprint looks wrong: %q", m.Fingerprint)
	}
}

func listManifests(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".json") {
			out = append(out, p)
		}
		return nil
	})
	return out
}
