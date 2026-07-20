package main

// Harness launcher: `cfrproxy claude --model nexum/qwen-3.8` (or codex,
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
			if m, ok := fuzzyModel(p.ModelsCached(ctx, prov), rest); ok {
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
		if m, ok := fuzzyModel(p.ModelsCached(ctx, prov), spec); ok {
			full := prov.Name + "/" + m
			return full, noteIfChanged(spec, full)
		}
	}
	return spec, fmt.Sprintf("note: %q not found at any provider; passing through as typed", spec)
}

// fuzzyModel: exact > case-insensitive > unique substring > unique
// normalized substring (punctuation-blind, so "Qwen3.8" finds
// "qwen-3.8-max-preview-thinking").
func fuzzyModel(models []string, want string) (string, bool) {
	for _, m := range models {
		if m == want {
			return m, true
		}
	}
	for _, m := range models {
		if strings.EqualFold(m, want) {
			return m, true
		}
	}
	var subs []string
	for _, m := range models {
		if strings.Contains(strings.ToLower(m), strings.ToLower(want)) {
			subs = append(subs, m)
		}
	}
	if len(subs) == 1 {
		return subs[0], true
	}
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	subs = nil
	for _, m := range models {
		if strings.Contains(norm(m), norm(want)) {
			subs = append(subs, m)
		}
	}
	if len(subs) == 1 {
		return subs[0], true
	}
	return "", false
}

func noteIfChanged(typed, resolved string) string {
	if typed == resolved {
		return ""
	}
	return fmt.Sprintf("model %q resolved to %q", typed, resolved)
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
