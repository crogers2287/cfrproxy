// cfrproxy — universal LLM proxy: any harness dialect in (openai, anthropic,
// ollama), any provider out, with declarative transforms in between.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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

// Set by the Makefile via -ldflags; `cfrproxy version` and GET /api/version
// report them so a session can confirm which build is actually serving.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

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
		if len(rest) > 0 && rest[0] == "trajectories" {
			cmdTrajectories(rest[1:])
			return
		}
		cmdRoute(rest)
	case "kvx":
		cmdKVX(rest)
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
	case "explain":
		cmdExplain(rest)
	case "skills":
		cmdSkills(rest)
	case "version", "--version", "-v":
		fmt.Printf("cfrproxy %s (commit %s, built %s)\n", version, commit, buildDate)
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
  cfrproxy version                                    build version, commit, date
  cfrproxy route trajectories [--limit N] [--json]        # what auto did to each conversation
  cfrproxy kvx prefixes [--client claude-code]              # recorded harness prefixes (seed sources)
  cfrproxy kvx seed --model fred/ornith-kvx-w6800 [--client claude-code] [--newest N]   # pin them in kvxd
  cfrproxy explain <model> [--endpoint N] [--scope P] [--image] [--inbound openai|anthropic|ollama] [--json]
  cfrproxy explain auto --tokens N --tools K [--image] [--tier routine|careful|hard | --text "..."]   # smart router dry run
                   dry-run the routing for a model id: policy, resolution, pool, fallback chain
  cfrproxy skills  list [--used] | groups | group set NAME [--desc D] [--members a,b,c]
                   | group rm NAME | rescan | import-usage FILE.json | assign provider|endpoint NAME [--groups g1,g2] [--skills s1,s2]
   cfrproxy provider add --name N (--preset P | --type T --base-url U)
                   [--key K] [--model M] [--models a,b] [--fallback P/M] [--pinned m1,m2] [--doc-url U]
                   [--doc-file F.md] [--inject-docs] [--models-filter 'claude-*,!claude-*-thinking']
                   [--context-length 262144]   advertised context window; 0 = auto-detect
                   [--reasoning off|low|medium|high|xhigh] [--reasoning-force]
                   thinking level for these models when the client sends none (force: always)
                   [--headers '{"User-Agent":"...","Authorization":"@file:/path"}']
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
	proxy.Version, proxy.Commit, proxy.BuildDate = version, commit, buildDate
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
		// Under systemd stdout is the journal, and a password does not belong
		// in a log that survives for months. Park it in the data dir instead.
		pwFile := filepath.Join(*data, "admin-password.txt")
		if err := os.WriteFile(pwFile, []byte(fresh+"\n"), 0o600); err != nil {
			fmt.Printf("  first run  : generated WebUI password: %s  (change with `cfrproxy passwd`)\n", fresh)
		} else {
			fmt.Printf("  first run  : WebUI password written to %s  (change with `cfrproxy passwd`)\n", pwFile)
		}
	}
	// ReadHeaderTimeout bounds slow-header connections; Read/Write timeouts
	// stay unset because long generations legitimately stream for minutes.
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		// Let in-flight requests (streams included) finish before the process
		// exits; systemd's default stop timeout is 90s, so 25s leaves room.
		fmt.Fprintln(os.Stderr, "cfrproxy: shutting down, draining in-flight requests (up to 25s)")
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	for _, f := range []string{"name", "preset", "type", "base-url", "key", "model", "models", "doc-url", "doc-file", "fallback", "pinned", "models-filter", "context-length", "headers", "reasoning"} {
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
	reasoningForce := fs.Bool("reasoning-force", false, "apply --reasoning even when the client sends its own level")
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
		if *f["reasoning"] != "" || passed["reasoning"] {
			lvl, err := store.NormalizeReasoning(*f["reasoning"])
			if err != nil {
				fatal(err.Error())
			}
			p.ReasoningEffort = lvl
		}
		if passed["reasoning-force"] {
			p.ReasoningForce = *reasoningForce
		}
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

// cmdExplain dry-runs routing for a model id against the live config — no
// request is sent. Answers "why did this land on X" and "why is this 403 on
// my share endpoint" without reading proxy.go.
func cmdExplain(args []string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	endpoint := fs.String("endpoint", "", "share endpoint name (the /e/{name} mount)")
	scope := fs.String("scope", "", "provider name of a /p/{provider} mount")
	inbound := fs.String("inbound", "openai", "inbound dialect: openai | anthropic | ollama | responses")
	image := fs.Bool("image", false, "the request carries an image")
	tokens := fs.Int("tokens", 0, "smart router: estimated prompt tokens")
	tools := fs.Int("tools", 0, "smart router: tools attached")
	depth := fs.Int("depth", 0, "smart router: messages so far")
	tier := fs.String("tier", "", "smart router: routine|careful|hard (skips the classifier)")
	text := fs.String("text", "", "smart router: last user message to grade")
	cached := fs.Bool("cached", false, "smart router: assume the static prefix is already cached locally (seeded)")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	var model string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		model, args = args[0], args[1:]
	}
	fs.Parse(args)
	if model == "" && fs.NArg() > 0 {
		model = fs.Arg(0)
	}
	if model == "" {
		fatal("usage: cfrproxy explain <model> [--endpoint N] [--scope P] [--image] [--inbound D] [--json]")
	}
	s := openStore(*data)
	defer s.Close()
	p := proxy.New(s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := p.Explain(ctx, proxy.ExplainRequest{Model: model, Endpoint: *endpoint, Scope: *scope, Inbound: *inbound, Image: *image,
		Tokens: *tokens, Tools: *tools, Depth: *depth, Tier: *tier, Text: *text, PrefixCached: *cached})
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Print(res.Text())
	}
	if res.Error != "" {
		os.Exit(1)
	}
}

// cmdSkills manages the skill index from the shell: the same store the WebUI
// edits, so cron jobs (usage import from aise) and scripts need no admin
// password.
func cmdSkills(args []string) {
	if len(args) < 1 {
		fatal("usage: cfrproxy skills list|groups|group|rescan|import-usage|assign ...")
	}
	fs := flag.NewFlagSet("skills", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	desc := fs.String("desc", "", "group description")
	members := fs.String("members", "", "comma-separated skill names (group set)")
	groups := fs.String("groups", "", "comma-separated group names (assign)")
	skillsFlag := fs.String("skills", "", "comma-separated skill names (assign)")
	used := fs.Bool("used", false, "list only skills with recorded usage")
	// positional words first, flags after
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	split := func(v string) []string {
		var out []string
		for _, x := range strings.Split(v, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	switch pos[0] {
	case "list":
		all, _ := s.Skills()
		loads, ext := s.SkillLoads(), s.SkillUsageExternal()
		type row struct {
			name  string
			score int64
			path  string
		}
		var rows []row
		seen := map[string]bool{}
		for _, sk := range all {
			k := strings.ToLower(strings.ReplaceAll(sk.Name, " ", "-"))
			if seen[k] {
				continue
			}
			seen[k] = true
			sc := loads[k].Count
			for _, u := range ext[k] {
				sc += u.Calls
			}
			if *used && sc == 0 {
				continue
			}
			rows = append(rows, row{sk.Name, sc, sk.Path})
		}
		sort.Slice(rows, func(i, j int) bool {
			return rows[i].score > rows[j].score || (rows[i].score == rows[j].score && rows[i].name < rows[j].name)
		})
		for _, r := range rows {
			fmt.Printf("%6d  %-36s %s\n", r.score, r.name, r.path)
		}
	case "groups":
		gs, _ := s.SkillGroups()
		for _, g := range gs {
			fmt.Printf("%-24s %3d members  %s\n    %s\n", g.Name, len(g.Members), g.Description, strings.Join(g.Members, ", "))
		}
	case "group":
		if len(pos) < 3 {
			fatal("usage: cfrproxy skills group set|rm NAME [--desc D] [--members a,b,c]")
		}
		switch pos[1] {
		case "set":
			g, ok := s.SkillGroupByName(pos[2])
			if !ok {
				g = store.SkillGroup{Name: pos[2]}
			}
			if *desc != "" {
				g.Description = *desc
			}
			if *members != "" {
				g.Members = split(*members)
			}
			if err := s.SaveSkillGroup(&g); err != nil {
				fatal("%v", err)
			}
			fmt.Printf("group %q: %d members\n", g.Name, len(g.Members))
		case "rm":
			g, ok := s.SkillGroupByName(pos[2])
			if !ok {
				fatal("no such group")
			}
			if err := s.DeleteSkillGroup(g.ID); err != nil {
				fatal("%v", err)
			}
			fmt.Println("deleted", g.Name)
		default:
			fatal("group: set or rm")
		}
	case "rescan":
		n, err := s.ScanSkills(8)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println("indexed", n, "skills")
	case "import-usage":
		if len(pos) < 2 {
			fatal("usage: cfrproxy skills import-usage FILE.json  (shape: {\"source\":\"hermes\",\"entries\":{\"name\":{\"calls\":N,\"sessions\":M}}} or a list of such objects)")
		}
		b, err := os.ReadFile(pos[1])
		if err != nil {
			fatal("%v", err)
		}
		type payload struct {
			Source  string                      `json:"source"`
			Entries map[string]store.SkillUsage `json:"entries"`
		}
		var many []payload
		if err := json.Unmarshal(b, &many); err != nil {
			var one payload
			if err2 := json.Unmarshal(b, &one); err2 != nil {
				fatal("bad file: %v", err)
			}
			many = []payload{one}
		}
		for _, p := range many {
			if err := s.ImportSkillUsage(p.Source, p.Entries); err != nil {
				fatal("%s: %v", p.Source, err)
			}
			fmt.Printf("imported %d entries for %s\n", len(p.Entries), p.Source)
		}
	case "assign":
		if len(pos) < 3 {
			fatal("usage: cfrproxy skills assign provider|endpoint NAME [--groups g1,g2] [--skills s1,s2]")
		}
		kind := pos[1]
		var id int64
		if kind == "provider" {
			p, ok := s.ProviderByName(pos[2])
			if !ok {
				fatal("no such provider")
			}
			id = p.ID
		} else {
			e, ok := s.EndpointByName(pos[2])
			if !ok {
				fatal("no such endpoint")
			}
			id = e.ID
		}
		var gids []int64
		for _, n := range split(*groups) {
			g, ok := s.SkillGroupByName(n)
			if !ok {
				fatal("no such group %q", n)
			}
			gids = append(gids, g.ID)
		}
		var sids []int64
		for _, n := range split(*skillsFlag) {
			sk, ok := s.BestSkillCopy(n)
			if !ok {
				fatal("no readable indexed skill named %q", n)
			}
			sids = append(sids, sk.ID)
		}
		if err := s.SetTargetGroups(kind, id, gids); err != nil {
			fatal("%v", err)
		}
		if err := s.SetTargetSkills(kind, id, sids); err != nil {
			fatal("%v", err)
		}
		eff := s.EffectiveSkillsFor(kind, id, "")
		fmt.Printf("%s %s now carries %d skill(s):\n", kind, pos[2], len(eff))
		for _, e := range eff {
			flag := ""
			if e.Missing {
				flag = "  (MISSING — no readable copy indexed)"
			}
			fmt.Printf("  %-32s via %s%s\n", e.Name, e.Via, flag)
		}
	default:
		fatal("unknown skills subcommand %q", pos[0])
	}
}

// cmdTrajectories prints one line per conversation the smart router handled.
func cmdTrajectories(args []string) {
	fs := flag.NewFlagSet("route trajectories", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	limit := fs.Int("limit", 40, "conversations to show")
	scan := fs.Int("scan", 2000, "newest traces to scan")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	s := openStore(*data)
	defer s.Close()
	p := proxy.New(s)
	ts, err := p.RouteTrajectories(*scan, *limit)
	if err != nil {
		fatal("%v", err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(ts, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Print(proxy.TrajectoriesText(ts))
}

// cmdKVX: prefixes | seed — kv-rosetta seeding from cfrproxy's recorded
// static prefixes (see internal/proxy/kvxseed.go).
func cmdKVX(args []string) {
	if len(args) < 1 {
		fatal("usage: cfrproxy kvx prefixes|seed ...")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("kvx "+sub, flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	client := fs.String("client", "", "harness label (claude-code, omp, openai-sdk, …); blank = all")
	model := fs.String("model", "", "provider/model to seed (must be resident in llama-swap)")
	newest := fs.Int("newest", 1, "prefixes per harness, newest first")
	turn := fs.String("user-turn", "", "user turn appended to the prefix (default: seed)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	ps, err := proxy.LoadSeedPrefixes(*client)
	if err != nil {
		fatal("%v", err)
	}
	switch sub {
	case "prefixes":
		if *asJSON {
			b, _ := json.MarshalIndent(ps, "", "  ")
			fmt.Println(string(b))
			return
		}
		fmt.Printf("%-14s %-24s %-20s %-8s %-6s %s\n", "client", "recorded on", "last seen", "sys(B)", "tools", "fingerprint")
		for _, p := range ps {
			fmt.Printf("%-14s %-24s %-20s %-8d %-6d %s\n", p.Client, p.Model, p.LastSeen, p.SystemBytes, p.ToolCount, p.Fingerprint[:12])
		}
	case "seed":
		if *model == "" {
			fatal("--model required")
		}
		count := map[string]int{}
		var pick []proxy.SeedPrefix
		for _, p := range ps {
			if count[p.Client] < *newest {
				count[p.Client]++
				pick = append(pick, p)
			}
		}
		if len(pick) == 0 {
			fatal("no recorded prefixes match")
		}
		s := openStore(*data)
		defer s.Close()
		p := proxy.New(s)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := p.KVXSeed(ctx, *model, pick, *turn)
		if err != nil {
			fatal("%v", err)
		}
		if *asJSON {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
			return
		}
		for _, r := range res {
			switch {
			case r.Seeded:
				fmt.Printf("%-14s %s seeded %d tokens into slot %d (%.1fs)\n", r.Client, r.Prefix, r.Tokens, r.Slot, r.Seconds)
			case r.Already:
				fmt.Printf("%-14s %s already held\n", r.Client, r.Prefix)
			default:
				fmt.Printf("%-14s %s not seeded: %s\n", r.Client, r.Prefix, r.Reason)
			}
		}
	default:
		fatal("usage: cfrproxy kvx prefixes|seed ...")
	}
}
