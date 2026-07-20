package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// ListModels queries a provider's native model-listing endpoint and returns
// the model IDs it serves.
func (p *Proxy) ListModels(ctx context.Context, prov store.Provider) ([]string, error) {
	base := strings.TrimRight(prov.BaseURL, "/")
	var url string
	switch prov.Type {
	case "ollama":
		if strings.HasSuffix(base, "/api") {
			url = base + "/tags"
		} else {
			url = base + "/api/tags"
		}
	default: // openai + anthropic both serve GET .../v1/models
		if strings.HasSuffix(base, "/v1") {
			url = base + "/models"
		} else {
			url = base + "/v1/models"
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	switch prov.Type {
	case "anthropic":
		if prov.APIKey != "" {
			req.Header.Set("x-api-key", prov.APIKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if prov.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+prov.APIKey)
		}
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	if prov.Type == "ollama" {
		var out struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		var ids []string
		for _, m := range out.Models {
			ids = append(ids, m.Name)
		}
		return ids, nil
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// modelCache holds per-provider scan results so the data-plane listing
// endpoints don't hammer providers on every harness poll.
type modelCache struct {
	mu      sync.Mutex
	entries map[int64]modelCacheEntry
}

type modelCacheEntry struct {
	models []string
	at     time.Time
}

const modelCacheTTL = 60 * time.Second

// ModelsCached returns a provider's model list from cache, scanning when
// stale. Errors degrade to an empty list (callers fall back to the
// configured default model).
func (p *Proxy) ModelsCached(ctx context.Context, prov store.Provider) []string {
	p.models.mu.Lock()
	e, ok := p.models.entries[prov.ID]
	p.models.mu.Unlock()
	if ok && time.Since(e.at) < modelCacheTTL {
		return e.models
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ids, err := p.ListModels(cctx, prov)
	if err != nil {
		ids = nil
	}
	p.models.mu.Lock()
	p.models.entries[prov.ID] = modelCacheEntry{models: ids, at: time.Now()}
	p.models.mu.Unlock()
	return ids
}

// AllModelIDs merges every enabled provider's scanned models (as
// provider/model), plus configured aliases and defaults. Scans run in
// parallel on cold cache.
func (p *Proxy) AllModelIDs(ctx context.Context) []string {
	provs := p.Store.Providers()
	type result struct {
		idx    int
		models []string
	}
	var wg sync.WaitGroup
	results := make([]result, 0, len(provs))
	var mu sync.Mutex
	for i, prov := range provs {
		if !prov.Enabled {
			continue
		}
		wg.Add(1)
		go func(i int, prov store.Provider) {
			defer wg.Done()
			ms := p.ModelsCached(ctx, prov)
			mu.Lock()
			results = append(results, result{i, ms})
			mu.Unlock()
		}(i, prov)
	}
	wg.Wait()
	byIdx := map[int][]string{}
	for _, r := range results {
		byIdx[r.idx] = r.models
	}
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for i, prov := range provs {
		if !prov.Enabled {
			continue
		}
		for _, m := range byIdx[i] {
			add(prov.Name + "/" + m)
		}
		if len(byIdx[i]) == 0 && prov.DefaultModel != "" {
			add(prov.Name + "/" + prov.DefaultModel)
		}
		for _, alias := range strings.Split(prov.Models, ",") {
			add(strings.TrimSpace(alias))
		}
	}
	if len(ids) == 0 {
		ids = []string{"default"}
	}
	return ids
}
