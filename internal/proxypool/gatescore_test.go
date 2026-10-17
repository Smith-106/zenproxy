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
	// One admission is not a reputation. The parked-but-proven fallback only
	// applies once the exit has actually carried traffic; a single pass that is
	// immediately refused is revoked, not remembered.
	for i := 0; i < 6; i++ {
		pool.MarkGatePass("e")
	}
	if !pool.BlockFreeTier("e", 2*time.Minute, "enveloped FreeTierError") {
		t.Fatal("BlockFreeTier returned false")
	}
	if passing, _ := pool.GatePassing("e"); passing {
		t.Fatal("a refusal left the admission in force")
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

// A refusal revokes the live admission and steps the score down. Repeated
// refusals walk a capped score back under the proven line, after which the
// exit is no longer retried ahead of the rest of the pool. This is what stops
// an exit that used to pass (resin-slot-199, 126 admissions, score 16) from
// being handed every request once the gate has turned on it.
func TestRepeatedRefusalsEjectAFormerlyProvenExit(t *testing.T) {
	pool := createTestPool(t, 6, config.Health{})
	for i := 0; i < 8; i++ {
		pool.MarkGatePass("a")
	}
	if got := pool.scoreOf("a"); got < gateGoodScore {
		t.Fatalf("precondition score %d, want >= %d", got, gateGoodScore)
	}
	for i := 0; i < 8; i++ {
		// Each call extends the park by a moment, so each one counts.
		time.Sleep(time.Millisecond)
		if !pool.BlockFreeTier("a", 2*time.Minute, "enveloped FreeTierError") {
			t.Fatalf("block %d did not extend the park", i)
		}
	}
	if got := pool.scoreOf("a"); got >= gateGoodScore {
		t.Fatalf("score after repeated refusals = %d, still proven", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		if px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true); px != nil {
			seen[px.ID] = true
		}
	}
	if len(seen) < 2 || (len(seen) == 1 && seen["a"]) {
		t.Fatalf("refused exit still monopolises gated picks: %v", seen)
	}
}

// Retirement is permanent, and on this pool the streak that triggers it is
// usually caused by our own concentration: measured 2026-10-17, all four working
// exits retired within two minutes of each other at twenty consecutive
// "proxyconnect ... i/o timeout" / "Not Enough Connections" dial failures, after
// carrying hundreds of successful free-tier requests that same hour. With them
// gone the gate had no admitted exit left at all — 0 admissions in 375 attempts.
func TestProvenExitIsQuarantinedNotRetired(t *testing.T) {
	pool := createTestPool(t, 4, config.Health{FailureThreshold: 1, RetireAfter: 4,
		Quarantine: 30 * time.Second, MaxBackoff: time.Minute})
	for i := 0; i < 6; i++ {
		pool.MarkGatePass("a")
	}
	for i := 0; i < 6; i++ {
		pool.MarkFailure("a", `Post "https://opencode.ai/inference/v1/responses": proxyconnect tcp: i/o timeout`)
	}
	pool.mu.RLock()
	m, ok := pool.byID["a"]
	pool.mu.RUnlock()
	if !ok {
		t.Fatal("exit a vanished")
	}
	m.mu.Lock()
	retired, score := m.retired, m.gateScore
	passUntil := m.gatePassUntil
	m.mu.Unlock()
	if retired {
		t.Fatalf("a proven exit was retired permanently (score %d)", score)
	}
	if passUntil.IsZero() {
		t.Fatal("precondition: the exit should still hold a live admission")
	}

	// The exemption is scoped to the admission window, not granted for life: an
	// exit that stops being admitted for thirty minutes retires like any other.
	m.mu.Lock()
	m.gatePassUntil = time.Time{}
	m.mu.Unlock()
	for i := 0; i < 6; i++ {
		pool.MarkFailure("a", "proxyconnect tcp: i/o timeout")
	}
	m.mu.Lock()
	retiredAfterLapse := m.retired
	m.mu.Unlock()
	if !retiredAfterLapse {
		t.Fatal("an exit whose admission lapsed kept its retirement exemption forever")
	}

	// An exit with no free-tier history still retires: that is what retirement is
	// for, and it is what keeps the 1,600-strong flagged population from
	// resurfacing forever.
	pool2 := createTestPool(t, 4, config.Health{FailureThreshold: 1, RetireAfter: 4,
		Quarantine: 30 * time.Second, MaxBackoff: time.Minute})
	for i := 0; i < 6; i++ {
		pool2.MarkFailure("b", "proxyconnect tcp: i/o timeout")
	}
	pool2.mu.RLock()
	m2 := pool2.byID["b"]
	pool2.mu.RUnlock()
	m2.mu.Lock()
	retired2 := m2.retired
	m2.mu.Unlock()
	if !retired2 {
		t.Fatal("an unproven exit survived the retirement threshold")
	}
}

// A proven exit whose admission lapsed must stay reachable for gated requests
// even when a probe has it degraded or quarantined: the pool's own admission
// history outranks a probe that never speaks the gate.
// A real dial failure is not a probe false-negative. Measured 2026-10-17 21:09:
// the four revived exits absorbed 281 transport attempts and completed zero,
// because quarantine put them in the impaired tier and the next gated pick
// took them straight back. After a proxyconnect failure they must leave the
// front of the queue.
func TestRequestDialFailureLeavesTheGatedFront(t *testing.T) {
	pool := createTestPool(t, 6, config.Health{FailureThreshold: 3,
		Quarantine: 10 * time.Minute, MaxBackoff: time.Minute})
	for i := 0; i < 6; i++ {
		pool.MarkGatePass("a")
	}
	pool.MarkFailure("a", `Post "https://opencode.ai/inference/v1/responses": proxyconnect tcp: dial tcp 10.0.0.1:3129: i/o timeout`)
	if !pool.RequestSick("a") {
		t.Fatal("a dial timeout was not recorded as a request failure")
	}
	if ok, _ := pool.Healthy("a"); !ok {
		t.Fatal("precondition: one failure is under the threshold, a stays healthy")
	}
	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true)
		if px == nil {
			t.Fatal("nil pick")
		}
		seen[px.ID]++
	}
	if seen["a"] == 20 {
		t.Fatalf("a dial-sick exit still took every gated pick: %v", seen)
	}
}

func TestProvenExitStaysVisibleWithoutLiveAdmission(t *testing.T) {
	pool := createTestPool(t, 6, config.Health{FailureThreshold: 1,
		Quarantine: 10 * time.Minute, MaxBackoff: time.Minute})
	for i := 0; i < 6; i++ {
		pool.MarkGatePass("c")
	}
	pool.mu.RLock()
	m := pool.byID["c"]
	pool.mu.RUnlock()
	m.mu.Lock()
	m.gatePassUntil = time.Time{} // admission lapsed
	m.mu.Unlock()
	pool.MarkFailure("c", "unreachable: dial tcp: i/o timeout") // probe, not a real request

	for i := 0; i < 10; i++ {
		px := pool.PickForAccountGated("", "muse-spark-1.2-contributor-free", "", true)
		if px == nil || px.ID != "c" {
			t.Fatalf("gated pick %d = %v, want proven exit c", i, px)
		}
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
