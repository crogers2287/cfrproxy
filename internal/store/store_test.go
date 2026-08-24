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

// The caveman flag must survive a save/load round trip on BOTH tables, and
// default to OFF. Column lists, placeholders and Scan destinations are three
// separate lists that must stay in lockstep; a mismatch in any one of them is
// silent at compile time (it only shows up as a runtime SQL error, or worse, a
// setting that quietly never persists).
func TestCavemanFlagRoundTrips(t *testing.T) {
	s := newTestStore(t)

	t.Run("provider defaults off", func(t *testing.T) {
		p := Provider{Name: "cave-default", Type: "openai", BaseURL: "https://x.invalid/v1", Enabled: true}
		if err := s.SaveProvider(&p); err != nil {
			t.Fatal(err)
		}
		got, ok := s.ProviderByName("cave-default")
		if !ok {
			t.Fatal("provider not found after save")
		}
		if got.Caveman {
			t.Fatal("caveman must default to OFF")
		}
	})

	t.Run("provider on then off", func(t *testing.T) {
		p := Provider{Name: "cave-on", Type: "openai", BaseURL: "https://y.invalid/v1", Enabled: true, Caveman: true}
		if err := s.SaveProvider(&p); err != nil {
			t.Fatal(err)
		}
		got, _ := s.ProviderByName("cave-on")
		if !got.Caveman {
			t.Fatal("caveman=true did not persist through insert")
		}
		got.Caveman = false
		if err := s.SaveProvider(&got); err != nil {
			t.Fatal(err)
		}
		again, _ := s.ProviderByName("cave-on")
		if again.Caveman {
			t.Fatal("caveman=false did not persist through update")
		}
	})

	t.Run("endpoint round trip", func(t *testing.T) {
		e := Endpoint{Name: "cave-ep", APIKey: "cfr_test", Enabled: true}
		if err := s.SaveEndpoint(&e); err != nil {
			t.Fatal(err)
		}
		eps, err := s.Endpoints()
		if err != nil {
			t.Fatal(err)
		}
		var found *Endpoint
		for i := range eps {
			if eps[i].Name == "cave-ep" {
				found = &eps[i]
			}
		}
		if found == nil {
			t.Fatal("endpoint not found")
		}
		if found.Caveman {
			t.Fatal("endpoint caveman must default to OFF")
		}
		found.Caveman = true
		found.APIKey = "" // blank = keep existing key; exercises that UPDATE variant
		if err := s.SaveEndpoint(found); err != nil {
			t.Fatal(err)
		}
		eps, _ = s.Endpoints()
		for _, x := range eps {
			if x.Name == "cave-ep" && !x.Caveman {
				t.Fatal("endpoint caveman=true did not persist")
			}
		}
	})
}
