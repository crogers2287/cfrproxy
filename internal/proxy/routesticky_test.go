package proxy

import (
	"fmt"
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
