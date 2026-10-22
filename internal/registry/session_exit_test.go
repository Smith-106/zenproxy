package registry

import (
	"path/filepath"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
)

// TestSessionFileSyncSharesPinsAcrossReplicas is the contract that keeps the
// dual-replica deploy from shredding exit affinity: both replicas must have
// the union of the sticky table within a couple of sync cycles, and a replica
// that boots late must warm up from disk.
func TestSessionFileSyncSharesPinsAcrossReplicas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a := NewSessionExitCache(time.Hour, 64)
	b := NewSessionExitCache(time.Hour, 64)

	// Replica a binds a live conversation, then shares it.
	a.Set("zen-01", "sessA", "muse", "direct")
	if err := a.SyncOnce(path); err != nil {
		t.Fatal(err)
	}
	// Replica b warms from disk and serves the same conversation with the
	// same egress — no rebind, no exit hop.
	if err := b.SyncOnce(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.GetAndRefresh("zen-01", "sessA", "muse"); !ok || got != "direct" {
		t.Fatalf("b did not adopt a's pin: got %v ok=%v", got, ok)
	}

	// b learns its own conversation; after its next sync, a adopts it too.
	b.Set("zen-02", "sessB", "muse", "socks-5")
	if err := b.SyncOnce(path); err != nil {
		t.Fatal(err)
	}
	if err := a.SyncOnce(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := a.GetAndRefresh("zen-02", "sessB", "muse"); !ok || got != "socks-5" {
		t.Fatalf("a did not adopt b's pin: got %v ok=%v", got, ok)
	}
	if got, ok := a.GetAndRefresh("zen-01", "sessA", "muse"); !ok || got != "direct" {
		t.Fatalf("a lost its own pin during merge: got %v ok=%v", got, ok)
	}

	// A fresh replica boots fully warm from the shared file.
	c := NewSessionExitCache(time.Hour, 64)
	if err := c.WarmFromDisk(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.GetAndRefresh("zen-01", "sessA", "muse"); !ok || got != "direct" {
		t.Fatalf("late boot did not warm from disk: got %v ok=%v", got, ok)
	}

	// A cache that has never written must not clobber the shared file when it
	// syncs (merge-first ordering, not save-first).
	d := NewSessionExitCache(time.Hour, 64)
	if err := d.SyncOnce(path); err != nil {
		t.Fatal(err)
	}
	e := NewSessionExitCache(time.Hour, 64)
	if err := e.WarmFromDisk(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := e.GetAndRefresh("zen-01", "sessA", "muse"); !ok || got != "direct" {
		t.Fatalf("empty replica destroyed the shared table: got %v ok=%v", got, ok)
	}
}

func TestExitForSessionStickyAcrossTurns(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	first := r.ExitForSession("zen-01", "pfx_same_conversation", "muse-spark")
	if first == nil {
		t.Fatal("ExitForSession returned nil")
	}
	for i := 0; i < 8; i++ {
		got := r.ExitForSession("zen-01", "pfx_same_conversation", "muse-spark")
		if got == nil || got.ID != first.ID {
			t.Fatalf("turn %d broke session stickiness: got %v want %s", i, got, first.ID)
		}
	}
	other := r.ExitForSession("zen-01", "pfx_other_conversation", "muse-spark")
	if other == nil {
		t.Fatal("other session got nil exit")
	}
	if got := r.ExitForSession("zen-01", "pfx_same_conversation", "muse-spark"); got == nil || got.ID != first.ID {
		t.Fatalf("other session disturbed the first: got %v want %s", got, first.ID)
	}
}

// BindSession pins a session to a named exit (the storm fallback's way of
// keeping a conversation on the direct exit that just worked). The pin obeys
// the same TTL table and the same health re-check as any binding: while the
// exit serves, hits return it; once it turns sick the next lookup reselects.
func TestBindSessionPinsThenSelfHeals(t *testing.T) {
	pool := poolOf(t, "a", "b", "direct")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !r.BindSession("zen-01", "pfx_pinned_conversation", "muse-spark", "direct") {
		t.Fatal("BindSession refused an eligible pin")
	}
	for i := 0; i < 5; i++ {
		px := r.ExitForSession("zen-01", "pfx_pinned_conversation", "muse-spark")
		if px == nil || px.ID != "direct" {
			t.Fatalf("turn %d did not honor the pin: got %+v", i, px)
		}
	}
	// Empty session or unknown exit is a no-op, not an error.
	if r.BindSession("zen-01", "", "muse-spark", "direct") {
		t.Fatal("empty session must not be pinnable")
	}

	// When the pinned exit turns sick the binding self-heals: the cache-hit
	// path checks health and falls through to a fresh selection.
	pool.MarkFailure("direct", "dial: refused")
	px := r.ExitForSession("zen-01", "pfx_pinned_conversation", "muse-spark")
	if px == nil || px.ID == "direct" {
		t.Fatalf("sick pinned exit must reselect, got %+v", px)
	}
}

func TestExitForSessionReselectsUnusableExit(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	got := r.ExitForSession("zen-01", "pfx_seed", "muse-spark")
	if got == nil || got.ID != "a" {
		t.Fatalf("seed want a, got %v", got)
	}
	pool.MarkFailure("a", "connection refused")
	if ok, _ := pool.Healthy("a"); ok {
		t.Fatal("precondition: a should be quarantined")
	}
	next := r.ExitForSession("zen-01", "pfx_seed", "muse-spark")
	if next == nil || next.ID == "a" {
		t.Fatalf("unusable sticky exit must be reselected, got %v", next)
	}
	again := r.ExitForSession("zen-01", "pfx_seed", "muse-spark")
	if again == nil || again.ID != next.ID {
		t.Fatalf("rebind did not stick: got %v want %s", again, next.ID)
	}
}

func TestRotateForSessionRebinds(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	first := r.ExitForSession("zen-01", "pfx_rot", "muse-spark")
	if first == nil {
		t.Fatal("nil first")
	}
	repl := r.RotateForSession("zen-01", "pfx_rot", first.ID, "muse-spark")
	if repl == nil || repl.ID == first.ID {
		t.Fatalf("rotate did not move: %v", repl)
	}
	got := r.ExitForSession("zen-01", "pfx_rot", "muse-spark")
	if got == nil || got.ID != repl.ID {
		t.Fatalf("session did not follow rotate: got %v want %s", got, repl.ID)
	}
}

// One proxyconnect timeout is under failure_threshold, so the exit stays
// healthy and the old pin would keep a gated session on it. That pin has to
// move: repeating the dial is an 8s timeout, not a cached conversation.
func TestGatedSessionLeavesDialSickExit(t *testing.T) {
	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "a", URL: "http://127.0.0.1:9"},
		{ID: "b", URL: "http://127.0.0.1:9"},
		{ID: "c", URL: "http://127.0.0.1:9"},
	}, config.Health{Timeout: time.Second, FailureThreshold: 3, Quarantine: time.Minute, MaxBackoff: time.Minute}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatal(err)
	}
	if !r.BindSession("zen-01", "pfx_sick", "muse-spark-1.2-contributor-free", "a") {
		t.Fatal("pin refused")
	}
	pool.MarkFailure("a", `Post "https://opencode.ai/inference/v1/responses": proxyconnect tcp: dial tcp 10.0.0.1:3129: i/o timeout`)
	px := r.ExitForSessionGated("zen-01", "pfx_sick", "muse-spark-1.2-contributor-free")
	if px == nil || px.ID == "a" {
		t.Fatalf("gated session stayed on a dial-sick exit: %v", px)
	}
}

func TestExitForSessionEmptyFallsThrough(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(model.Account{
		ID: "zen-01", Upstream: "zen", APIKey: "k", Proxy: "a", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := r.ExitForSession("zen-01", "", "muse-spark")
	if got == nil || got.ID != "a" {
		t.Fatalf("empty session must use account affinity, got %v", got)
	}
}

func TestSessionPinStatsCountsSplitAccounts(t *testing.T) {
	c := NewSessionExitCache(time.Hour, 64)
	c.Set("zen-01", "s1", "m", "a")
	c.Set("zen-01", "s2", "m", "b")
	c.Set("zen-02", "s3", "m", "a")
	got := c.PinStats()
	if got.Live != 3 || got.Exits != 2 || got.AccountsOnSeveralExits != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestRebindAccountMovesOnlyThatAccount(t *testing.T) {
	c := NewSessionExitCache(time.Hour, 64)
	c.Set("zen-01", "s1", "m", "a")
	c.Set("zen-01", "s2", "m", "b")
	c.Set("zen-02", "s3", "m", "c")
	if n := c.RebindAccount("zen-01", "b"); n != 1 {
		t.Fatalf("moved %d, want 1", n)
	}
	if got, ok := c.GetAndRefresh("zen-01", "s1", "m"); !ok || got != "b" {
		t.Fatalf("zen-01 s1 = %q ok=%v", got, ok)
	}
	if got, ok := c.GetAndRefresh("zen-02", "s3", "m"); !ok || got != "c" {
		t.Fatalf("zen-02 was moved: %q ok=%v", got, ok)
	}
	if n := c.RebindAccount("zen-01", "b"); n != 0 {
		t.Fatalf("second rebind moved %d", n)
	}
}

func TestAccountPinsCountsEachAccount(t *testing.T) {
	c := NewSessionExitCache(time.Hour, 64)
	c.Set("zen-01", "s1", "m", "a")
	c.Set("zen-01", "s2", "m", "b")
	c.Set("zen-02", "s3", "m", "a")
	got := c.AccountPins()
	if got["zen-01"].Pins != 2 || got["zen-01"].Exits != 2 || got["zen-02"].Pins != 1 || got["zen-02"].Exits != 1 {
		t.Fatalf("pins = %+v", got)
	}
}

func TestListCountsPinsOffTheGate(t *testing.T) {
	pool := poolOf(t, "a", "b")
	pool.MarkGatePass("a")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatal(err)
	}
	r.sessions.Set("zen-01", "s1", "m", "a")
	r.sessions.Set("zen-01", "s2", "m", "b")
	var got View
	for _, v := range r.List() {
		if v.ID == "zen-01" {
			got = v
		}
	}
	if got.SessionPins != 2 || got.SessionExits != 2 || got.SessionNotGate != 1 {
		t.Fatalf("pins=%d exits=%d not_gate=%d", got.SessionPins, got.SessionExits, got.SessionNotGate)
	}
}
