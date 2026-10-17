package proxypool

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zenproxy/internal/config"
)

// The free-tier gate refuses by exit population, not by load: measured on live
// traffic, every admission came from a handful of exits while hundreds were
// refused every time. These tests pin the consequence — a recently admitted
// exit must be preferred over every other healthy one, and must stop being
// preferred the moment it is refused or its admission goes stale.

func TestGatePasserIsPreferredOverHealthyExits(t *testing.T) {
	pool := createTestPool(t, 8, config.Health{})
	pool.MarkGatePass("c")

	for i := 0; i < 12; i++ {
		px := pool.PickForAccount("", "muse-spark-1.2-contributor-free", "")
		if px == nil {
			t.Fatalf("pick %d: nil exit", i)
		}
		if px.ID != "c" {
			t.Fatalf("pick %d: got %q, want the gate passer c", i, px.ID)
		}
	}
}

func TestGatePassRefusalDropsPreference(t *testing.T) {
	pool := createTestPool(t, 8, config.Health{})
	pool.MarkGatePass("c")
	// The gate admitted c once and then refused it: the block is the fresher
	// verdict, so c must leave the pool's choices entirely until it expires.
	if !pool.BlockFreeTier("c", 2*time.Minute, "enveloped FreeTierError") {
		t.Fatal("BlockFreeTier returned false")
	}
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		if px := pool.PickForAccount("", "muse-spark-1.2-contributor-free", ""); px != nil {
			seen[px.ID] = true
		}
	}
	if seen["c"] {
		t.Fatal("free-tier-blocked exit was still picked")
	}
	if len(seen) == 0 {
		t.Fatal("no exit picked at all")
	}
}

func TestGatePassExpiryRestoresNormalSelection(t *testing.T) {
	pool := createTestPool(t, 8, config.Health{})
	pool.MarkGatePass("c")
	passing, passes := pool.GatePassing("c")
	if !passing || passes != 1 {
		t.Fatalf("GatePassing = %v/%d, want true/1", passing, passes)
	}
	// Age the admission past its TTL by hand: the pick path must then stop
	// concentrating on c, otherwise a stale paser would pin every free request
	// to one IP for the process's lifetime.
	pool.mu.RLock()
	m := pool.byID["c"]
	pool.mu.RUnlock()
	m.mu.Lock()
	m.gatePassUntil = time.Now().Add(-time.Second)
	m.mu.Unlock()

	if passing, _ := pool.GatePassing("c"); passing {
		t.Fatal("expired admission still reported as passing")
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if px := pool.PickForAccount("", "muse-spark-1.2-contributor-free", ""); px != nil {
			seen[px.ID] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("selection did not spread after expiry: %v", seen)
	}
}

func TestGatePassSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exits-state.json")
	specs := []Spec{
		{ID: "a", URL: "http://127.0.0.1:9"},
		{ID: "b", URL: "http://127.0.0.1:9"},
	}
	hc := config.Health{Timeout: time.Second, Quarantine: time.Minute, MaxBackoff: time.Minute}
	pool, err := NewPool(specs, hc, testLogger(), path)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	pool.MarkGatePass("b")
	pool.SaveState()

	revived, err := NewPool(specs, hc, testLogger(), path)
	if err != nil {
		t.Fatalf("NewPool(restore): %v", err)
	}
	passing, passes := revived.GatePassing("b")
	if !passing || passes != 1 {
		t.Fatalf("after restart passing=%v passes=%d, want true/1", passing, passes)
	}
	if got := revived.GatePassers(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("GatePassers after restart = %v, want [b]", got)
	}
	if px := revived.PickForAccount("", "muse-spark-1.2-contributor-free", ""); px == nil || px.ID != "b" {
		t.Fatalf("restored pool did not prefer the passer: %v", px)
	}
}

func TestGatePassersListsOnlyCurrentAdmissions(t *testing.T) {
	pool := createTestPool(t, 5, config.Health{})
	pool.MarkGatePass("a")
	pool.MarkGatePass("b")
	if got := strings.Join(pool.GatePassers(), ","); got != "a,b" {
		t.Fatalf("GatePassers = %q, want a,b", got)
	}
	pool.mu.RLock()
	m := pool.byID["a"]
	pool.mu.RUnlock()
	m.mu.Lock()
	m.gatePassUntil = time.Time{}
	m.mu.Unlock()
	if got := strings.Join(pool.GatePassers(), ","); got != "b" {
		t.Fatalf("GatePassers after expiry = %q, want b", got)
	}
}
