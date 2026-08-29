// cfrproxy — universal LLM proxy: any harness dialect in (openai, anthropic,
// ollama), any provider out, with declarative transforms in between.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/api"
	"github.com/crogers2287/cfrproxy/internal/proxy"
	"github.com/crogers2287/cfrproxy/internal/store"
	"github.com/crogers2287/cfrproxy/internal/tui"
)

// presets give known providers a base URL and type so `provider add --preset
// openrouter` works; anything else is added generically with --base-url.
var presets = map[string]struct{ Type, BaseURL, Doc string }{
	"openai":     {"openai", "https://api.openai.com", "https://platform.openai.com/docs/api-reference"},
	"codex":      {"openai", "https://api.openai.com", "https://platform.openai.com/docs/api-reference"},
	"anthropic":  {"anthropic", "https://api.anthropic.com", "https://docs.anthropic.com/en/api"},
	"claude":     {"anthropic", "https://api.anthropic.com", "https://docs.anthropic.com/en/api"},
	"openrouter": {"openai", "https://openrouter.ai/api", "https://openrouter.ai/docs"},
	"ollama":     {"ollama", "http://127.0.0.1:11434", "https://github.com/ollama/ollama/blob/main/docs/api.md"},
	"supergrok":  {"openai", "https://api.x.ai", "https://docs.x.ai"},
	"grok":       {"openai", "https://api.x.ai", "https://docs.x.ai"},
	// commandcode speaks /alpha/generate — the only endpoint the $1 Go plan
	// permits (the Pro-only /provider/v1/* paths 403 on Go).
	"commandcode": {"commandcode", "https://api.commandcode.ai", "https://commandcode.ai"},
	"cmd":         {"commandcode", "https://api.commandcode.ai", "https://commandcode.ai"},
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cfrproxy")
}

func openStore(dataDir string) *store.Store {
	s, err := store.Open(dataDir)
	if err != nil {
		fatal("open store: %v", err)
	}
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, rest := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		cmdServe(rest)
	case "tui":
		cmdTUI(rest)
	case "provider":
		cmdProvider(rest)
	case "route":
		cmdRoute(rest)
	case "test":
		cmdTest(rest)
	case "logs":
		cmdLogs(rest)
	case "transform":
		cmdTransform(rest)
	case "passwd":
		cmdPasswd(rest)
	case "models":
		cmdModels(rest)
	case "vision":
		cmdVision(rest)
	case "sync-opencode":
		cmdSyncOpencode(rest)
	case "map":
		cmdMap(rest)
	case "login":
		cmdLogin(rest)
	case "oauth":
		cmdOAuth(rest)
	case "config":
		cmdConfig(rest)
	case "mcp":
		cmdMCP(rest)
	case "launch":
		if len(rest) < 1 {
			fatal("usage: cfrproxy launch <harness> [--model provider/model] [harness args...]")
		}
		cmdLaunch(rest[0], rest[1:])
	case "help", "-h", "--help":
		usage()
	default:
		// any binary on PATH is launchable directly: `cfrproxy claude --model ...`
		if _, err := exec.LookPath(cmd); err == nil {
			cmdLaunch(cmd, rest)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command %q (not a subcommand, and not a harness on PATH)\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`cfrproxy — universal LLM proxy

Usage:
  cfrproxy serve   [--addr :8420] [--data DIR]        run the proxy + WebUI
  cfrproxy tui     [--data DIR]                       full-screen management TUI
   cfrproxy provider add --name N (--preset P | --type T --base-url U)
                   [--key K] [--model M] [--models a,b] [--fallback P/M] [--pinned m1,m2] [--doc-url U]
                   [--doc-file F.md] [--inject-docs] [--models-filter 'claude-*,!claude-*-thinking']
                   [--context-length 262144]   advertised context window; 0 = auto-detect
                   [--headers '{"User-Agent":"...","Authorization":"@file:/path"}']
                   [--integrity-mode off|observe] [--integrity-models 'model-*,!*-vision']
                   [--integrity-profile general|code|multilingual]
                   extra outbound headers; @file: reads the value live each request
  cfrproxy provider list | rm --name N | edit --name N [flags]
                   on edit, passing an optional flag empty clears it:
                   --pinned '' restores the full catalog to model pickers
  cfrproxy route   [set N1,N2,...]                    show / set routing priority
  cfrproxy test    --name N [--prompt "..."]          send a test prompt
  cfrproxy logs    [-f] [-n 20]                       show / follow request traces
  cfrproxy transform list | add --name N --phase request|response --rules JSON
                   [--provider P] [--target openai|anthropic|ollama]
  cfrproxy transform enable|disable|rm --name N
  cfrproxy passwd  --pass NEWPASS                     reset WebUI basic-auth password
  cfrproxy models  [--name N]                         scan providers' live model lists
  cfrproxy sync-opencode [--dry-run]                  declare cfrproxy's catalog in
                   ~/.config/opencode/opencode.json — opencode only offers models
                   listed there, so without this its picker and --model both ignore them
  cfrproxy mcp                                        round-table consensus MCP server (stdio)
                   register: claude mcp add roundtable -- cfrproxy mcp
  cfrproxy config  set KEY VALUE | get KEY            server settings (e.g. cliproxy_mgmt_key)
  cfrproxy oauth   scan [--apply] [--key K]           auto-register providers for
                   every OAuth account CLIProxyAPI already holds (claude, codex,
                   grok/xai, antigravity/gemini, kimi) with the right models_filter
  cfrproxy login   codex|codex-device|claude|antigravity|kimi|supergrok [--no-browser]
                   OAuth device/browser login via CLIProxyAPI; models appear
                   under the "oauth" provider automatically
  cfrproxy map     [PATTERN TARGET | --rm PATTERN]    map harness model names to providers
                   e.g. cfrproxy map 'claude-sonnet*' fred/agents-a1
                   (Claude Code's /model presets become switchable slots)
  cfrproxy <harness> [--model provider/model] [args]  launch a harness through the proxy
                   e.g. cfrproxy claude --model nexum/qwen-3.8
                        cfrproxy codex --model fred/agents-a1
                        cfrproxy opencode | cfrproxy omp | any binary on PATH
                   (also: cfrproxy launch <harness> ...)

Inbound endpoints (point any harness at these):
  OpenAI-compat    POST /v1/chat/completions   (Codex, OpenCode, ...)
  Anthropic        POST /v1/messages           (Claude Code)
  Ollama           POST /api/chat              (anything ollama-native)

Model routing: "provider/model" targets a provider by name; a bare model name
matches provider alias lists; anything else goes to the highest-priority
enabled provider.
`)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8420", "listen address")
	data := fs.String("data", defaultDataDir(), "data directory")
	fs.Parse(args)

	s := openStore(*data)
	defer s.Close()
	p := proxy.New(s)
	a := &api.API{Store: s, Proxy: p}
	user, fresh, err := a.EnsureCredentials()
	if err != nil {
		fatal("credentials: %v", err)
	}

	mux := http.NewServeMux()
	p.Register(mux)
	a.Register(mux)

	fmt.Printf("cfrproxy listening on %s\n", *addr)
	fmt.Printf("  data plane : /v1/chat/completions  /v1/messages  /api/chat\n")
	fmt.Printf("  webui      : http://localhost%s/admin/  (user %q)\n", portOf(*addr), user)
	if fresh != "" {
		fmt.Printf("  first run  : generated WebUI password: %s  (change with `cfrproxy passwd`)\n", fresh)
	}
	srv := &http.Server{Addr: *addr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		fatal("%v", err)
	}
}

func portOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i:]
	}
	return addr
}

func cmdTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	if err := tui.Run(s, proxy.New(s)); err != nil {
		fatal("%v", err)
	}
}

func providerFlags(fs *flag.FlagSet) map[string]*string {
	m := map[string]*string{}
	for _, f := range []string{"name", "preset", "type", "base-url", "key", "model", "models", "doc-url", "doc-file", "fallback", "pinned", "models-filter", "context-length", "headers", "integrity-mode", "integrity-models", "integrity-profile"} {
		m[f] = fs.String(f, "", "")
	}
	return m
}

func cmdProvider(args []string) {
	if len(args) < 1 {
		fatal("usage: cfrproxy provider add|list|rm|edit ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("provider "+sub, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	f := providerFlags(fs)
	inject := fs.Bool("inject-docs", false, "inject docs as system context")
	disabled := fs.Bool("disabled", false, "add in disabled state")
	fs.Parse(rest)
	// Which flags the user actually typed. Optional fields are applied when the
	// flag was passed at all, so `--pinned ""` clears a curated list instead of
	// being silently ignored — without this there is no way to unset one.
	passed := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { passed[fl.Name] = true })
	applyOptional := func(name string, dst *string) {
		if *f[name] != "" || passed[name] {
			*dst = *f[name]
		}
	}
	s := openStore(*data)
	defer s.Close()

	switch sub {
	case "list":
		for _, p := range s.Providers() {
			state := "on "
			if !p.Enabled {
				state = "off"
			}
			key := "-"
			if p.HasKey {
				key = "set"
			}
			fmt.Printf("%3d  [%s]  %-16s %-10s %-40s key:%-4s model:%s\n", p.Priority, state, p.Name, p.Type, p.BaseURL, key, p.DefaultModel)
		}
	case "add", "edit":
		var p store.Provider
		if sub == "edit" {
			exist, ok := s.ProviderByName(*f["name"])
			if !ok {
				fatal("provider %q not found", *f["name"])
			}
			p = exist
			p.APIKey = "" // keep unless --key given
		}
		if *f["name"] != "" {
			p.Name = *f["name"]
		}
		if *f["preset"] != "" {
			pr, ok := presets[strings.ToLower(*f["preset"])]
			if !ok {
				fatal("unknown preset %q (known: openai codex anthropic claude openrouter ollama supergrok grok commandcode cmd)", *f["preset"])
			}
			p.Type, p.BaseURL = pr.Type, pr.BaseURL
			if p.DocURL == "" {
				p.DocURL = pr.Doc
			}
		}
		if *f["type"] != "" {
			p.Type = *f["type"]
		}
		if *f["base-url"] != "" {
			p.BaseURL = *f["base-url"]
		}
		if *f["key"] != "" {
			p.APIKey = *f["key"]
		}
		// optional fields: an explicitly-empty flag clears them
		applyOptional("model", &p.DefaultModel)
		applyOptional("models", &p.Models)
		applyOptional("doc-url", &p.DocURL)
		applyOptional("fallback", &p.Fallback)
		applyOptional("pinned", &p.PinnedModels)
		applyOptional("models-filter", &p.ModelsFilter)
		applyOptional("headers", &p.Headers)
		applyOptional("integrity-mode", &p.IntegrityMode)
		applyOptional("integrity-models", &p.IntegrityModels)
		applyOptional("integrity-profile", &p.IntegrityProfile)
		if *f["context-length"] != "" || passed["context-length"] {
			n, err := strconv.Atoi(strings.TrimSpace(*f["context-length"]))
			if err != nil || n < 0 {
				fatal("--context-length must be a non-negative integer (0 = auto)")
			}
			p.ContextLength = n
		}
		if *f["doc-file"] != "" {
			b, err := os.ReadFile(*f["doc-file"])
			if err != nil {
				fatal("read doc file: %v", err)
			}
			p.DocMarkdown = string(b)
		}
		p.InjectDocs = p.InjectDocs || *inject
		if sub == "add" {
			p.Enabled = !*disabled
		}
		if *f["base-url"] != "" || sub == "add" {
			probe := p
			if probe.APIKey == "" {
				if exist, ok := s.ProviderByName(p.Name); ok {
					probe.APIKey = exist.APIKey
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			base, note := proxy.New(s).DiscoverBase(ctx, probe)
			cancel()
			p.BaseURL = base
			if note != "" {
				fmt.Println(note)
			}
		}
		if err := s.SaveProvider(&p); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("saved provider %s (id %d)\n", p.Name, p.ID)
	case "rm":
		p, ok := s.ProviderByName(*f["name"])
		if !ok {
			fatal("provider %q not found", *f["name"])
		}
		if err := s.DeleteProvider(p.ID); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed %s\n", p.Name)
	default:
		fatal("unknown provider subcommand %q", sub)
	}
}

func cmdRoute(args []string) {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	rest := fs.Args()
	if len(rest) == 2 && rest[0] == "set" {
		var ids []int64
		for _, name := range strings.Split(rest[1], ",") {
			p, ok := s.ProviderByName(strings.TrimSpace(name))
			if !ok {
				fatal("provider %q not found", name)
			}
			ids = append(ids, p.ID)
		}
		if err := s.Reorder(ids); err != nil {
			fatal("%v", err)
		}
	}
	fmt.Println("routing priority (first enabled wins for bare model names):")
	for i, p := range s.Providers() {
		state := ""
		if !p.Enabled {
			state = "  (disabled)"
		}
		fmt.Printf("  %d. %s%s\n", i+1, p.Name, state)
	}
}

func cmdTest(args []string) {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	name := fs.String("name", "", "provider name")
	prompt := fs.String("prompt", "Reply with the single word: pong", "test prompt")
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	prov, ok := s.ProviderByName(*name)
	if !ok {
		fatal("provider %q not found", *name)
	}
	p := proxy.New(s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := time.Now()
	resp, err := p.TestProvider(ctx, prov, *prompt)
	if err != nil {
		fatal("test failed: %v", err)
	}
	fmt.Printf("ok (%.1fs, %d tokens)\n%s\n", time.Since(start).Seconds(), resp.CompletionTokens, resp.Content)
}

func cmdLogs(args []string) {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	follow := fs.Bool("f", false, "follow")
	n := fs.Int("n", 20, "number of traces")
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	print := func(ts []store.Trace) int64 {
		last := int64(0)
		for i := len(ts) - 1; i >= 0; i-- {
			t := ts[i]
			stream := ""
			if t.Stream {
				stream = " stream"
			}
			// tok/s is what you actually want when comparing models; the
			// ttfb/post split says whether a slow call was the model thinking,
			// the model typing, or cfrproxy getting in the way.
			// pp/tg follow llama.cpp's naming: prompt processing (prefill) and
			// token generation, both in tokens/sec.
			perf := ""
			if pp := t.PromptPerSec(); pp > 0 {
				perf += fmt.Sprintf(" pp=%.0ftok/s", pp)
			}
			if tg := t.TokensPerSec(); tg > 0 {
				perf += fmt.Sprintf(" tg=%.1ftok/s %dout", tg, t.CompletionTokens)
			}
			if t.TTFBMS > 0 {
				perf += fmt.Sprintf(" ttfb=%dms", t.TTFBMS)
			}
			// always shown: an absent field reads as "not measured", which is
			// exactly the confusion that hid a broken measurement point
			if t.Status == 200 {
				perf += fmt.Sprintf(" post=%.2fms", float64(t.PostUS)/1000)
			}
			line := fmt.Sprintf("%s  %-12s %-24s %s %3d %5dms%s%s", time.UnixMilli(t.TS).Format("15:04:05"),
				t.Provider, t.Model, t.Inbound, t.Status, t.LatencyMS, perf, stream)
			if t.Err != "" {
				line += "  ERR: " + t.Err
			}
			fmt.Println(line)
			if t.ID > last {
				last = t.ID
			}
		}
		return last
	}
	ts, err := s.Traces(0, *n)
	if err != nil {
		fatal("%v", err)
	}
	last := print(ts)
	for *follow {
		time.Sleep(time.Second)
		ts, err := s.Traces(last, 100)
		if err != nil {
			continue
		}
		if l := print(ts); l > last {
			last = l
		}
	}
}

func cmdTransform(args []string) {
	if len(args) < 1 {
		fatal("usage: cfrproxy transform list|add|rm|enable|disable ...")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("transform "+sub, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	name := fs.String("name", "", "transform name")
	phase := fs.String("phase", "request", "request|response")
	rules := fs.String("rules", "", "JSON rules array")
	provider := fs.String("provider", "", "restrict to provider name")
	target := fs.String("target", "", "restrict to inbound dialect")
	fs.Parse(rest)
	s := openStore(*data)
	defer s.Close()

	findByName := func() store.Transform {
		ts, _ := s.Transforms()
		for _, t := range ts {
			if t.Name == *name {
				return t
			}
		}
		fatal("transform %q not found", *name)
		return store.Transform{}
	}

	switch sub {
	case "list":
		ts, err := s.Transforms()
		if err != nil {
			fatal("%v", err)
		}
		for _, t := range ts {
			state := "on "
			if !t.Enabled {
				state = "off"
			}
			scope := "all providers"
			if t.ProviderID != 0 {
				if p, ok := s.ProviderByID(t.ProviderID); ok {
					scope = p.Name
				} else {
					scope = "provider#" + strconv.FormatInt(t.ProviderID, 10)
				}
			}
			tgt := t.Target
			if tgt == "" {
				tgt = "any"
			}
			fmt.Printf("[%s] %-20s %-8s scope:%-14s target:%-9s %s\n", state, t.Name, t.Phase, scope, tgt, string(t.Rules))
		}
	case "add":
		t := store.Transform{Name: *name, Phase: *phase, Rules: []byte(*rules), Target: *target, Enabled: true}
		if *provider != "" {
			p, ok := s.ProviderByName(*provider)
			if !ok {
				fatal("provider %q not found", *provider)
			}
			t.ProviderID = p.ID
		}
		if err := s.SaveTransform(&t); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("added transform %s (id %d)\n", t.Name, t.ID)
	case "rm":
		t := findByName()
		if err := s.DeleteTransform(t.ID); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("removed %s\n", t.Name)
	case "enable", "disable":
		t := findByName()
		if err := s.SetTransformEnabled(t.ID, sub == "enable"); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%s %sd\n", t.Name, sub)
	default:
		fatal("unknown transform subcommand %q", sub)
	}
}

func cmdConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	fs.Parse(args)
	rest := fs.Args()
	s := openStore(*data)
	defer s.Close()
	switch {
	case len(rest) == 3 && rest[0] == "set":
		if err := s.SetSetting(rest[1], rest[2]); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("%s set\n", rest[1])
	case len(rest) == 2 && rest[0] == "get":
		v := s.Setting(rest[1])
		if strings.Contains(strings.ToLower(rest[1]), "key") || strings.Contains(strings.ToLower(rest[1]), "pass") {
			if v == "" {
				fmt.Println("(unset)")
			} else {
				fmt.Println("(set, hidden)")
			}
			return
		}
		fmt.Println(v)
	default:
		fatal("usage: cfrproxy config set KEY VALUE | get KEY")
	}
}

func cmdPasswd(args []string) {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	pass := fs.String("pass", "", "new password")
	fs.Parse(args)
	if *pass == "" {
		fatal("--pass required")
	}
	s := openStore(*data)
	defer s.Close()
	a := &api.API{Store: s}
	if err := a.SetPassword(*pass); err != nil {
		fatal("%v", err)
	}
	fmt.Println("password updated")
}

// cmdOAuth — `cfrproxy oauth scan [--apply]`. Turns the OAuth logins
// CLIProxyAPI already holds into cfrproxy providers, so a fresh install doesn't
// have to hand-build one provider per backend with the right models_filter.
func cmdOAuth(args []string) {
	if len(args) == 0 || args[0] != "scan" {
		fatal("usage: cfrproxy oauth scan [--apply] [--key K] [--data DIR]")
	}
	fs := flag.NewFlagSet("oauth scan", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	apply := fs.Bool("apply", false, "actually create the providers (default: preview only)")
	key := fs.String("key", "", "CLIProxyAPI api-key (default: reuse an existing provider's, else read config.yaml)")
	fs.Parse(args[1:])

	s := openStore(*data)
	defer s.Close()
	a := &api.API{Store: s, Proxy: proxy.New(s)}

	results, keySrc, err := a.ScanOAuth(context.Background(), *apply, *key)
	if err != nil {
		fatal("%v", err)
	}
	if len(results) == 0 {
		fmt.Println("no OAuth accounts found in CLIProxyAPI.")
		fmt.Println("log in first, e.g.: cfrproxy login claude   (also: codex, antigravity, supergrok, kimi)")
		return
	}
	fmt.Printf("CLIProxyAPI api-key: %s\n\n", keySrc)
	fmt.Printf("%-13s %-10s %-12s %-7s %s\n", "AUTH", "PROVIDER", "ACTION", "MODELS", "DEFAULT / DETAIL")
	for _, r := range results {
		d := r.Default
		if d == "" {
			d = r.Detail
		}
		fmt.Printf("%-13s %-10s %-12s %-7d %s\n", r.Auth, r.Provider, r.Action, r.Models, d)
	}
	if !*apply {
		fmt.Println("\npreview only — re-run with --apply to create these providers.")
		return
	}
	fmt.Println("\ndone. Next:")
	fmt.Println("  cfrproxy models              # confirm each provider's catalog")
	fmt.Println("  python3 scripts/sync_hermes_cfrproxy.py   # expose them in Hermes/Telegram pickers")
}
