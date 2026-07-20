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
