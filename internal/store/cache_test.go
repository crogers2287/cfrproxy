package store

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func openPair(t *testing.T) (*Store, *Store) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfrproxy-store-cache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// Settings are served from memory; a write by another process (the CLI) is
// visible within the data_version probe window, and our own writes at once.
func TestSettingsCacheSeesOwnAndExternalWrites(t *testing.T) {
	a, b := openPair(t)
	if err := a.SetSetting("k", "own"); err != nil {
		t.Fatal(err)
	}
	if got := a.Setting("k"); got != "own" {
		t.Fatalf("own write not visible immediately: %q", got)
	}
	if err := b.SetSetting("k", "external"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Setting("k") != "external" {
		if time.Now().After(deadline) {
			t.Fatalf("external write never became visible: %q", a.Setting("k"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTransformAndEndpointCachesRefreshOnWrite(t *testing.T) {
	a, _ := openPair(t)
	if err := a.SaveTransform(&Transform{Name: "t1", Phase: "request", Rules: json.RawMessage(`[]`), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ts, _ := a.Transforms()
	if len(ts) != 1 || ts[0].Name != "t1" {
		t.Fatalf("transform not visible after save: %+v", ts)
	}
	if err := a.SaveEndpoint(&Endpoint{Name: "share1", APIKey: "cfr_x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if ep, ok := a.EndpointByName("share1"); !ok || ep.APIKey != "cfr_x" {
		t.Fatalf("endpoint not visible (decrypted) after save: %+v %v", ep, ok)
	}
	if err := a.DeleteEndpoint(1); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.EndpointByName("share1"); ok {
		t.Fatal("endpoint still visible after delete")
	}
}

// Retention runs on a timer, not per insert.
func TestPruneRetentionKeepsNewest(t *testing.T) {
	a, _ := openPair(t)
	old := KeepTraces
	KeepTraces = 10
	t.Cleanup(func() { KeepTraces = old })
	for i := 0; i < 25; i++ {
		a.AddTrace(&Trace{TS: time.Now().Unix(), Provider: "p", Model: "m", Status: 200})
	}
	if tr, _ := a.Traces(0, 100); len(tr) != 25 {
		t.Fatalf("inserts should not prune inline, have %d", len(tr))
	}
	a.PruneRetention()
	tr, _ := a.Traces(0, 100)
	if len(tr) != 10 || tr[0].ID != 25 {
		t.Fatalf("want the newest 10 traces, got %d (newest id %d)", len(tr), tr[0].ID)
	}
}
