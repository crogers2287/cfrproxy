package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// Prefix-cache observability.
//
// llama.cpp-family upstreams (llama-server, and the beellama fork fred runs)
// return a `timings` object alongside `usage`. It carries the only ground truth
// about prefix reuse there is: how many prompt tokens were restored from cache,
// how many had to be re-prefilled, where the restore came from, and — when
// nothing was reused — WHY. cfrproxy already recorded usage.prompt_tokens; it
// threw `timings` away, so an operator could see that a request was slow but
// never that it was slow because the prefix missed.
//
// The record goes to a JSONL file, one line per request, never to stdout: this
// proxy carries agent traffic at several requests per second and a per-request
// stdout line would bury the service log.

// cacheTimings mirrors the subset of llama.cpp's `timings` object that
// describes prefix reuse. Field names match the wire format exactly.
type cacheTimings struct {
	CacheN           int     `json:"cache_n"`             // prompt tokens restored from cache
	CacheLCPN        int     `json:"cache_lcp_n"`         // longest common prefix found
	CacheReprocessed int     `json:"cache_reprocessed_n"` // tokens that had to be prefilled again
	CacheSource      string  `json:"cache_source"`        // none | ram | checkpoint | slot
	CacheReason      string  `json:"cache_reason"`        // committed | no_restorable_kvarn_boundary | ...
	PromptN          int     `json:"prompt_n"`
	PromptMS         float64 `json:"prompt_ms"`
	PromptPerSecond  float64 `json:"prompt_per_second"`
	PredictedPerSec  float64 `json:"predicted_per_second"`
}

// parseCacheTimings pulls the `timings` object out of a complete upstream
// response body. Absent on providers that are not llama.cpp-family, which is
// why the bool matters — a zero-valued struct would otherwise be logged as a
// 0% cache hit for every OpenAI/Anthropic request.
func parseCacheTimings(body []byte) (cacheTimings, bool) {
	var v struct {
		Timings *cacheTimings `json:"timings"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Timings == nil {
		return cacheTimings{}, false
	}
	return *v.Timings, true
}

// applyCacheTimings copies upstream prefix-cache telemetry onto the trace.
func applyCacheTimings(tr *store.Trace, body []byte) {
	t, ok := parseCacheTimings(body)
	if !ok {
		return
	}
	tr.CacheLCP = t.CacheLCPN
	tr.CacheReprocessed = t.CacheReprocessed
	tr.CacheSource = t.CacheSource
	tr.CacheReason = t.CacheReason
	tr.PromptMS = t.PromptMS
	tr.PromptTPS = t.PromptPerSecond
	tr.DecodeTPS = t.PredictedPerSec
	// Prefer the timings value for cached tokens when usage did not carry it:
	// they are the same number (verified against llama-server: usage
	// .prompt_tokens_details.cached_tokens == timings.cache_n), but streamed
	// responses sometimes emit one and not the other.
	if tr.CachedTokens == 0 && t.CacheN > 0 {
		tr.CachedTokens = t.CacheN
	}
}

// timingsSniffer taps the raw upstream stream and keeps only its tail, because
// llama.cpp emits the `timings` object in the FINAL SSE chunk. 95% of fred's
// traffic is streamed, so without this the cache log would be near-empty.
//
// A tail tap is used rather than threading a timings field through wire.Delta
// and every dialect's readStream: this stays dialect-agnostic and touches no
// translation code.
type timingsSniffer struct {
	r    io.ReadCloser
	tail []byte
}

const sniffTailBytes = 32 << 10

func newTimingsSniffer(r io.ReadCloser) *timingsSniffer {
	return &timingsSniffer{r: r, tail: make([]byte, 0, sniffTailBytes+4096)}
}

func (s *timingsSniffer) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.tail = append(s.tail, p[:n]...)
		if len(s.tail) > sniffTailBytes {
			s.tail = s.tail[len(s.tail)-sniffTailBytes:]
		}
	}
	return n, err
}

func (s *timingsSniffer) Close() error { return s.r.Close() }

// apply parses the last SSE/NDJSON line in the tail that carries `timings`.
func (s *timingsSniffer) apply(tr *store.Trace) {
	if s == nil || len(s.tail) == 0 {
		return
	}
	for _, line := range bytes.Split(s.tail, []byte("\n")) {
		if !bytes.Contains(line, []byte("\"timings\"")) {
			continue
		}
		data := line
		if i := bytes.Index(line, []byte("data:")); i >= 0 {
			data = bytes.TrimSpace(line[i+5:])
		}
		// keep scanning: the last parseable line wins
		applyCacheTimings(tr, data)
	}
}

// clientLabel identifies which agent/harness issued the request, so cache hit
// rates can be attributed per client rather than smeared across the fleet.
// Claude Code sends "claude-cli/<ver>"; the OpenAI SDK that Hermes uses sends
// "OpenAI/Python <ver>". Anything unrecognised is reported by its first token.
// warmupClient is the label the prefix-warmup daemon identifies itself with, so
// its traffic can be excluded from prefix recording and reported separately.
const warmupClient = "kvwarm"

func clientLabel(r *http.Request) string {
	ua := strings.TrimSpace(r.Header.Get("User-Agent"))
	if ua == "" {
		return "unknown"
	}
	switch {
	case strings.HasPrefix(ua, "cfrproxy-kvwarm"):
		return warmupClient
	case strings.HasPrefix(ua, "claude-cli"):
		return "claude-code"
	case strings.HasPrefix(strings.ToLower(ua), "codex"):
		return "codex"
	case strings.HasPrefix(strings.ToLower(ua), "omp"), strings.Contains(strings.ToLower(ua), "oh-my-pi"):
		return "omp"
	case strings.HasPrefix(ua, "OpenAI/"), strings.Contains(ua, "openai-python"):
		// Hermes gateways reach the proxy through the OpenAI SDK; the suffix
		// varies by language binding ("OpenAI/Python 1.x", "OpenAI/JS ...").
		return "openai-sdk"
	case strings.Contains(ua, "node"), strings.Contains(ua, "undici"):
		return "node"
	}
	if i := strings.IndexAny(ua, " /"); i > 0 {
		return ua[:i]
	}
	return ua
}

const cacheLogMaxBytes = 64 << 20 // rotate at 64 MiB, keep one previous file

type cacheLogWriter struct {
	mu sync.Mutex
	f  *os.File
	n  int64
}

var cacheLog cacheLogWriter

// cacheLogPath mirrors main.go's default data dir (~/.cfrproxy). Override with
// CFRPROXY_CACHE_LOG; set it to "off" to disable the log entirely.
func cacheLogPath() string {
	if v := strings.TrimSpace(os.Getenv("CFRPROXY_CACHE_LOG")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cfrproxy", "cache-observability.jsonl")
}

// cacheRecord is the per-request observability row. One line of JSON.
type cacheRecord struct {
	TS               string  `json:"ts"`
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	Client           string  `json:"client"`
	Inbound          string  `json:"inbound"`
	Stream           bool    `json:"stream"`
	Status           int     `json:"status"`
	InputTokens      int     `json:"input_tokens"`
	CacheHitTokens   int     `json:"cache_hit_tokens"`
	NewPrefillTokens int     `json:"new_prefill_tokens"`
	HitRatePct       float64 `json:"prefix_hit_rate_pct"`
	PrefillTPS       float64 `json:"prefill_tokens_per_sec"`
	PrefillMS        float64 `json:"prefill_ms"`
	TTFBMS           int64   `json:"ttfb_ms"`
	DecodeTPS        float64 `json:"decode_tokens_per_sec"`
	OutputTokens     int     `json:"output_tokens"`
	LatencyMS        int64   `json:"latency_ms"`
	CacheSource      string  `json:"cache_source,omitempty"`
	CacheReason      string  `json:"cache_reason,omitempty"`
}

// writeCacheRecord appends one JSONL row. Best-effort: observability must never
// fail a request, so every error path simply drops the line.
func writeCacheRecord(tr *store.Trace) {
	// Only llama.cpp-family upstreams populate these; skip everything else so
	// the log stays a prefix-cache log rather than a second trace table.
	if tr.CacheSource == "" && tr.CacheReason == "" && tr.PromptMS == 0 {
		return
	}
	path := cacheLogPath()
	if path == "" || path == "off" {
		return
	}

	newPrefill := tr.CacheReprocessed
	if newPrefill == 0 && tr.PromptTokens > 0 {
		newPrefill = tr.PromptTokens - tr.CachedTokens
	}
	hit := 0.0
	if tr.PromptTokens > 0 {
		hit = 100.0 * float64(tr.PromptTokens-newPrefill) / float64(tr.PromptTokens)
	}
	rec := cacheRecord{
		TS:       time.UnixMilli(tr.TS).UTC().Format(time.RFC3339Nano),
		Provider: tr.Provider, Model: tr.Model, Client: tr.Client,
		Inbound: tr.Inbound, Stream: tr.Stream, Status: tr.Status,
		InputTokens: tr.PromptTokens, CacheHitTokens: tr.CachedTokens,
		NewPrefillTokens: newPrefill, HitRatePct: hit,
		PrefillTPS: tr.PromptTPS, PrefillMS: tr.PromptMS,
		TTFBMS: tr.TTFBMS, DecodeTPS: tr.DecodeTPS,
		OutputTokens: tr.CompletionTokens, LatencyMS: tr.LatencyMS,
		CacheSource: tr.CacheSource, CacheReason: tr.CacheReason,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	line = append(line, '\n')

	cacheLog.mu.Lock()
	defer cacheLog.mu.Unlock()
	if cacheLog.f == nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		if st, err := f.Stat(); err == nil {
			cacheLog.n = st.Size()
		}
		cacheLog.f = f
	}
	if cacheLog.n+int64(len(line)) > cacheLogMaxBytes {
		cacheLog.f.Close()
		os.Rename(path, path+".1")
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			cacheLog.f = nil
			return
		}
		cacheLog.f, cacheLog.n = f, 0
	}
	n, err := cacheLog.f.Write(line)
	if err != nil {
		cacheLog.f.Close()
		cacheLog.f = nil
		return
	}
	cacheLog.n += int64(n)
}
