package proxy

import (
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// A share cap must only ever lower the window. Raising it is the failure this
// field exists to prevent: a model whose slot holds 98,304 tokens advertised as
// 256,000 let an agent build prompts no slot could accept.
func TestContextLimitForCapsOnlyDownward(t *testing.T) {
	p := New(newDiscoveryStore(t))
	prov := store.Provider{Name: "fred", ContextLength: 98304}

	cases := []struct {
		name string
		cap  int
		want int
	}{
		{"no cap uses derived", 0, 98304},
		{"cap below derived wins", 32768, 32768},
		{"cap above derived is ignored", 262144, 98304},
		{"cap equal to derived", 98304, 98304},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := &store.Endpoint{ContextLength: tc.cap}
			if got := p.ContextLimitFor(ep, prov, "m"); got != tc.want {
				t.Fatalf("cap=%d: got %d, want %d", tc.cap, got, tc.want)
			}
		})
	}
}

// When the upstream reports nothing there is no number to contradict, so an
// explicit operator value is better than the harness inventing one from the id.
func TestContextLimitForSuppliesWhenUpstreamSilent(t *testing.T) {
	p := New(newDiscoveryStore(t))
	prov := store.Provider{Name: "fred"} // no ContextLength, no metadata
	if got := p.ContextLimitFor(&store.Endpoint{ContextLength: 65536}, prov, "unknown-model"); got != 65536 {
		t.Fatalf("silent upstream: got %d, want 65536", got)
	}
	// ...but with no cap either, we still advertise nothing rather than guess.
	if got := p.ContextLimitFor(&store.Endpoint{}, prov, "unknown-model"); got != 0 {
		t.Fatalf("silent upstream and no cap: got %d, want 0", got)
	}
}

func TestContextLimitForNilEndpoint(t *testing.T) {
	p := New(newDiscoveryStore(t))
	prov := store.Provider{Name: "fred", ContextLength: 131072}
	if got := p.ContextLimitFor(nil, prov, "m"); got != 131072 {
		t.Fatalf("nil endpoint: got %d, want 131072", got)
	}
}
