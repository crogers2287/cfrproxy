package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A full sticky table sheds its oldest quarter, not everything: for the pool
// affinity table a wipe means every live conversation re-prefills at once.
func TestStickyRoutesEvictOldestQuarterOnly(t *testing.T) {
	s := &stickyRoutes{m: map[string]stickyEntry{}}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < stickyMaxEntries; i++ {
		s.m[fmt.Sprintf("fp%d", i)] = stickyEntry{model: "m", at: base.Add(time.Duration(i) * time.Millisecond)}
	}
	s.put("fresh", "m", "code")
	if n := len(s.m); n < stickyMaxEntries*3/4 || n >= stickyMaxEntries+1 {
		t.Fatalf("expected roughly three quarters retained, have %d", n)
	}
	if _, _, ok := s.get("fresh", time.Hour); !ok {
		t.Fatal("the entry just pinned must survive eviction")
	}
	if _, _, ok := s.get(fmt.Sprintf("fp%d", stickyMaxEntries-1), 2*time.Hour); !ok {
		t.Fatal("the newest pre-existing entry must survive eviction")
	}
	if _, _, ok := s.get("fp0", 2*time.Hour); ok {
		t.Fatal("the oldest entry should have been evicted")
	}
}

func TestStickyRoutesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pins.json")
	a := &stickyRoutes{m: map[string]stickyEntry{}, path: path}
	a.put("conv1", "local/tiel-a", "routine")
	a.put("conv2", "cloud/terra", "hard")
	a.mu.Lock()
	a.m["stale"] = stickyEntry{model: "x/y", bucket: "b", at: time.Now().Add(-3 * time.Hour)}
	a.dirty = true
	a.mu.Unlock()
	a.flush()
	b := &stickyRoutes{m: map[string]stickyEntry{}}
	if n := b.load(path, 2*time.Hour); n != 2 {
		t.Fatalf("loaded %d entries, want 2 (stale one dropped)", n)
	}
	if m, bk, ok := b.get("conv1", time.Hour); !ok || m != "local/tiel-a" || bk != "routine" {
		t.Fatalf("conv1 after reload: %s %s %v", m, bk, ok)
	}
	if _, _, ok := b.get("stale", 24*time.Hour); ok {
		t.Fatal("stale entry must not come back")
	}
	// a clean table with nothing changed writes nothing
	os.Remove(path)
	b.flush()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("flush without changes must not write")
	}
}
