package proxy

import (
	"strings"
	"testing"
)

func poolStore(t *testing.T, pools string) *Proxy {
	t.Helper()
	s := newDiscoveryStore(t)
	if pools != "" {
		if err := s.SetSetting("model_pools", pools); err != nil {
			t.Fatal(err)
		}
	}
	return New(s)
}

func TestPoolMembersParsing(t *testing.T) {
	p := poolStore(t, `{"tiel-w6800":["tiel-coder-q5-w6800","tiel-b-w6800"]}`)
	got := p.poolMembers("tiel-w6800")
	if len(got) != 2 || got[0] != "tiel-coder-q5-w6800" || got[1] != "tiel-b-w6800" {
		t.Fatalf("members = %v", got)
	}
	if p.poolMembers("something-else") != nil {
		t.Error("unpooled model returned members")
	}
	// a pool of one is not a pool: dispatching would just add indirection
	if poolStore(t, `{"solo":["only-one"]}`).poolMembers("solo") != nil {
		t.Error("single-member pool treated as a pool")
	}
	// unset and malformed settings must not panic or route anywhere
	if poolStore(t, "").poolMembers("tiel-w6800") != nil {
		t.Error("unset setting returned members")
	}
	if poolStore(t, "{not json").poolMembers("tiel-w6800") != nil {
		t.Error("malformed setting returned members")
	}
}

// The dispatcher must send work to the instance with the least in flight —
// with turns varying from a dozen tokens to several thousand, round-robin
// would keep feeding an instance still grinding on a long answer.
func TestPickLeastBusyMember(t *testing.T) {
	p := poolStore(t, `{"tiel-w6800":["a","b"]}`)
	m := p.poolMembers("tiel-w6800")

	if got := p.pickPoolMember(m); got != "a" {
		t.Errorf("empty pool picked %q, want the first member", got)
	}
	p.inflight.add("a", 1)
	if got := p.pickPoolMember(m); got != "b" {
		t.Errorf("with a busy, picked %q, want b", got)
	}
	p.inflight.add("b", 2)
	if got := p.pickPoolMember(m); got != "a" {
		t.Errorf("with b busier, picked %q, want a", got)
	}
	// releasing work makes an instance eligible again
	p.inflight.add("b", -2)
	if got := p.pickPoolMember(m); got != "b" {
		t.Errorf("after b drained, picked %q, want b", got)
	}
	// saturation is not an error: it still picks one and lets llama.cpp queue
	p.inflight.add("a", 5)
	p.inflight.add("b", 5)
	if got := p.pickPoolMember(m); got == "" {
		t.Error("no member chosen when both are busy; the request must still queue somewhere")
	}
}

func TestPoolStatusReporting(t *testing.T) {
	p := poolStore(t, `{"tiel-w6800":["a","b"]}`)
	p.inflight.add("a", 3)
	st := p.PoolStatus("tiel-w6800")
	if st["a"] != 3 || st["b"] != 0 {
		t.Errorf("status = %v", st)
	}
	if p.PoolStatus("nope") != nil {
		t.Error("status for an unpooled model should be nil")
	}
}

// Balance check: dispatching N requests without releasing them must spread
// across members rather than piling onto one.
func TestPoolSpreadsLoad(t *testing.T) {
	p := poolStore(t, `{"m":["a","b"]}`)
	members := p.poolMembers("m")
	for i := 0; i < 10; i++ {
		p.inflight.add(p.pickPoolMember(members), 1)
	}
	a, b := p.inflight.get("a"), p.inflight.get("b")
	if a+b != 10 {
		t.Fatalf("lost dispatches: a=%d b=%d", a, b)
	}
	if a != 5 || b != 5 {
		t.Errorf("uneven spread a=%d b=%d, want 5/5", a, b)
	}
	_ = strings.TrimSpace("")
}
