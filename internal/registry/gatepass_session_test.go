package registry

import (
	"testing"
	"time"
)

// The session table is restored from disk across restarts, so it carries
// bindings made before the pool knew which exits the free-tier gate admits.
// Measured after the admission set was seeded: 342 of 346 refusals came from
// exits OUTSIDE that set while every success came from inside it — stickiness
// was pinning conversations to exits the gate refuses. The gated path must
// therefore break a binding that is no longer admitted, while plain traffic
// keeps the stickiness it has always had.

func TestSeatFollowsFallbackLeavesAdmittedSeat(t *testing.T) {
	pool := poolOf(t, "a", "direct")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	r.setBinding("zen-01", "a")
	pool.MarkGatePass("a")
	r.SeatFollowsFallback("zen-01", "direct")
	if got := r.BoundExit("zen-01"); got != "a" {
		t.Fatalf("admitted seat moved to %s", got)
	}

	spent := poolOf(t, "spent", "direct")
	r2 := New("", spent, testLogger())
	if err := r2.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	r2.setBinding("zen-01", "spent")
	r2.SeatFollowsFallback("zen-01", "direct")
	if got := r2.BoundExit("zen-01"); got != "direct" {
		t.Fatalf("unadmitted seat stayed on %s", got)
	}
}

func TestGatedSessionRebindsOffUnadmittedExit(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !r.BindSession("zen-01", "sess", "muse-spark", "a") {
		t.Fatal("BindSession failed")
	}

	// No exit has been admitted yet: the gated path must behave exactly like
	// the ordinary sticky path, not invent a preference out of nothing.
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "a" {
		t.Fatalf("with no admitted exit the binding must hold, got %v", px)
	}

	// The gate admits c. The conversation is still bound to a, which the gate
	// does not serve, so the gated path moves it and the move sticks.
	pool.MarkGatePass("c")
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "c" {
		t.Fatalf("gated session did not move to the admitted exit, got %v", px)
	}
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "c" {
		t.Fatalf("rebind did not stick, got %v", px)
	}

	// Plain traffic on the same pool keeps its own stickiness: this is a
	// free-tier rule, not a global change of behaviour.
	if !r.BindSession("zen-01", "sess_plain", "some-paid-model", "a") {
		t.Fatal("BindSession(plain) failed")
	}
	if px := r.ExitForSession("zen-01", "sess_plain", "some-paid-model"); px == nil || px.ID != "a" {
		t.Fatalf("plain session must stay sticky to a, got %v", px)
	}
}

func TestGatedSessionKeepsAdmittedBinding(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pool.MarkGatePass("a")
	if !r.BindSession("zen-01", "sess", "muse-spark", "a") {
		t.Fatal("BindSession failed")
	}
	for i := 0; i < 3; i++ {
		if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "a" {
			t.Fatalf("hit %d: admitted binding must be kept, got %v", i, px)
		}
	}
}

func TestGatedSessionMovesOffRefusedExit(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pool.MarkGatePass("a")
	if !r.BindSession("zen-01", "sess", "muse-spark", "a") {
		t.Fatal("BindSession failed")
	}
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "a" {
		t.Fatalf("want admitted exit a, got %v", px)
	}
	// a is admitted but answered a request with a refusal, so it is parked
	// against free-tier traffic (admission alone does not survive a refusal).
	// The conversation must follow the pool instead of staying on a and paying
	// the refusal again.
	pool.BlockFreeTier("a", 2*time.Minute, "enveloped FreeTierError")
	pool.MarkGatePass("b")
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "b" {
		t.Fatalf("refused exit kept the binding, got %v", px)
	}
}

func TestGatedSessionRebindKeepsAccountSeat(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pool.MarkGatePass("b")
	pool.MarkGatePass("c")
	if err := r.Bind("zen-01", "b"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if !r.BindSession("zen-01", "old", "muse-spark", "a") {
		t.Fatal("BindSession failed")
	}
	px := r.ExitForSessionGated("zen-01", "old", "muse-spark")
	if px == nil || px.ID == "a" {
		t.Fatalf("session stayed off the admission set, got %v", px)
	}
	if got := r.BoundExit("zen-01"); got != "b" {
		t.Fatalf("account seat moved from b to %s", got)
	}
}

func TestGatedSessionHitDoesNotDragAccountSeat(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	r.SetSessionExitCache(NewSessionExitCache(time.Hour, 64))
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pool.MarkGatePass("b")
	pool.MarkGatePass("c")
	if err := r.Bind("zen-01", "b"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if !r.BindSession("zen-01", "sess", "muse-spark", "c") {
		t.Fatal("BindSession failed")
	}
	if px := r.ExitForSessionGated("zen-01", "sess", "muse-spark"); px == nil || px.ID != "c" {
		t.Fatalf("session left its admitted exit, got %v", px)
	}
	if got := r.BoundExit("zen-01"); got != "b" {
		t.Fatalf("account seat moved from b to %s", got)
	}
}
