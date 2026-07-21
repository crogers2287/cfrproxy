package main

// Harness launcher: `cfrproxy claude --model openrouter/anthropic/claude-sonnet-4` (or codex,
// opencode, omp, any binary on PATH) execs the harness with every
// conventional dialect env var pointed at the proxy, so its /model picker
// lists whatever the providers actually serve.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
)

func defaultAddr() string {
	if v := os.Getenv("CFRPROXY_ADDR"); v != "" {
		return v
	}
	return "http://127.0.0.1:8420"
}

func cmdLaunch(harness string, args []string) {
	bin, err := exec.LookPath(harness)
	if err != nil {
		fatal("harness %q not found on PATH", harness)
	}
	// consume --model/-m, --addr, --data; forward everything else verbatim
	model, addr, data := "", defaultAddr(), defaultDataDir()
	var fwd []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--model", "-m":
			if i+1 < len(args) {
				model = args[i+1]
				i++
			}
		case "--addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			}
		case "--data":
			if i+1 < len(args) {
				data = args[i+1]
				i++
			}
		default:
			fwd = append(fwd, args[i])
		}
	}
	addr = strings.TrimRight(addr, "/")

	// the harness is useless if the proxy is down — check first
	hc := &http.Client{Timeout: 3 * time.Second}
	if resp, err := hc.Get(addr + "/health"); err != nil {
		fatal("cfrproxy server not reachable at %s (%v)\nstart it with: systemctl --user start cfrproxy   or   cfrproxy serve", addr, err)
	} else {
		resp.Body.Close()
	}

	if model != "" {
		resolved, note := resolveLaunchModel(data, model)
		if note != "" {
			fmt.Fprintln(os.Stderr, note)
		}
		model = resolved
	}

	env := os.Environ()
	setenv := func(k, v string, always bool) {
		if !always && os.Getenv(k) != "" {
			return
		}
		env = append(env, k+"="+v)
	}
	setenv("ANTHROPIC_BASE_URL", addr, true)
	setenv("ANTHROPIC_AUTH_TOKEN", "cfrproxy", false)
	setenv("OPENAI_BASE_URL", addr+"/v1", true)
	setenv("OPENAI_API_KEY", "cfrproxy", false)
	setenv("OLLAMA_HOST", addr, true)
	if model != "" {
		setenv("ANTHROPIC_MODEL", model, true)
		setenv("ANTHROPIC_SMALL_FAST_MODEL", model, true)
		setenv("CFRPROXY_MODEL", model, true)
	}
	// harness-specific model flags where env alone doesn't set the default
	switch harness {
	case "codex":
		if model != "" {
			fwd = append([]string{"-m", model}, fwd...)
		}
	}

	fmt.Fprintf(os.Stderr, "cfrproxy → %s via %s", harness, addr)
	if model != "" {
		fmt.Fprintf(os.Stderr, "  model=%s", model)
	}
	fmt.Fprintln(os.Stderr)
	if err := syscall.Exec(bin, append([]string{harness}, fwd...), env); err != nil {
		fatal("exec %s: %v", bin, err)
	}
}

// resolveLaunchModel fuzzy-matches a model spec against the registry and the
// providers' live model lists. Returns the canonical provider/model string
// and an informational note.
func resolveLaunchModel(dataDir, spec string) (string, string) {
	s, err := store.Open(dataDir)
	if err != nil {
		return spec, ""
	}
	defer s.Close()
	p := proxy.New(s)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	provs := s.Providers()
	if i := strings.IndexByte(spec, '/'); i > 0 {
		name, rest := spec[:i], spec[i+1:]
		for _, prov := range provs {
			if !strings.EqualFold(prov.Name, name) {
				continue
			}
			if rest == "" {
				rest = prov.DefaultModel
			}
			if m, ok := proxy.FuzzyModel(p.ModelsCached(ctx, prov), rest); ok {
				return prov.Name + "/" + m, noteIfChanged(spec, prov.Name+"/"+m)
			}
			return prov.Name + "/" + rest, fmt.Sprintf("note: %q not in %s's live model list; passing through as typed", rest, prov.Name)
		}
		return spec, fmt.Sprintf("warning: no provider named %q; passing model through as typed", name)
	}
	// bare model: alias match wins, else search every enabled provider's scan
	for _, prov := range provs {
		for _, alias := range strings.Split(prov.Models, ",") {
			if strings.EqualFold(strings.TrimSpace(alias), spec) {
				return spec, ""
			}
		}
	}
	for _, prov := range provs {
		if !prov.Enabled {
			continue
		}
		if m, ok := proxy.FuzzyModel(p.ModelsCached(ctx, prov), spec); ok {
			full := prov.Name + "/" + m
			return full, noteIfChanged(spec, full)
		}
	}
	return spec, fmt.Sprintf("note: %q not found at any provider; passing through as typed", spec)
}

func noteIfChanged(typed, resolved string) string {
	if typed == resolved {
		return ""
	}
	return fmt.Sprintf("model %q resolved to %q", typed, resolved)
}

// cmdLogin proxies OAuth logins to CLIProxyAPI, which holds the device-code
// and browser flows for subscription providers (Codex, Claude, Antigravity,
// Kimi). New accounts land in its auth dir; models flow into cfrproxy via
// the registered "oauth" provider's live scan.
func cmdLogin(args []string) {
	bin := os.Getenv("CLIPROXY_BIN")
	if bin == "" {
		if p, err := exec.LookPath("cli-proxy-api"); err == nil {
			bin = p
		} else {
			bin = "cli-proxy-api" // rely on PATH; override with CLIPROXY_BIN
		}
	}
	cfg := os.Getenv("CLIPROXY_CONFIG")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = home + "/.cli-proxy-api/config.yaml"
	}
	flags := map[string]string{
		"codex":        "-codex-login",
		"codex-device": "-codex-device-login",
		"claude":       "-claude-login",
		"anthropic":    "-claude-login",
		"antigravity":  "-antigravity-login",
		"kimi":         "-kimi-login",
		"xai":          "-xai-login",
		"grok":         "-xai-login",
		"supergrok":    "-xai-login",
	}
	if len(args) < 1 {
		fmt.Println("usage: cfrproxy login codex|codex-device|claude|antigravity|kimi|supergrok [--no-browser]")
		fmt.Println("logins are handled by CLIProxyAPI; accounts stack, models appear under the 'oauth' provider")
		return
	}
	fl, ok := flags[args[0]]
	if !ok {
		fatal("unknown login target %q (want codex|codex-device|claude|antigravity|kimi|supergrok)", args[0])
	}
	if _, err := os.Stat(bin); err != nil {
		fatal("CLIProxyAPI binary not found at %s (set CLIPROXY_BIN)", bin)
	}
	argv := []string{bin, "--config", cfg, fl}
	for _, a := range args[1:] {
		if a == "--no-browser" {
			argv = append(argv, "-no-browser")
		}
	}
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		fatal("exec: %v", err)
	}
}

func cmdMap(args []string) {
	data := defaultDataDir()
	rm := ""
	var pos []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data":
			if i+1 < len(args) {
				data = args[i+1]
				i++
			}
		case "--rm":
			if i+1 < len(args) {
				rm = args[i+1]
				i++
			}
		default:
			pos = append(pos, args[i])
		}
	}
	s := openStore(data)
	defer s.Close()
	m := s.ModelMap()
	switch {
	case rm != "":
		if _, ok := m[rm]; !ok {
			fatal("no map entry %q", rm)
		}
		delete(m, rm)
		if err := s.SetModelMap(m); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed %s\n", rm)
	case len(pos) == 2:
		m[pos[0]] = pos[1]
		if err := s.SetModelMap(m); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%s → %s\n", pos[0], pos[1])
	case len(pos) == 0:
		if len(m) == 0 {
			fmt.Println("no model map entries (patterns: exact name or trailing-*, e.g. 'claude-sonnet*')")
			return
		}
		for k, v := range m {
			fmt.Printf("%-32s → %s\n", k, v)
		}
	default:
		fatal("usage: cfrproxy map [PATTERN TARGET | --rm PATTERN]")
	}
}

func cmdModels(args []string) {
	var name, data string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--data":
			if i+1 < len(args) {
				data = args[i+1]
				i++
			}
		}
	}
	if data == "" {
		data = defaultDataDir()
	}
	s := openStore(data)
	defer s.Close()
	p := proxy.New(s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, prov := range s.Providers() {
		if name != "" && !strings.EqualFold(prov.Name, name) {
			continue
		}
		models, err := p.ListModels(ctx, prov)
		if err != nil {
			fmt.Printf("%s (%s): scan failed: %v\n", prov.Name, prov.Type, err)
			continue
		}
		fmt.Printf("%s (%s): %d models\n", prov.Name, prov.Type, len(models))
		for _, m := range models {
			fmt.Printf("  %s/%s\n", prov.Name, m)
		}
	}
}
