package proxypool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zenproxy/internal/config"
)

// A restart must not amnesty the pool: the retire verdict and the clocked
// blocks have to survive with their semantics intact, and expired deadlines
// must not come back from the dead.
func TestExitStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "exits-state.json")
	specs := []Spec{
		{ID: "ex-a", URL: "http://127.0.0.1:58001"},
		{ID: "ex-b", URL: "http://127.0.0.1:58002"},
	}

	p1, err := NewPool(specs, config.Health{}, testLogger(), path)
	if err != nil {
		t.Fatalf("pool1: %v", err)
	}
	if p1.statePath != path {
		t.Fatalf("statePath = %q, want %q", p1.statePath, path)
	}
	p1.BlockFreeTier("ex-a", 15*time.Minute, "enveloped FreeTierError")
	p1.Retire("ex-b", "chronic garbage")
	p1.SaveState()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if bytes.Contains(raw, []byte("0001-01-01")) {
		t.Fatalf("zero timestamps were persisted:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte(`"retired":true`)) && !bytes.Contains(raw, []byte(`"retired": true`)) {
		t.Fatalf("retired exit not marked:\n%s", raw)
	}

	p2, err := NewPool(specs, config.Health{}, testLogger(), path)
	if err != nil {
		t.Fatalf("pool2 (restart): %v", err)
	}
	if got := p2.RetiredCount(); got != 1 {
		t.Fatalf("after restart RetiredCount = %d, want 1", got)
	}
	if !p2.FreeTierBlocked("ex-a") {
		t.Fatal("free-tier block lost across restart")
	}
	if ok, known := p2.Healthy("ex-b"); !known || ok {
		t.Fatalf("retired exit usable after restart (ok=%v known=%v)", ok, known)
	}

	t.Run("revive persists as un-retired", func(t *testing.T) {
		if err := p2.Revive("ex-b"); err != nil {
			t.Fatalf("revive: %v", err)
		}
		p2.SaveState()
		p3, err := NewPool(specs, config.Health{}, testLogger(), path)
		if err != nil {
			t.Fatalf("pool3: %v", err)
		}
		if got := p3.RetiredCount(); got != 0 {
			t.Fatalf("after revive+restart RetiredCount = %d, want 0", got)
		}
	})
}

func TestSeedFromSharedFileDoesNotRewriteIt(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "exits-state.json")
	own := filepath.Join(dir, "exits-state-replica-a.json")
	specs := []Spec{{ID: "ex-b", URL: "http://127.0.0.1:58002"}}
	p1, err := NewPool(specs, config.Health{}, testLogger(), legacy)
	if err != nil {
		t.Fatalf("legacy pool: %v", err)
	}
	if err := p1.Retire("ex-b", "chronic garbage"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	p1.SaveState()
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}

	p2, err := NewPool(specs, config.Health{}, testLogger(), own)
	if err != nil {
		t.Fatalf("replica pool: %v", err)
	}
	if got := p2.RetiredCount(); got != 1 {
		t.Fatalf("seeded RetiredCount = %d, want 1", got)
	}
	after, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("reread legacy: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("seeding rewrote the shared file")
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatal("seeding created the replica file before anything changed")
	}
	p2.markDirty()
	p2.SaveState()
	if _, err := os.Stat(own); err != nil {
		t.Fatalf("replica file not written: %v", err)
	}
	afterSave, _ := os.ReadFile(legacy)
	if !bytes.Equal(before, afterSave) {
		t.Fatal("replica save rewrote the shared file")
	}
}

func TestStateSnapshotsSeeTheOtherReplica(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{ID: "ex-b", URL: "http://127.0.0.1:58002"}}
	a, err := NewPool(specs, config.Health{}, testLogger(), filepath.Join(dir, "exits-state-a.json"))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := NewPool(specs, config.Health{}, testLogger(), filepath.Join(dir, "exits-state-b.json"))
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if err := b.Retire("ex-b", "only b saw this"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	b.SaveState()
	a.markDirty()
	a.SaveState()

	snaps := a.StateSnapshots()
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snaps))
	}
	var self, other *ReplicaState
	for i := range snaps {
		if snaps[i].Self {
			self = &snaps[i]
		} else {
			other = &snaps[i]
		}
	}
	if self == nil || other == nil {
		t.Fatalf("missing self/other: %+v", snaps)
	}
	if self.Retired != 0 {
		t.Fatalf("self retired = %d, want 0", self.Retired)
	}
	if other.Retired != 1 || other.Exits != 1 {
		t.Fatalf("other = %+v, want 1 retired of 1", *other)
	}
}

func TestStateSnapshotsHideStaleReplicas(t *testing.T) {
	dir := t.TempDir()
	specs := []Spec{{ID: "ex-b", URL: "http://127.0.0.1:58002"}}
	a, err := NewPool(specs, config.Health{}, testLogger(), filepath.Join(dir, "exits-state-a.json"))
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	a.markDirty()
	a.SaveState()
	stale := []byte(`{"version":1,"saved_at":"2020-01-01T00:00:00Z","exits":{"ex-b":{"retired":true}}}`)
	for _, name := range []string{"exits-state-dead.json", "exits-state.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), stale, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snaps := a.StateSnapshots()
	if len(snaps) != 1 || !snaps[0].Self {
		t.Fatalf("snapshots = %+v, want only the live self file", snaps)
	}
}

func TestExpiredBlockNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exits-state.json")
	specs := []Spec{{ID: "ex-a", URL: "http://127.0.0.1:58001"}}
	p1, err := NewPool(specs, config.Health{}, testLogger(), path)
	if err != nil {
		t.Fatalf("pool1: %v", err)
	}
	p1.BlockFreeTier("ex-a", time.Hour, "enveloped FreeTierError")

	// Age the persisted deadline into the past through the sidecar types —
	// no time travel needed, just a direct document rewrite.
	p1.SaveState()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc struct {
		Exits map[string]struct {
			FreeTierUntil time.Time `json:"free_tier_until"`
		} `json:"exits"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if _, ok := doc.Exits["ex-a"]; !ok {
		t.Fatal("ex-a not persisted")
	}
	p1.mu.RLock()
	m := p1.byID["ex-a"]
	p1.mu.RUnlock()
	m.mu.Lock()
	m.freeTierUntil = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()
	p1.SaveState()

	p2, err := NewPool(specs, config.Health{}, testLogger(), path)
	if err != nil {
		t.Fatalf("pool2: %v", err)
	}
	if p2.FreeTierBlocked("ex-a") {
		t.Fatal("an expired free-tier block survived the restart as active")
	}
}
