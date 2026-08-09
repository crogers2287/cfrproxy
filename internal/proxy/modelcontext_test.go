package proxy

import "testing"

// The catalog exists because harnesses guess from the model id, so the cases
// that matter most are the ones where the id is a poor guide: aggregator
// renames, and families whose newer member has a SMALLER window.
func TestCatalogContextFor(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  int
	}{
		// Plain ids straight off the provider.
		{"claude 5 is 1M", "claude-opus-5", 1_000_000},
		{"claude 4 stays 200k", "claude-opus-4-8", 200_000},
		{"dated claude 4 id", "claude-sonnet-4-5-20250929", 200_000},
		{"gpt 5.6", "gpt-5.6-terra", 1_050_000},
		{"gemini 3 refresh", "gemini-3.6-flash-high", 1_048_576},
		{"deepseek v4 flash", "deepseek-v4-flash", 1_000_000},

		// Renamed by the aggregator mounts. These are the ids Hermes
		// actually sees on /p/opencode and /p/command, and the reason
		// matching is substring rather than equality.
		{"opencode rename", "claude-opencode-sonnet-5", 1_000_000},
		{"command rename", "claude-command-deepseek-v4-flash", 1_000_000},
		{"opencode go rename", "claude-opencode-go-deepseek-v4-flash", 1_000_000},
		{"vendor-prefixed", "deepseek/deepseek-v4-flash", 1_000_000},

		// Longest match wins. Without that, "glm-5" would swallow
		// "glm-5.2" and under-report a 1M model by 5x.
		{"glm 5.2 beats glm 5", "glm-5.2", 1_000_000},
		{"glm 5 unaffected", "glm-5", 200_000},
		{"glm 4.5 never left 128k", "glm-4.5-air", 128_000},

		// id order is not size order: 4.5 is newer than 4.3 but smaller.
		{"grok 4.5 is 500k", "grok-4.5", 500_000},
		{"grok 4.3 is 1M", "grok-4.3", 1_000_000},
		{"grok 4.20 variant", "grok-4.20-0309-reasoning", 1_000_000},

		// Unknown models advertise nothing rather than a fabricated
		// number — the harness then uses its own documented default.
		{"unsourced model", "gpt-5.3-codex-spark", 0},
		{"image model", "grok-imagine-video", 0},
		{"empty", "", 0},
		{"nonsense", "totally-made-up-model", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := catalogContextFor(c.model); got != c.want {
				t.Fatalf("catalogContextFor(%q) = %d, want %d", c.model, got, c.want)
			}
		})
	}
}

// Case should not decide whether a window is advertised.
func TestCatalogContextForIsCaseInsensitive(t *testing.T) {
	if got := catalogContextFor("Claude-Opus-5"); got != 1_000_000 {
		t.Fatalf("mixed-case id = %d, want 1000000", got)
	}
}

// A narrow "unsourced" entry must beat the broader family rule, or the mini
// tier silently inherits the flagship's window.
func TestUnsourcedEntryBeatsBroaderFamilyRule(t *testing.T) {
	if got := catalogContextFor("gpt-5.4"); got != 1_050_000 {
		t.Fatalf("gpt-5.4 = %d, want 1050000", got)
	}
	if got := catalogContextFor("gpt-5.4-mini"); got != 0 {
		t.Fatalf("gpt-5.4-mini = %d, want 0 (unsourced, must not inherit 5.4)", got)
	}
	if got := catalogContextFor("claude-opencode-gpt-5.4-mini"); got != 0 {
		t.Fatalf("renamed mini = %d, want 0", got)
	}
}
