package api

// OAuth auto-discovery. A fresh install already has logins sitting in
// CLIProxyAPI (from `cfrproxy login …`, or from the user's existing codex /
// claude / grok / antigravity sessions). Before this, turning those into
// cfrproxy providers was manual: one provider per OAuth backend, all pointing
// at the same CLIProxyAPI base URL, separated by a models_filter. Getting the
// filter wrong silently mixes model families across providers.
//
// ScanOAuth reads the accounts CLIProxyAPI actually holds and registers the
// matching providers, so a new machine goes from "logged in" to "routable"
// without hand-editing anything.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

// oauthPreset maps a CLIProxyAPI auth backend to the cfrproxy provider that
// should front it.
//
// The filter is an allow-list, and it matters: every one of these providers
// shares a single base URL, so without it a request for any model would be
// servable by any of them (that is REQ-024's "providers on a shared backend
// don't leak").
//
// Claude names its real families explicitly rather than excluding the alias
// ones. CLIProxyAPI's oauth-model-alias config mints forks like
// claude-gpt-5.6-sol / claude-command-* / claude-opencode-*, and every machine
// configures a different set — an exclusion list would silently absorb any
// family we hadn't heard of. Allow-listing opus/sonnet/haiku/fable/3-x/4-x
// cannot be broken that way.
type oauthPreset struct {
	Auth   string   // "provider" field reported by /v0/management/auth-files
	Name   string   // cfrproxy provider name to create
	Filter string   // models_filter allow-list
	Prefer []string // preferred default model, first one present wins
}

var oauthPresets = []oauthPreset{
	{"claude", "claude", "claude-opus-*,claude-sonnet-*,claude-haiku-*,claude-fable-*,claude-3-*,claude-4-*", []string{"claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5"}},
	{"anthropic", "claude", "claude-opus-*,claude-sonnet-*,claude-haiku-*,claude-fable-*,claude-3-*,claude-4-*", []string{"claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5"}},
	{"codex", "codex", "gpt-*,codex-*", []string{"gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"}},
	{"xai", "grok", "grok-*", []string{"grok-4.5", "grok-4"}},
	{"antigravity", "gemini", "gemini-*", []string{"gemini-3-flash", "gemini-3.1-flash"}},
	{"gemini", "gemini", "gemini-*", []string{"gemini-3-flash", "gemini-3.1-flash"}},
	{"kimi", "kimi", "kimi-*", []string{"kimi-k2"}},
}

// OAuthScanResult is one line of the scan report.
type OAuthScanResult struct {
	Auth     string `json:"auth"`     // CLIProxyAPI backend
	Account  string `json:"account"`  // which login
	Provider string `json:"provider"` // cfrproxy provider name
	Action   string `json:"action"`   // created | exists | skipped
	Detail   string `json:"detail"`
	Models   int    `json:"models"`
	Default  string `json:"default_model"`
}

type authFile struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Label    string `json:"label"`
	Disabled bool   `json:"disabled"`
}

// authFiles asks CLIProxyAPI which OAuth accounts it holds.
func (a *API) authFiles() ([]authFile, error) {
	code, body, err := a.mgmt("GET", "/auth-files", nil, "")
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("CLIProxyAPI management API returned HTTP %d — check cliproxy_mgmt_key", code)
	}
	var out struct {
		Files []authFile `json:"files"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// cliproxyAPIKey finds the data-plane key CLIProxyAPI expects on /v1 calls.
// Order: an explicit override, then a key already stored on a provider that
// points at the same base (so a re-scan stays consistent), then the first entry
// of api-keys in CLIProxyAPI's own config.yaml.
func (a *API) cliproxyAPIKey(explicit, base string) (string, string) {
	if explicit != "" {
		return explicit, "supplied"
	}
	for _, p := range a.Store.Providers() {
		if sameHost(p.BaseURL, base) && p.APIKey != "" {
			return p.APIKey, "reused from provider " + p.Name
		}
	}
	if k := keyFromCLIProxyConfig(); k != "" {
		return k, "read from CLIProxyAPI config.yaml"
	}
	return "", "not found"
}

func sameHost(a, b string) bool {
	trim := func(s string) string {
		s = strings.TrimRight(s, "/")
		return strings.TrimSuffix(s, "/v1")
	}
	return strings.EqualFold(trim(a), trim(b))
}

// cliproxyConfigPath is CLIProxyAPI's own config file.
func cliproxyConfigPath() string {
	if p := os.Getenv("CLIPROXY_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cli-proxy-api", "config.yaml")
}

// keyFromCLIProxyConfig pulls the first api-keys entry out of CLIProxyAPI's
// config.yaml — the data-plane key its /v1 endpoints require.
//
//	api-keys:
//	- sk-...
//
// Deliberately a small hand parser rather than a YAML dependency: two known
// fields in a file we don't own is not worth a new module in go.mod.
func keyFromCLIProxyConfig() string {
	f, err := os.Open(cliproxyConfigPath())
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inList := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inList {
			if strings.HasPrefix(trimmed, "- ") {
				if k := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), `"'`); k != "" {
					return k
				}
				continue
			}
			inList = false // any non-item line ends the list
		}
		// only a TOP-LEVEL "api-keys:" counts — an indented key of the same
		// name under some other section must not be picked up
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.HasPrefix(trimmed, "api-keys:") {
			if rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "api-keys:")); rest != "" && rest != "[]" {
				return strings.Trim(rest, `"'`)
			}
			inList = true
		}
	}
	return ""
}

// looksHashed reports whether a config value is a bcrypt digest rather than a
// usable secret. CLIProxyAPI stores remote-management.secret-key HASHED, so the
// plaintext is not recoverable from config.yaml — sending the digest as a
// bearer token just yields a baffling 401. Detecting it lets us say so.
func looksHashed(v string) bool {
	return strings.HasPrefix(v, "$2a$") || strings.HasPrefix(v, "$2b$") || strings.HasPrefix(v, "$2y$")
}

// MgmtKeyHint explains how to get the management key, distinguishing "the
// config doesn't mention it" from the far more common "it's there but stored as
// a bcrypt digest, so only you know the plaintext".
func MgmtKeyHint() string {
	if rawMgmtSecretFromConfig() != "" {
		return "CLIProxyAPI stores remote-management.secret-key as a bcrypt hash in " +
			cliproxyConfigPath() + ", so the plaintext can't be read back. Use the secret you set when " +
			"you configured remote management:\n  cfrproxy config set cliproxy_mgmt_key <secret>"
	}
	return "no remote-management.secret-key in " + cliproxyConfigPath() +
		" — enable remote management in CLIProxyAPI, then:\n  cfrproxy config set cliproxy_mgmt_key <secret>"
}

// rawMgmtSecretFromConfig returns the secret-key value as written, hashed or
// not — used only to tell those two cases apart in MgmtKeyHint.
func rawMgmtSecretFromConfig() string {
	f, err := os.Open(cliproxyConfigPath())
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inSection := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inSection = strings.HasPrefix(trimmed, "remote-management:")
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "secret-key:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "secret-key:")), `"'`)
		}
	}
	return ""
}

// mgmtKeyFromCLIProxyConfig pulls remote-management.secret-key, the management
// API's bearer token:
//
//	remote-management:
//	  secret-key: ...
//
// Returns "" when the value is hashed (the common case) — see looksHashed.
func mgmtKeyFromCLIProxyConfig() string {
	f, err := os.Open(cliproxyConfigPath())
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inSection := false
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if !indented {
			inSection = strings.HasPrefix(trimmed, "remote-management:")
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "secret-key:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "secret-key:")), `"'`)
			if looksHashed(v) {
				return "" // digest, not a usable token
			}
			return v
		}
	}
	return ""
}

// ScanOAuth discovers CLIProxyAPI's OAuth accounts and reports (apply=false) or
// creates (apply=true) the cfrproxy provider for each. Providers that already
// exist are left completely alone — a re-scan never overwrites tuning.
func (a *API) ScanOAuth(ctx context.Context, apply bool, explicitKey string) ([]OAuthScanResult, string, error) {
	files, err := a.authFiles()
	if err != nil {
		return nil, "", err
	}
	base := a.mgmtURL() + "/v1"
	key, keySrc := a.cliproxyAPIKey(explicitKey, base)

	// live catalog, used to pick a default model that actually exists
	var catalog []string
	if key != "" && a.Proxy != nil {
		catalog, _ = a.Proxy.ListModels(ctx, store.Provider{Type: "openai", BaseURL: base, APIKey: key})
	}

	existing := map[string]bool{}
	for _, p := range a.Store.Providers() {
		existing[strings.ToLower(p.Name)] = true
	}

	var out []OAuthScanResult
	done := map[string]bool{} // one provider per name even if several logins map to it
	for _, f := range files {
		for _, pre := range oauthPresets {
			if !strings.EqualFold(pre.Auth, f.Provider) || done[pre.Name] {
				continue
			}
			done[pre.Name] = true
			acct := f.Account
			if acct == "" {
				acct = f.Label
			}
			r := OAuthScanResult{Auth: f.Provider, Account: acct, Provider: pre.Name}
			switch {
			case f.Disabled:
				r.Action, r.Detail = "skipped", "account disabled in CLIProxyAPI"
			case existing[strings.ToLower(pre.Name)]:
				r.Action, r.Detail = "exists", "left unchanged"
			case key == "":
				r.Action, r.Detail = "skipped", "no CLIProxyAPI api-key found ("+keySrc+") — pass one explicitly"
			default:
				matched := proxy.ApplyModelsFilter(catalog, pre.Filter)
				def := pickDefault(pre.Prefer, matched)
				r.Models, r.Default = len(matched), def
				if apply {
					p := store.Provider{
						Name:         pre.Name,
						Type:         "openai",
						BaseURL:      base,
						APIKey:       key,
						DefaultModel: def,
						ModelsFilter: pre.Filter,
						Enabled:      true,
					}
					if err := a.Store.SaveProvider(&p); err != nil {
						r.Action, r.Detail = "skipped", "save failed: "+err.Error()
					} else {
						r.Action, r.Detail = "created", "filter "+pre.Filter
					}
				} else {
					r.Action, r.Detail = "would create", "filter "+pre.Filter
				}
			}
			out = append(out, r)
		}
	}
	return out, keySrc, nil
}

// pickDefault returns the first preferred model present in the catalog, else
// the first catalog entry. An empty result is fine — the provider still routes
// explicitly-addressed models.
func pickDefault(prefer, catalog []string) string {
	for _, want := range prefer {
		for _, m := range catalog {
			if strings.EqualFold(m, want) {
				return m
			}
		}
	}
	if len(catalog) > 0 {
		return catalog[0]
	}
	return ""
}
