package proxy

import (
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// A scoped mount (/p/{provider}) has to correct a caller who addressed some
// other provider, but it must NOT mangle a model id that merely contains a
// slash. Command Code ids are vendor-qualified ("thinkingmachines/inkling"),
// and blindly stripping to the last segment leaves bare "inkling", which is
// ambiguous against "inkling-small" — the fuzzy match then declines and the
// request dies as `model "inkling" is not served`. Ids whose stripped form
// happened to be unique kept working, which is why this only bit some models.
func TestStripProviderPrefixOnlyStripsRealProviders(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)
	prov := store.Provider{Name: "grok", Type: "openai", BaseURL: "http://x", Enabled: true}
	if err := s.SaveProvider(&prov); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"real provider prefix is corrected", "grok/grok-4.5", "grok-4.5"},
		{"vendor prefix preserved", "thinkingmachines/inkling", "thinkingmachines/inkling"},
		{"ambiguous sibling preserved", "thinkingmachines/inkling-small", "thinkingmachines/inkling-small"},
		{"deepseek vendor id preserved", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
		{"meta vendor id preserved", "meta/muse-spark-1.2", "meta/muse-spark-1.2"},
		{"no slash", "gpt-5.6-luna", "gpt-5.6-luna"},
		{"empty", "", ""},
		{"leading slash is not a prefix", "/weird", "/weird"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.stripProviderPrefix(c.in); got != c.want {
				t.Fatalf("stripProviderPrefix(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
