package proxypool

import (
	"path/filepath"
	"testing"
	"time"

	"zenproxy/internal/config"
)

// The pass tier fixed WHICH exits serve free models; it did not fix WHICH of them
// serves each request. Measured on the six admitted exits 2026-10-17, admissions
// against mid-stream drops were static-161 131/32, static-1040 69/70, static-44
// 53/94 — a threefold spread in what a client actually experiences, while
// selection rotated them evenly. These tests pin the ranking that replaces the
// even rotation, its bounds, and the retry of an exit whose admission lapsed
// while its record did not.

func gateDropsN(t *testing.T, pool *Pool, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pool.MarkGateDrop(id, "stream ended without a completed response")
	}
}

func TestGateScoreWeightsAdmittedExitsByReliability(t *testing.T) {
	pool := createTestPool(t, 6, config.Health{})
	// Three admitted exits: one proven (caps at gateScoreMax), one fresh
	// (neutral), one that keeps dropping streams (floor).
	pool.MarkGatePass("a")
	for i := 0; i < gateScoreMax; i++ {
		pool.MarkGatePass("a")
	}
	pool.MarkGatePass("b")
	pool.MarkGatePass("c")
	gateDropsN(t, pool, "c", 6)

	count := map[string]int{}
	for i := 0; i < 300; i++ {
		px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true)
		if px == nil {
			t.Fatalf("pick %d: nil exit", i)
		}
		count[px.ID]++
	}
	good, fresh, bad := count["a"], count["b"], count["c"]
	if good == 0 || fresh == 0 || bad == 0 {
		t.Fatalf("an admitted exit was starved: %v", count)
	}
	// Weights are 3 / 2 / 1, so the proven exit should take about half the picks
	// and the dropping one about a sixth. Assert the ORDER, which is the whole
	// point, with enough slack that the hash scatter cannot flake it.
	if !(good > fresh && fresh >= bad) {
		t.Fatalf("weights did not rank the exits: %v", count)
	}
	if good < 2*bad {
		t.Fatalf("proven exit did not get a clear majority over the dropping one: %v", count)
	}
}

func TestGateScoreIsBoundedAndRecovers(t *testing.T) {
	pool := createTestPool(t, 3, config.Health{})
	pool.MarkGatePass("a")
	gateDropsN(t, pool, "a", 50)
	if got := pool.scoreOf("a"); got != gateScoreMin {
		t.Fatalf("score after 50 drops = %d, want floor %d", got, gateScoreMin)
	}
	// A bad episode must not condemn the exit forever: admissions walk it back.
	for i := 0; i < (gateScoreMax-gateScoreMin)/gateScorePass+2; i++ {
		pool.MarkGatePass("a")
	}
	if got := pool.scoreOf("a"); got != gateScoreMax {
		t.Fatalf("score after a clean run = %d, want cap %d", got, gateScoreMax)
	}
}

// An exit the gate has never admitted cannot be ranked: counting drops for it
// would make the column meaningless for the 1,600 exits that never carry a free
// request, and would give a name to a route the pool never takes.
func TestGateDropIgnoresNeverAdmittedExit(t *testing.T) {
	pool := createTestPool(t, 3, config.Health{})
	gateDropsN(t, pool, "b", 10)
	if got := pool.scoreOf("b"); got != 0 {
		t.Fatalf("score of a never-admitted exit = %d, want 0", got)
	}
}

// The admission TTL is 30 minutes; a clean IP stays clean for far longer.
// Measured: resin-slot-199 (126 admissions) and resin-slot-386 (13) simply aged
// out of the tier and were then only reachable when no admitted exit was left,
// so they never got the attempt that would re-admit them.
func TestLapsedAdmissionIsRetriedBeforeNeverAdmittedExits(t *testing.T) {
	pool := createTestPool(t, 8, config.Health{})
	for i := 0; i < 6; i++ {
		pool.MarkGatePass("d")
	}
	pool.mu.RLock()
	m := pool.byID["d"]
	pool.mu.RUnlock()
	m.mu.Lock()
	m.gatePassUntil = time.Time{} // admission lapsed, score intact
	m.mu.Unlock()

	for i := 0; i < 20; i++ {
		px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true)
		if px == nil {
			t.Fatalf("pick %d: nil exit", i)
		}
		if px.ID != "d" {
			t.Fatalf("pick %d: got %q, want the lapsed-but-proven exit d", i, px.ID)
		}
	}

	// The same exit is not preferred for plain traffic: without a live admission
	// the gate says nothing, so the ordinary ranking must be untouched.
	plain := map[string]bool{}
	for i := 0; i < 40; i++ {
		if px := pool.PickForAccount("", "deepseek-v4.1-flash", ""); px != nil {
			plain[px.ID] = true
		}
	}
	if len(plain) < 2 {
		t.Fatalf("lapsed admission leaked into plain selection: %v", plain)
	}

	// A lapsed exit with a mediocre record is not retried ahead of the rest.
	pool2 := createTestPool(t, 8, config.Health{})
	pool2.MarkGatePass("e")
	pool2.mu.RLock()
	m2 := pool2.byID["e"]
	pool2.mu.RUnlock()
	m2.mu.Lock()
	m2.gatePassUntil = time.Time{}
	m2.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if px := pool2.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true); px != nil {
			seen[px.ID] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("a merely-neutral exit was treated as lapsed-proven: %v", seen)
	}
}

// A park is a two-minute verdict; a never-admitted exit is a permanent one.
// Measured after the admission set was seeded: 129 of 134 refusals in one window
// came from never-admitted exits the pool only reached because every admitted
// exit was parked. Choosing a certain refusal over a stale admission is the same
// mistake as the earlier quarantine fallback, one tier down.
func TestGatedPickPrefersParkedAdmissionOverNeverAdmittedExit(t *testing.T) {
	pool := createTestPool(t, 6, config.Health{})
	pool.MarkGatePass("e")
	if !pool.BlockFreeTier("e", 2*time.Minute, "enveloped FreeTierError") {
		t.Fatal("BlockFreeTier returned false")
	}
	for i := 0; i < 12; i++ {
		px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true)
		if px == nil || px.ID != "e" {
			t.Fatalf("gated pick %d = %v, want the parked admitted exit e", i, px)
		}
	}
	// Non-gated traffic keeps the old rule: a parked exit is out of rotation.
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if px := pool.PickForAccount("", "deepseek-v4.1-flash", ""); px != nil {
			seen[px.ID] = true
		}
	}
	if seen["e"] {
		t.Fatalf("a parked exit served plain traffic: %v", seen)
	}
}

func TestGateScoreSurvivesRestart(t *testing.T) {
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
	for i := 0; i < 5; i++ {
		pool.MarkGatePass("b")
	}
	gateDropsN(t, pool, "b", 2)
	wantScore := pool.scoreOf("b")
	if wantScore != 5*gateScorePass-2*gateScoreDrop {
		t.Fatalf("unexpected score before save: %d", wantScore)
	}
	pool.SaveState()

	revived, err := NewPool(specs, hc, testLogger(), path)
	if err != nil {
		t.Fatalf("NewPool(restore): %v", err)
	}
	if got := revived.scoreOf("b"); got != wantScore {
		t.Fatalf("score after restart = %d, want %d", got, wantScore)
	}
	if got := revived.dropsOf("b"); got != 2 {
		t.Fatalf("drops after restart = %d, want 2", got)
	}
}

// scoreOf and dropsOf read the ranking counters under the member lock.
func (p *Pool) scoreOf(id string) int {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gateScore
}

func (p *Pool) dropsOf(id string) int {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gateDrops
}
