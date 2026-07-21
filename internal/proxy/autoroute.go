package proxy

// Auto router: requests addressed to the virtual model "auto" are classified
// into a task bucket by a small (ideally local) classifier model, then
// forwarded to the provider/model mapped for that bucket. Config lives in
// settings key "auto_router":
//
//	{"enabled": true,
//	 "classifier": "ollama/qwen2.5:7b",
//	 "routes": {"code":"oauth/gpt-5.6-terra", "reasoning":"...", "quick":"...",
//	            "long":"...", "vision":"...", "default":"..."}}

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

type AutoRouterConfig struct {
	Enabled    bool              `json:"enabled"`
	Classifier string            `json:"classifier"`
	Routes     map[string]string `json:"routes"`
}

func (p *Proxy) AutoRouterConfig() AutoRouterConfig {
	var c AutoRouterConfig
	if raw := p.Store.Setting("auto_router"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// AutoRoute classifies the request and returns (target model, bucket).
// Any failure degrades to the "default" route; ("", "") means auto routing
// is not configured and the caller should resolve "auto" normally.
func (p *Proxy) AutoRoute(ctx context.Context, req *wire.Request) (string, string) {
	cfg := p.AutoRouterConfig()
	if !cfg.Enabled || len(cfg.Routes) == 0 {
		return "", ""
	}
	def := cfg.Routes["default"]
	if cfg.Classifier == "" {
		return def, "default"
	}

	// snapshot of the request for the classifier
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" && req.Messages[i].Content != "" {
			lastUser = req.Messages[i].Content
			break
		}
	}
	if len(lastUser) > 2000 {
		lastUser = lastUser[:2000]
	}
	buckets := make([]string, 0, len(cfg.Routes))
	for k := range cfg.Routes {
		if k != "default" {
			buckets = append(buckets, k)
		}
	}
	sort.Strings(buckets)
	prompt := fmt.Sprintf(
		"Classify this LLM request into exactly one bucket: %s. Reply with ONLY the bucket word.\n\nRequest has %d tools attached. Latest user message:\n%s",
		strings.Join(buckets, ", "), len(req.Tools), lastUser)

	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	prov, model, err := p.ResolveModel(cctx, cfg.Classifier)
	if err != nil {
		return def, "default"
	}
	creq := &wire.Request{Model: model, MaxTokens: 8,
		Messages: []wire.Msg{{Role: "user", Content: prompt}}}
	body, err := buildOutbound(prov.Type, creq)
	if err != nil {
		return def, "default"
	}
	resp, err := p.send(cctx, prov, providerPath(prov.Type), body)
	if err != nil {
		return def, "default"
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return def, "default"
	}
	norm, err := parseOutboundResponse(prov.Type, rb)
	if err != nil {
		return def, "default"
	}
	answer := strings.ToLower(strings.TrimSpace(norm.Content))
	for _, b := range buckets {
		if strings.Contains(answer, strings.ToLower(b)) {
			return cfg.Routes[b], b
		}
	}
	return def, "default"
}
