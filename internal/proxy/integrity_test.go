package proxy

import (
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func TestIntegrityModelSelected(t *testing.T) {
	tests := []struct {
		name, patterns, provider, model string
		want                            bool
	}{
		{"blank selects all", "", "cloud", "alpha", true},
		{"bare glob", "alpha-*", "cloud", "alpha-code", true},
		{"qualified glob", "cloud/alpha-*", "cloud", "alpha-code", true},
		{"positive miss", "beta-*", "cloud", "alpha-code", false},
		{"exclude wins", "alpha-*,!*-vision", "cloud", "alpha-vision", false},
		{"exclusion only", "!*-vision", "cloud", "alpha-code", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := integrityModelSelected(test.patterns, test.provider, test.model); got != test.want {
				t.Fatalf("got %v want %v", got, test.want)
			}
		})
	}
}

func TestShareIntegrityPolicyOverridesProvider(t *testing.T) {
	p := &Proxy{}
	provider := store.Provider{Name: "cloud", IntegrityMode: "observe", IntegrityProfile: "code"}

	trace := &store.Trace{}
	if got := p.newOutputObservation(provider, "alpha", &store.Endpoint{IntegrityMode: "off"}, trace); got != nil {
		t.Fatal("share endpoint off must disable provider observation")
	}

	provider.IntegrityMode = "off"
	trace = &store.Trace{}
	got := p.newOutputObservation(provider, "alpha", &store.Endpoint{
		IntegrityMode: "observe", IntegrityModels: "alpha", IntegrityProfile: "multilingual",
	}, trace)
	if got == nil || trace.GuardMode != "observe" || trace.GuardProfile != "multilingual" {
		t.Fatalf("share observe did not override provider: observer=%v trace=%+v", got, trace)
	}

	trace = &store.Trace{}
	if got := p.newOutputObservation(provider, "beta", &store.Endpoint{
		IntegrityMode: "observe", IntegrityModels: "alpha-*",
	}, trace); got != nil || trace.GuardMode != "" {
		t.Fatalf("share model filter did not disable beta: observer=%v trace=%+v", got, trace)
	}

	provider.IntegrityMode = "observe"
	provider.IntegrityProfile = "code"
	trace = &store.Trace{}
	if got := p.newOutputObservation(provider, "alpha", &store.Endpoint{IntegrityMode: "inherit"}, trace); got == nil || trace.GuardProfile != "code" {
		t.Fatalf("share inherit did not use provider policy: observer=%v trace=%+v", got, trace)
	}
}
