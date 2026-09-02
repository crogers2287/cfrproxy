package proxy

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// Body captured verbatim from fred's llama-server (beellama preview-v0.4.4)
// on 2026-08-19 — a warm request that restored 16000 tokens from the RAM
// prompt cache. The field names here are the contract; if the fork renames
// one, this test fails rather than silently logging 0% forever.
const warmBody = `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":16059,` +
	`"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":16000}},` +
	`"timings":{"cache_n":16000,"cache_lcp_n":16049,"cache_planned_n":16000,` +
	`"cache_reprocessed_n":59,"cache_source":"ram","cache_reason":"committed",` +
	`"prompt_n":16059,"prompt_ms":5386.2,"prompt_per_second":11.0,"predicted_per_second":36.2}}`

func TestApplyCacheTimingsWarm(t *testing.T) {
	tr := &store.Trace{}
	applyCacheTimings(tr, []byte(warmBody))
	if tr.CacheSource != "ram" || tr.CacheReason != "committed" {
		t.Fatalf("source/reason = %q/%q", tr.CacheSource, tr.CacheReason)
	}
	if tr.CacheReprocessed != 59 || tr.CacheLCP != 16049 {
		t.Fatalf("reprocessed=%d lcp=%d", tr.CacheReprocessed, tr.CacheLCP)
	}
	if tr.CachedTokens != 16000 {
		t.Fatalf("cached tokens fell back wrong: %d", tr.CachedTokens)
	}
	if tr.PromptTPS != 11.0 || tr.DecodeTPS != 36.2 {
		t.Fatalf("rates = %v/%v", tr.PromptTPS, tr.DecodeTPS)
	}
}

// A cold miss must be recorded with its REASON, not just as a zero. The reason
// string is what distinguishes "new conversation" from "the prefix was broken".
func TestApplyCacheTimingsColdMissKeepsReason(t *testing.T) {
	cold := `{"usage":{"prompt_tokens":16059},"timings":{"cache_n":0,"cache_lcp_n":31,` +
		`"cache_reprocessed_n":16059,"cache_source":"none",` +
		`"cache_reason":"no_restorable_kvarn_boundary","prompt_ms":31262.3,"prompt_per_second":513.7}}`
	tr := &store.Trace{}
	applyCacheTimings(tr, []byte(cold))
	if tr.CacheReason != "no_restorable_kvarn_boundary" {
		t.Fatalf("reason = %q", tr.CacheReason)
	}
	if tr.CachedTokens != 0 || tr.CacheReprocessed != 16059 {
		t.Fatalf("cached=%d reprocessed=%d", tr.CachedTokens, tr.CacheReprocessed)
	}
}

// Providers that are not llama.cpp-family must leave the trace untouched, so
// the cache log never claims a 0% hit rate for an OpenAI/Anthropic request.
func TestApplyCacheTimingsIgnoresNonLlamaCpp(t *testing.T) {
	tr := &store.Trace{CachedTokens: 4096}
	applyCacheTimings(tr, []byte(`{"usage":{"prompt_tokens":9,"completion_tokens":1}}`))
	if tr.CacheSource != "" || tr.CacheReason != "" || tr.CachedTokens != 4096 {
		t.Fatalf("clobbered a non-llama.cpp trace: %+v", tr)
	}
}

// The real capture path for 95% of fred's traffic: timings arrive in the final
// SSE chunk, after the [DONE]-adjacent deltas, and only the tail is retained.
func TestTimingsSnifferReadsFinalSSEChunk(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"pad\"}}]}\n\n", 500) +
		"data: " + warmBody + "\n\n" +
		"data: [DONE]\n\n"
	s := newTimingsSniffer(io.NopCloser(strings.NewReader(stream)))
	if _, err := io.Copy(io.Discard, s); err != nil {
		t.Fatal(err)
	}
	tr := &store.Trace{}
	s.apply(tr)
	if tr.CacheSource != "ram" || tr.CacheReprocessed != 59 {
		t.Fatalf("sniffer missed the final chunk: %+v", tr)
	}
}

// The retained tail is bounded — a long generation must not buffer the whole
// stream just to reach the timings at the end.
func TestTimingsSnifferTailIsBounded(t *testing.T) {
	big := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 200000)
	s := newTimingsSniffer(io.NopCloser(strings.NewReader(big)))
	io.Copy(io.Discard, s)
	if len(s.tail) > sniffTailBytes {
		t.Fatalf("tail grew to %d bytes, cap is %d", len(s.tail), sniffTailBytes)
	}
}

func TestClientLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"claude-cli/2.0.1 (external, cli)": "claude-code",
		"codex_cli_rs/0.99.0":              "codex",
		"omp/18.0.8":                       "omp",
		"OpenAI/Python 1.55.0":             "openai-sdk",
		"":                                 "unknown",
		"curl/8.5.0":                       "curl",
	} {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		if got := clientLabel(r); got != want {
			t.Errorf("clientLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}
