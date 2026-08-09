package main

// Harness launcher: `cfrproxy claude --model nexum/qwen-3.8` (or codex,
// opencode, omp, any binary on PATH) execs the harness with every
// conventional dialect env var pointed at the proxy, so its /model picker
// lists whatever the providers actually serve.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	// Harness-specific model flags, for harnesses that ignore the env vars above.
	//
	// opencode namespaces every model as <its-provider>/<model>, and its
	// provider ids are its own, not cfrproxy's. Passing a bare
	// "fred/deepseek-v4-flash" makes it look for an opencode provider called
	// "fred", find nothing, and silently fall back to its configured default —
	// which is exactly the "launched with the wrong model" symptom. Prefix with
	// whichever opencode provider actually points at this proxy.
	switch harness {
	case "codex":
		if model != "" {
			fwd = append([]string{"-m", model}, fwd...)
		}
	case "opencode":
		if model != "" {
			prov := opencodeProviderFor(addr)
			if prov == "" {
				fmt.Fprintf(os.Stderr,
					"warning: no opencode provider points at %s — cannot set --model %q.\n"+
						"         add one to ~/.config/opencode/opencode.json, or opencode will use its own default.\n",
					addr, model)
			} else {
				fwd = append([]string{"--model", prov + "/" + model}, fwd...)
				model = prov + "/" + model // so the banner shows what was actually passed
			}
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
	bin := findCLIProxyBin()
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
		// filtered, not raw: a shared upstream answers with its whole catalog
		// and routing rejects whatever models_filter excludes, so printing the
		// raw list would offer provider/model pairs that cannot be reached
		models, scanned, err := p.ListModelsFiltered(ctx, prov)
		if err != nil {
			fmt.Printf("%s (%s): scan failed: %v\n", prov.Name, prov.Type, err)
			continue
		}
		if len(models) != scanned {
			fmt.Printf("%s (%s): %d of %d models (filter: %s)\n",
				prov.Name, prov.Type, len(models), scanned, prov.ModelsFilter)
		} else {
			fmt.Printf("%s (%s): %d models\n", prov.Name, prov.Type, len(models))
		}
		for _, m := range models {
			fmt.Printf("  %s/%s\n", prov.Name, m)
		}
	}
}

// findCLIProxyBin locates the CLIProxyAPI binary: an explicit CLIPROXY_BIN
// wins, then PATH, then the usual install spots. Checking those spots matters
// because CLIProxyAPI is commonly built from source into its own directory and
// never linked onto PATH — without this, `cfrproxy login` fails on a machine
// where the daemon is plainly running.
func findCLIProxyBin() string {
	if v := os.Getenv("CLIPROXY_BIN"); v != "" {
		return v
	}
	if p, err := exec.LookPath("cli-proxy-api"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, "cliproxyapi", "cli-proxy-api"),
		filepath.Join(home, "cli-proxy-api", "cli-proxy-api"),
		filepath.Join(home, ".cli-proxy-api", "cli-proxy-api"),
		"/usr/local/bin/cli-proxy-api",
		"/opt/cli-proxy-api/cli-proxy-api",
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return "cli-proxy-api" // last resort; the caller reports a clear not-found
}

// cmdVision reports how the proactive vision gate classifies models, so an
// operator can see WHY an image rerouted without reading proxy source. With no
// arguments it classifies every enabled provider's default model plus every
// configured vision target; with arguments it classifies exactly those ids.
func cmdVision(args []string) {
	var data string
	var models []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--data" && i+1 < len(args) {
			data = args[i+1]
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "--") {
			models = append(models, args[i])
		}
	}
	if data == "" {
		data = defaultDataDir()
	}
	s := openStore(data)
	defer s.Close()
	p := proxy.New(s)

	cfg := p.VisionFallbackConfig()
	pats, custom, disabled := p.VisionModelPatterns()
	state := "off"
	if cfg.Enabled && len(cfg.Targets) > 0 {
		state = "on"
	}
	fmt.Printf("vision fallback chain : %s", state)
	if len(cfg.Targets) > 0 {
		fmt.Printf("  →  %s", strings.Join(cfg.Targets, ", "))
	}
	fmt.Println()
	switch {
	case disabled:
		fmt.Println("proactive gate       : DISABLED (vision_models=\"-\") — on-error failover only")
	case custom:
		fmt.Printf("proactive gate       : on, %d custom globs (vision_models)\n", len(pats))
	default:
		fmt.Printf("proactive gate       : on, %d built-in globs\n", len(pats))
	}
	if state == "off" {
		fmt.Println("\nNo targets configured, so image requests are never rerouted. Set one with:")
		fmt.Println(`  cfrproxy config set vision_fallback '{"enabled":true,"targets":["gemini/gemini-3-flash"]}'`)
	}

	if len(models) == 0 {
		seen := map[string]bool{}
		for _, prov := range s.Providers() {
			if prov.Enabled && prov.DefaultModel != "" {
				m := prov.Name + "/" + prov.DefaultModel
				if !seen[m] {
					seen[m] = true
					models = append(models, m)
				}
			}
		}
		for _, t := range cfg.Targets {
			if t = strings.TrimSpace(t); t != "" && !seen[t] {
				seen[t] = true
				models = append(models, t)
			}
		}
	}
	if len(models) == 0 {
		return
	}
	fmt.Println()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, m := range models {
		sees, how := p.VisionCapable(m), "name"
		// a scoped id lets us ask the provider itself, which is authoritative
		if i := strings.IndexByte(m, '/'); i > 0 && !sees {
			if prov, ok := s.ProviderByName(m[:i]); ok {
				if p.VisionCapableFor(ctx, prov, m[i+1:]) {
					sees, how = true, "provider says so"
				}
			}
		}
		if sees {
			fmt.Printf("  %-52s sees images (%s)\n", m, how)
		} else {
			fmt.Printf("  %-52s BLIND → image requests route to the vision chain\n", m)
		}
	}
}

// opencodeProviderFor finds the opencode provider id whose baseURL points at
// this cfrproxy instance, so `cfrproxy opencode --model fred/x` can hand
// opencode the "<provider>/fred/x" spelling it actually understands. Returns ""
// when opencode has no such provider configured, so the caller can say so
// instead of launching the wrong model silently.
func opencodeProviderFor(addr string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// host:port is the stable part; scheme and trailing path vary between the
	// launcher's addr and however the user wrote baseURL
	hostport := strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	hostport = strings.TrimSuffix(hostport, "/")
	if i := strings.IndexByte(hostport, '/'); i > 0 {
		hostport = hostport[:i]
	}
	for _, name := range []string{"opencode.json", "config.json"} {
		b, err := os.ReadFile(filepath.Join(home, ".config", "opencode", name))
		if err != nil {
			continue
		}
		var cfg struct {
			Provider map[string]struct {
				Options struct {
					BaseURL string `json:"baseURL"`
				} `json:"options"`
			} `json:"provider"`
		}
		if json.Unmarshal(b, &cfg) != nil {
			continue
		}
		for id, pv := range cfg.Provider {
			if hostport != "" && strings.Contains(pv.Options.BaseURL, hostport) {
				return id
			}
		}
	}
	return ""
}

// cmdSyncOpencode writes cfrproxy's live catalogue into opencode's provider
// `models` map.
//
// opencode's custom `@ai-sdk/openai-compatible` providers only expose models
// DECLARED in that map — dynamic discovery exists only for a few built-in
// providers. Its TUI validates `--model` against the map and, when the model
// is absent, silently falls back to the config default, which looks exactly
// like "the flag is being ignored". Declaring the catalogue fixes both the
// missing picker entries and the ignored flag.
func cmdSyncOpencode(args []string) {
	addr, dry := defaultAddr(), false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			if i+1 < len(args) {
				addr = args[i+1]
				i++
			}
		case "--dry-run":
			dry = true
		}
	}
	addr = strings.TrimRight(addr, "/")

	home, err := os.UserHomeDir()
	if err != nil {
		fatal("home dir: %v", err)
	}
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("read %s: %v", path, err)
	}
	// Round-trip through a generic map so every unrelated key the user has
	// (agents, mcp, permissions, other providers) survives untouched.
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fatal("parse %s: %v", path, err)
	}
	provs, _ := cfg["provider"].(map[string]any)
	if provs == nil {
		fatal("no \"provider\" section in %s", path)
	}
	target := ""
	hostport := strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
	for id, v := range provs {
		pv, _ := v.(map[string]any)
		opts, _ := pv["options"].(map[string]any)
		if b, _ := opts["baseURL"].(string); strings.Contains(b, hostport) {
			target = id
			break
		}
	}
	if target == "" {
		fatal("no opencode provider points at %s — add one to %s first", addr, path)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(addr + "/v1/models")
	if err != nil {
		fatal("fetch models: %v", err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		fatal("decode models: %v", err)
	}
	if len(list.Data) == 0 {
		fatal("cfrproxy returned no models")
	}

	models := map[string]any{}
	withCtx := 0
	for _, m := range list.Data {
		entry := map[string]any{"name": m.ID}
		if m.ContextLength > 0 {
			// Carry the advertised window through so opencode sizes its own
			// compaction correctly instead of guessing from the id. Its schema
			// requires `output` whenever `limit` is present, and cfrproxy has
			// no per-model output cap to report, so derive a conventional
			// quarter-of-context clamped to a sane band.
			out := m.ContextLength / 4
			if out < 4096 {
				out = 4096
			}
			if out > 32768 {
				out = 32768
			}
			if out > m.ContextLength {
				out = m.ContextLength
			}
			entry["limit"] = map[string]any{"context": m.ContextLength, "output": out}
			withCtx++
		}
		models[m.ID] = entry
	}
	pv, _ := provs[target].(map[string]any)
	prev := 0
	if old, ok := pv["models"].(map[string]any); ok {
		prev = len(old)
	}
	fmt.Printf("opencode provider %q → %d models (was %d), %d with a context limit\n",
		target, len(models), prev, withCtx)
	if dry {
		fmt.Println("dry run: nothing written")
		return
	}
	pv["models"] = models
	provs[target] = pv

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fatal("encode: %v", err)
	}
	bak := path + ".bak-cfrsync-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(bak, raw, 0o644); err != nil {
		fatal("backup: %v", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		fatal("write: %v", err)
	}
	fmt.Printf("wrote %s (backup: %s)\n", path, filepath.Base(bak))
}
