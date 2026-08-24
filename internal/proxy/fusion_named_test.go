package proxy

import (
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// A fusion id reaches cfrproxy by two spellings: "fusion:NAME" (matching the
// "auto:NAME" router convention) and "fusion/NAME" (what a harness produces
// when it registers the fusion mount as a provider). Both are the same fusion
// and both must resolve, or the mount silently 503s on every request.
func TestFusionSpecAcceptsBothSpellings(t *testing.T) {
	cases := []struct {
		in     string
		name   string
		isFuse bool
	}{
		{"fusion", "", true},                   // unnamed default
		{"fusion:deep", "deep", true},          // router-style
		{"fusion/deep", "deep", true},          // provider-mount style
		{"cfrproxy-fusion/deep", "deep", true}, // harness provider name
		{"grok/fusion", "", true},              // provider-qualified default
		{"FUSION:Deep", "Deep", true},          // case-insensitive prefix
		{"fusionary", "", false},               // must not prefix-match
		{"grok/grok-4.5", "", false},           // ordinary model
		{"", "", false},                        // empty
		{"auto:code", "", false},               // a router, not a fusion
	}
	for _, c := range cases {
		name, ok := fusionSpec(c.in)
		if ok != c.isFuse || name != c.name {
			t.Errorf("fusionSpec(%q) = (%q,%v), want (%q,%v)", c.in, name, ok, c.name, c.isFuse)
		}
	}
}

// The mount name is what a harness types into a custom-provider base URL.
func TestIsFusionMount(t *testing.T) {
	for _, s := range []string{"fusion", "Fusion", "cfrproxy-fusion", "CFRPROXY-FUSION"} {
		if !isFusionMount(s) {
			t.Errorf("isFusionMount(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"grok", "fred", "", "fusions"} {
		if isFusionMount(s) {
			t.Errorf("isFusionMount(%q) = true, want false", s)
		}
	}
}

// A named fusion must behave like a named router: disabled or incomplete means
// "not found" so the request falls through to normal routing, rather than
// running a pipeline with no judge and returning nothing.
func TestNamedFusionConfigRejectsIncomplete(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)

	good := store.Fusion{Name: "deep", Enabled: true, Judge: "grok/grok-4.5",
		Participants: []string{"a/b", "c/d"}, MaxTokens: 1234}
	if err := s.SaveFusion(&good); err != nil {
		t.Fatal(err)
	}
	off := store.Fusion{Name: "off", Enabled: false, Judge: "grok/grok-4.5",
		Participants: []string{"a/b"}}
	if err := s.SaveFusion(&off); err != nil {
		t.Fatal(err)
	}

	c, ok := p.NamedFusionConfig("deep")
	if !ok {
		t.Fatal("enabled fusion should resolve")
	}
	if c.Judge != "grok/grok-4.5" || len(c.Participants) != 2 || c.MaxTokens != 1234 {
		t.Fatalf("config not carried through: %+v", c)
	}
	if _, ok := p.NamedFusionConfig("off"); ok {
		t.Error("disabled fusion must not resolve")
	}
	if _, ok := p.NamedFusionConfig("nope"); ok {
		t.Error("unknown fusion must not resolve")
	}
	// case-insensitive, like RouterByName
	if _, ok := p.NamedFusionConfig("DEEP"); !ok {
		t.Error("lookup should be case-insensitive")
	}
}

// A fusion with no max_tokens inherits a budget rather than defaulting to zero,
// which would make every participant return an empty draft.
func TestNamedFusionInheritsTokenBudget(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)
	f := store.Fusion{Name: "nb", Enabled: true, Judge: "j/j",
		Participants: []string{"a/b"}}
	if err := s.SaveFusion(&f); err != nil {
		t.Fatal(err)
	}
	c, ok := p.NamedFusionConfig("nb")
	if !ok {
		t.Fatal("should resolve")
	}
	if c.MaxTokens <= 0 {
		t.Fatalf("max_tokens = %d, want a nonzero inherited budget", c.MaxTokens)
	}
}

// The name becomes part of a model id, so a separator in it would split wrong
// downstream — same rule routers enforce.
func TestSaveFusionValidates(t *testing.T) {
	s := newDiscoveryStore(t)
	bad := []store.Fusion{
		{Name: "", Judge: "j/j", Participants: []string{"a/b"}},
		{Name: "has space", Judge: "j/j", Participants: []string{"a/b"}},
		{Name: "has/slash", Judge: "j/j", Participants: []string{"a/b"}},
		{Name: "has:colon", Judge: "j/j", Participants: []string{"a/b"}},
		{Name: "nojudge", Participants: []string{"a/b"}},
		{Name: "noparts", Judge: "j/j"},
	}
	for _, f := range bad {
		f := f
		if err := s.SaveFusion(&f); err == nil {
			t.Errorf("SaveFusion(%+v) should have failed", f)
		}
	}
	ok := store.Fusion{Name: "fine", Enabled: true, Judge: "j/j", Participants: []string{"a/b", " ", "c/d"}}
	if err := s.SaveFusion(&ok); err != nil {
		t.Fatalf("valid fusion rejected: %v", err)
	}
	if len(ok.Participants) != 2 {
		t.Fatalf("blank participant should be dropped, got %v", ok.Participants)
	}
}

// The fusion mount exists so a harness can register one provider whose whole
// catalogue is the fusions, instead of pointing at the global mount.
func TestFusionModelIDsListsNamedFusions(t *testing.T) {
	s := newDiscoveryStore(t)
	p := New(s)
	for _, f := range []store.Fusion{
		{Name: "deep", Enabled: true, Judge: "j/j", Participants: []string{"a/b"}},
		{Name: "off", Enabled: false, Judge: "j/j", Participants: []string{"a/b"}},
	} {
		f := f
		if err := s.SaveFusion(&f); err != nil {
			t.Fatal(err)
		}
	}
	ids := p.fusionModelIDs(false)
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["fusion:deep"] {
		t.Errorf("enabled fusion missing from %v", ids)
	}
	if found["fusion:off"] {
		t.Errorf("disabled fusion listed in %v", ids)
	}
}
