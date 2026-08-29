package store

import (
	"os"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfrproxy-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyEncryptionAtRest(t *testing.T) {
	s := newTestStore(t)
	p := &Provider{Name: "or", Type: "openai", BaseURL: "https://x", APIKey: "sk-secret-123", Enabled: true}
	if err := s.SaveProvider(p); err != nil {
		t.Fatal(err)
	}
	// raw DB must not contain the plaintext key
	var enc []byte
	if err := s.db.QueryRow(`SELECT api_key_enc FROM providers WHERE id=?`, p.ID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if string(enc) == "sk-secret-123" || len(enc) == 0 {
		t.Fatal("api key stored in plaintext or empty")
	}
	got, ok := s.ProviderByName("or")
	if !ok || got.APIKey != "sk-secret-123" {
		t.Fatalf("decrypt round-trip failed: %+v", got)
	}
	// update without key keeps the key
	got.APIKey = ""
	got.DefaultModel = "m2"
	if err := s.SaveProvider(&got); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ProviderByName("or")
	if again.APIKey != "sk-secret-123" {
		t.Fatal("empty-key update wiped the stored key")
	}
}

func TestResolve(t *testing.T) {
	s := newTestStore(t)
	s.SaveProvider(&Provider{Name: "a", Type: "openai", BaseURL: "https://a", DefaultModel: "am", Priority: 10, Enabled: true, Models: "alias-1"})
	s.SaveProvider(&Provider{Name: "b", Type: "ollama", BaseURL: "http://b", DefaultModel: "bm", Priority: 20, Enabled: true})

	if p, m, _ := s.Resolve("b/llama3"); p.Name != "b" || m != "llama3" {
		t.Errorf("prefixed resolve: %s %s", p.Name, m)
	}
	if p, m, _ := s.Resolve("alias-1"); p.Name != "a" || m != "alias-1" {
		t.Errorf("alias resolve: %s %s", p.Name, m)
	}
	if p, m, _ := s.Resolve("unknown-model"); p.Name != "a" || m != "unknown-model" {
		t.Errorf("priority fallback: %s %s", p.Name, m)
	}
	if _, m, _ := s.Resolve("a/"); m != "am" {
		t.Errorf("prefix default model: %s", m)
	}
	// reorder flips priority
	pb, _ := s.ProviderByName("b")
	pa, _ := s.ProviderByName("a")
	s.Reorder([]int64{pb.ID, pa.ID})
	if p, _, _ := s.Resolve("whatever"); p.Name != "b" {
		t.Errorf("after reorder, want b first, got %s", p.Name)
	}
}

// A provider named "Qwen" must be findable from the scoped mount however the
// harness spells it. Hermes configs carried "/p/Qwen /v1" (trailing space from
// a since-renamed provider) and lowercase "/p/qwen/v1"; both missed the
// case-sensitive exact lookup and surfaced as a provider with zero models in
// the Telegram picker.
func TestProviderByNameIsTrimmedAndCaseInsensitive(t *testing.T) {
	s := newTestStore(t)
	p := Provider{Name: "Qwen", Type: "openai", BaseURL: "https://example.invalid/v1", DefaultModel: "qwen3.7-max", Enabled: true}
	if err := s.SaveProvider(&p); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"Qwen", "qwen", "QWEN", "Qwen ", " qwen"} {
		got, ok := s.ProviderByName(spelling)
		if !ok || got.ID != p.ID {
			t.Errorf("ProviderByName(%q) did not resolve to the Qwen provider", spelling)
		}
	}
	if _, ok := s.ProviderByName("nope"); ok {
		t.Error("unknown provider name resolved")
	}
	if _, ok := s.ProviderByName("   "); ok {
		t.Error("blank provider name resolved")
	}
}

// An exact match must still win when two names differ only by case, so the
// loose fallback can never steal traffic from a precisely-addressed provider.
func TestProviderByNameExactMatchWins(t *testing.T) {
	s := newTestStore(t)
	lower := Provider{Name: "acme", Type: "openai", BaseURL: "https://a.invalid/v1", Enabled: true}
	upper := Provider{Name: "ACME", Type: "openai", BaseURL: "https://b.invalid/v1", Enabled: true}
	if err := s.SaveProvider(&lower); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProvider(&upper); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ProviderByName("acme"); got.ID != lower.ID {
		t.Errorf("exact match lost: got %q (%d), want %q (%d)", got.Name, got.ID, lower.Name, lower.ID)
	}
	if got, _ := s.ProviderByName("ACME"); got.ID != upper.ID {
		t.Errorf("exact match lost: got %q (%d), want %q (%d)", got.Name, got.ID, upper.Name, upper.ID)
	}
}

// Names are trimmed on save so a stray space can never reach base_url builders
// (/p/<name>/v1) or the Hermes sync script.
func TestSaveProviderTrimsName(t *testing.T) {
	s := newTestStore(t)
	p := Provider{Name: "  Spaced  ", Type: "openai", BaseURL: "  https://x.invalid/v1  ", Enabled: true}
	if err := s.SaveProvider(&p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "Spaced" {
		t.Errorf("name not trimmed on save: %q", p.Name)
	}
	if p.BaseURL != "https://x.invalid/v1" {
		t.Errorf("base_url not trimmed on save: %q", p.BaseURL)
	}
	got, ok := s.ProviderByName("Spaced")
	if !ok || got.Name != "Spaced" {
		t.Errorf("stored name not trimmed: %+v", got)
	}
}

func TestIntegritySettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	p := Provider{
		Name: "observed", Type: "openai", BaseURL: "https://example.invalid/v1", Enabled: true,
		IntegrityMode: "OBSERVE", IntegrityModels: "code-*,!*-vision", IntegrityProfile: "CODE",
	}
	if err := s.SaveProvider(&p); err != nil {
		t.Fatal(err)
	}
	got, ok := s.ProviderByName(p.Name)
	if !ok || got.IntegrityMode != "observe" || got.IntegrityModels != p.IntegrityModels || got.IntegrityProfile != "code" {
		t.Fatalf("provider integrity settings did not round-trip: %+v", got)
	}

	ep := Endpoint{
		Name: "team", APIKey: "share-secret", Enabled: true,
		IntegrityMode: "observe", IntegrityModels: "observed/code-*", IntegrityProfile: "multilingual",
	}
	if err := s.SaveEndpoint(&ep); err != nil {
		t.Fatal(err)
	}
	eps, err := s.Endpoints()
	if err != nil || len(eps) != 1 {
		t.Fatalf("endpoints: %v %+v", err, eps)
	}
	if got := eps[0]; got.IntegrityMode != "observe" || got.IntegrityModels != ep.IntegrityModels || got.IntegrityProfile != "multilingual" {
		t.Fatalf("endpoint integrity settings did not round-trip: %+v", got)
	}
	badProvider := Provider{Name: "bad", Type: "openai", BaseURL: "https://example.invalid", IntegrityMode: "enforce"}
	if err := s.SaveProvider(&badProvider); err == nil {
		t.Fatal("reserved enforce mode was accepted")
	}
	badEndpoint := Endpoint{Name: "bad", IntegrityMode: "enforce"}
	if err := s.SaveEndpoint(&badEndpoint); err == nil {
		t.Fatal("reserved endpoint enforce mode was accepted")
	}
}

func TestGuardTraceRoundTripLabelAndStats(t *testing.T) {
	s := newTestStore(t)
	trace := Trace{
		TS: 1, Provider: "observed", Model: "code-model", Status: 200,
		GuardMode: "observe", GuardProfile: "code", GuardState: "corrupt",
		GuardScore: 0.82, GuardMaxScore: 0.91, GuardReason: "repetition loop",
		GuardOnset: 750, GuardChars: 1500, GuardCheckpoints: 4,
		GuardData: `{"version":1,"samples":[]}`, GuardExcerpt: "loop loop loop",
	}
	s.AddTrace(&trace)
	if trace.ID == 0 {
		t.Fatal("trace was not inserted")
	}
	traces, err := s.Traces(0, 10)
	if err != nil || len(traces) != 1 {
		t.Fatalf("traces: %v %+v", err, traces)
	}
	got := traces[0]
	if got.GuardState != "corrupt" || got.GuardMaxScore != trace.GuardMaxScore || got.GuardData != trace.GuardData || got.GuardExcerpt != trace.GuardExcerpt {
		t.Fatalf("guard observation did not round-trip: %+v", got)
	}
	if err := s.SetGuardLabel(trace.ID, "clean"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGuardLabel(trace.ID, "not-a-label"); err == nil {
		t.Fatal("invalid label was accepted")
	}
	cleanTrace := Trace{TS: 2, Provider: "observed", Model: "code-model", Status: 200,
		GuardMode: "observe", GuardProfile: "code", GuardState: "clean", GuardData: `{"version":1}`}
	s.AddTrace(&cleanTrace)
	if err := s.SetGuardLabel(cleanTrace.ID, "corrupt"); err != nil {
		t.Fatal(err)
	}
	traces, _ = s.Traces(0, 10)
	if traces[0].GuardLabel != "corrupt" || traces[1].GuardLabel != "clean" {
		t.Fatalf("labels not stored: %+v", traces)
	}
	stats, err := s.Stats()
	if err != nil || len(stats) != 1 {
		t.Fatalf("stats: %v %+v", err, stats)
	}
	if stats[0].GuardObserved != 2 || stats[0].GuardCorrupt != 1 || stats[0].GuardReviewed != 2 || stats[0].GuardFalseAlarms != 1 || stats[0].GuardMisses != 1 {
		t.Fatalf("guard stats wrong: %+v", stats[0])
	}
	export, err := s.IntegrityObservations(0)
	if err != nil || len(export) != 2 {
		t.Fatalf("integrity export: %v %+v", err, export)
	}
	if export[0].GuardData != trace.GuardData || export[0].GuardLabel != "clean" || export[0].ReqSnip != "" || export[0].RespSnip != "" {
		t.Fatalf("integrity export omitted or leaked fields: %+v", export[0])
	}
}
