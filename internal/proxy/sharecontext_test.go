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

// A ForceModel share pins every request to one model, so that is the window the
// client will actually get — the advertised number must follow the pin, not the
// id the client happened to ask about.
func TestEndpointAdvertisedContextFollowsForceModel(t *testing.T) {
	s := newDiscoveryStore(t)
	if err := s.SaveProvider(&store.Provider{
		Name: "fred", Type: "openai", BaseURL: "http://x", Enabled: true, ContextLength: 98304,
	}); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	// pinned to a fred model: the pin's window is what the client gets
	ep := store.Endpoint{ForceModel: "fred/pinned"}
	if got := p.endpointAdvertisedContext(ep, "fred/something-else"); got != 98304 {
		t.Fatalf("force-model window: got %d, want 98304", got)
	}

	// the share's cap lowers it
	ep.ContextLength = 32768
	if got := p.endpointAdvertisedContext(ep, "fred/something-else"); got != 32768 {
		t.Fatalf("capped: got %d, want 32768", got)
	}

	// a cap above the backend's window is ignored, never advertised
	ep.ContextLength = 262144
	if got := p.endpointAdvertisedContext(ep, "fred/anything"); got != 98304 {
		t.Fatalf("over-cap must be ignored: got %d, want 98304", got)
	}
}

// An unqualified id has no provider to resolve against. We must not invent a
// window; only an explicit operator cap is honest there.
func TestEndpointAdvertisedContextUnqualifiedID(t *testing.T) {
	p := New(newDiscoveryStore(t))
	if got := p.endpointAdvertisedContext(store.Endpoint{}, "bare-model"); got != 0 {
		t.Fatalf("unqualified with no cap: got %d, want 0", got)
	}
	if got := p.endpointAdvertisedContext(store.Endpoint{ContextLength: 8192}, "bare-model"); got != 8192 {
		t.Fatalf("unqualified with cap: got %d, want 8192", got)
	}
}
