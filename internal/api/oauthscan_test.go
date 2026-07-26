package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/proxy"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIPROXY_CONFIG", p)
}

// The data-plane key is a top-level list. An indented key of the same name
// under another section must not be mistaken for it.
func TestKeyFromCLIProxyConfig(t *testing.T) {
	writeConfig(t, `host: ''
port: 8317
remote-management:
  allow-remote: true
  secret-key: $2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR
auth-dir: ~/.cli-proxy-api
api-keys:
- sk-first-key
- sk-second-key
debug: false
`)
	if got := keyFromCLIProxyConfig(); got != "sk-first-key" {
		t.Errorf("api-key = %q, want sk-first-key", got)
	}
}

func TestKeyFromCLIProxyConfigIgnoresNested(t *testing.T) {
	writeConfig(t, `providers:
  some-vendor:
    api-keys:
    - nested-must-not-win
api-keys:
- sk-real
`)
	if got := keyFromCLIProxyConfig(); got != "sk-real" {
		t.Errorf("nested api-keys leaked through: got %q", got)
	}
}

func TestKeyFromCLIProxyConfigInline(t *testing.T) {
	writeConfig(t, "api-keys: sk-inline\n")
	if got := keyFromCLIProxyConfig(); got != "sk-inline" {
		t.Errorf("inline form = %q", got)
	}
}

// CLIProxyAPI stores the management secret bcrypt-hashed. Handing that digest
// back as a bearer token produces an unexplained 401, so it must be treated as
// absent — and the hint must say why rather than "not found".
func TestMgmtSecretHashedIsRejected(t *testing.T) {
	writeConfig(t, `remote-management:
  allow-remote: true
  secret-key: $2a$10$abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQR
`)
	if got := mgmtKeyFromCLIProxyConfig(); got != "" {
		t.Errorf("bcrypt digest was returned as a usable key: %q", got)
	}
	if rawMgmtSecretFromConfig() == "" {
		t.Error("raw read should still see the value so the hint can explain it")
	}
	hint := MgmtKeyHint()
	if !strings.Contains(hint, "bcrypt") {
		t.Errorf("hint should explain the hash, got: %s", hint)
	}
}

func TestMgmtSecretPlaintextIsUsed(t *testing.T) {
	writeConfig(t, `remote-management:
  secret-key: plain-secret
`)
	if got := mgmtKeyFromCLIProxyConfig(); got != "plain-secret" {
		t.Errorf("plaintext secret = %q", got)
	}
}

func TestMgmtHintWhenAbsent(t *testing.T) {
	writeConfig(t, "port: 8317\n")
	if got := mgmtKeyFromCLIProxyConfig(); got != "" {
		t.Errorf("expected no key, got %q", got)
	}
	if hint := MgmtKeyHint(); strings.Contains(hint, "bcrypt") {
		t.Errorf("absent key should not claim it is hashed: %s", hint)
	}
}

func TestPickDefault(t *testing.T) {
	catalog := []string{"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5"}
	if got := pickDefault([]string{"claude-sonnet-5", "claude-opus-4-8"}, catalog); got != "claude-sonnet-5" {
		t.Errorf("preferred order ignored: %q", got)
	}
	// preference absent -> first catalog entry
	if got := pickDefault([]string{"nope"}, catalog); got != "claude-opus-4-8" {
		t.Errorf("fallback = %q", got)
	}
	if got := pickDefault([]string{"x"}, nil); got != "" {
		t.Errorf("empty catalog should give empty default, got %q", got)
	}
}

// The claude preset must admit the genuine families and reject the alias forks
// CLIProxyAPI mints (claude-command-*, claude-opencode-*, claude-gpt-*, and the
// machine-specific ones an exclusion list could never enumerate).
func TestClaudePresetFilterExcludesAliasForks(t *testing.T) {
	var filter string
	for _, p := range oauthPresets {
		if p.Auth == "claude" {
			filter = p.Filter
		}
	}
	if filter == "" {
		t.Fatal("no claude preset")
	}
	real := []string{"claude-opus-4-8", "claude-sonnet-5", "claude-haiku-4-5-20251001", "claude-fable-5", "claude-3-5-haiku-20241022"}
	forks := []string{"claude-command-fable-5", "claude-opencode-sonnet-5", "claude-gpt-5.6-luna", "claude-gemini-3-flash", "claude-myhost-agents-a1", "claude-novita-thing", "claude-somethingnew-x"}

	got := proxy.ApplyModelsFilter(append(append([]string{}, real...), forks...), filter)
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, m := range real {
		if !set[m] {
			t.Errorf("real Claude model %q was filtered out", m)
		}
	}
	for _, m := range forks {
		if set[m] {
			t.Errorf("alias fork %q leaked into the claude provider", m)
		}
	}
}
