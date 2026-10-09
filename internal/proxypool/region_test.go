package proxypool

import (
	"testing"
	"time"
)

// A region refusal belongs to the exit-and-model pair, and that is the fact the
// whole feature is built on. Measured on the live pool: one exit answered 403
// RegionError for one free model and 200 for another in the same second.
func TestARegionBlockIsScopedToTheModelNotTheExit(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	if !pool.BlockRegion("a", "m-1") {
		t.Fatal("BlockRegion refused a known exit and a real model name")
	}
	if !pool.RegionBlocked("a", "m-1") {
		t.Fatal("the block did not read back")
	}
	if pool.RegionBlocked("a", "m-2") {
		t.Fatal("the block leaked onto another model on the same exit")
	}
	if pool.RegionBlocked("b", "m-1") {
		t.Fatal("the block leaked onto another exit for the same model")
	}
}

// An empty model name must not be stored as a wildcard. There is no evidence
// that "refused for everything" exists, and accepting it would quietly become a
// per-exit block through a different door — exactly the shape this is meant to
// avoid.
func TestABlockWithoutAModelNameIsRefused(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	if pool.BlockRegion("a", "") {
		t.Fatal("an empty model name was accepted")
	}
	if pool.BlockRegion("a", "   ") {
		t.Fatal("a blank model name was accepted")
	}
	if pool.RegionBlocked("a", "") {
		t.Fatal("an empty model name reported as blocked")
	}
	if got := pool.RegionBlocks(); len(got) != 0 {
		t.Fatalf("a refused write still left %d block(s) behind", len(got))
	}
}

// Unknown exits are refused rather than created: a typo must not silently
// produce a block that nothing will ever read.
func TestABlockOnAnUnknownExitIsRefused(t *testing.T) {
	pool, _ := healthyPool(t, 1)
	if pool.BlockRegion("nope", "m-1") {
		t.Fatal("a block was recorded against an exit that does not exist")
	}
}

// The upstream never says when a country reopens, so the proxy has to decide
// when to stop believing the block. Without an expiry an exit could never come
// back, and the only remedy would be a restart.
func TestARegionBlockExpiresOnItsOwn(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.BlockRegion("a", "m-1")

	// Age the record past the trust window directly; the alternative is a test
	// that sleeps six hours.
	px, _ := pool.Get("a")
	pool.mu.RLock()
	m := pool.byID[px.ID]
	pool.mu.RUnlock()
	m.mu.Lock()
	m.regionBlocked["m-1"] = time.Now().Add(-regionBlockTTL - time.Minute)
	m.mu.Unlock()

	if pool.RegionBlocked("a", "m-1") {
		t.Fatal("the block outlived its trust window")
	}
	if got := pool.RegionBlocks(); len(got) != 0 {
		t.Fatalf("an expired block is still listed: %v", got)
	}
}

// An exit refused for one model has to stay fully available for every other.
// This is the expensive direction of a mistake: scoping the block to the exit
// would take a working IP out of rotation for all its other models.
func TestAnExitRefusedForOneModelStillServesOthers(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.BlockRegion("a", "m-1")

	// Excluding b leaves only the blocked a, so this is a question about the
	// tiering and not about the pool running dry.
	for i := 0; i < 10; i++ {
		px := pool.PickFor("m-1", "b")
		if px == nil {
			t.Fatal("PickFor gave up while an exit remained")
		}
		if px.ID != "a" {
			t.Fatalf("PickFor chose %q, but a was the only exit left", px.ID)
		}
	}
	// For a different model the same exit is ordinary again, which is the
	// distinction: it was never demoted, only set aside for one model.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		seen[pool.PickFor("m-2", "").ID] = true
	}
	if !seen["a"] {
		t.Fatal("the exit blocked for m-1 was withheld for m-2 as well")
	}
}

// Clearing exists because a country reopening is not something the proxy can
// observe, only an operator can. It reports how many were live so the operator
// can tell "nothing was blocked" from "I cleared three".
func TestClearRegionBlocksForgetsThemAndSaysHowMany(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.BlockRegion("a", "m-1")
	pool.BlockRegion("a", "m-2")
	pool.BlockRegion("b", "m-1")

	if n := pool.ClearRegionBlocks("a"); n != 2 {
		t.Fatalf("ClearRegionBlocks cleared %d, want 2", n)
	}
	if pool.RegionBlocked("a", "m-1") || pool.RegionBlocked("a", "m-2") {
		t.Fatal("a cleared block still reads as blocked")
	}
	if !pool.RegionBlocked("b", "m-1") {
		t.Fatal("clearing one exit cleared another")
	}
	// Idempotent, and honest about it.
	if n := pool.ClearRegionBlocks("a"); n != 0 {
		t.Fatalf("a second clear reported %d, want 0", n)
	}
	if n := pool.ClearRegionBlocks("nope"); n != 0 {
		t.Fatalf("clearing an unknown exit reported %d, want 0", n)
	}
}

// The panel asks "is this model unavailable everywhere, or just from some
// exits", and it can only answer that if the listing names both halves of every
// pair.
func TestRegionBlocksListsEveryLivePairInAStableOrder(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.BlockRegion("b", "m-2")
	pool.BlockRegion("a", "m-1")

	got := pool.RegionBlocks()
	if len(got) != 2 {
		t.Fatalf("got %d blocks, want 2", len(got))
	}
	// Sorted by model, then exit: an unordered listing would make the panel
	// flicker between identical reads.
	if got[0].Model != "m-1" || got[0].ExitID != "a" {
		t.Fatalf("first entry = %s/%s, want m-1/a", got[0].Model, got[0].ExitID)
	}
	if got[1].Model != "m-2" || got[1].ExitID != "b" {
		t.Fatalf("second entry = %s/%s, want m-2/b", got[1].Model, got[1].ExitID)
	}
	for _, b := range got {
		if b.BlockedAt.IsZero() {
			t.Errorf("%s/%s has no timestamp", b.Model, b.ExitID)
		}
		if b.RetryAfter == "" {
			t.Errorf("%s/%s has no recheck hint", b.Model, b.ExitID)
		}
	}
}

// The trust window is what the operator is told to plan around, so it must not
// be zero: a zero TTL would make every block evaporate the instant it was
// recorded and the feature silently do nothing.
func TestTheTrustWindowIsUsable(t *testing.T) {
	pool, _ := healthyPool(t, 1)
	if got := pool.RegionBlockTTL(); got != regionBlockTTL {
		t.Fatalf("RegionBlockTTL = %v, want %v", got, regionBlockTTL)
	}
	if regionBlockTTL <= 0 {
		t.Fatalf("regionBlockTTL = %v; a block would never survive being written", regionBlockTTL)
	}
}

// A block and a quota cooldown on the same exit are independent, and the pool
// must not let one stand in for the other: they end by different mechanisms and
// a reader of either must see only its own.
func TestARegionBlockAndAQuotaCooldownDoNotImpersonateEachOther(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.BlockRegion("a", "m-1")

	if _, reason, cool := pool.QuotaCooled("a"); cool {
		t.Fatalf("a region block registered as a quota cooldown (reason %q)", reason)
	}
	pool.CoolForQuota("a", time.Now().Add(time.Hour), "rate_limit")
	if !pool.RegionBlocked("a", "m-1") {
		t.Fatal("cooling for quota erased the region block")
	}
	pool.ClearQuotaCool("a")
	if !pool.RegionBlocked("a", "m-1") {
		t.Fatal("clearing the quota cooldown erased the region block")
	}
	// And the reverse, with a fresh cooldown so the check is about the clear
	// rather than about the cooldown that was already removed above.
	pool.CoolForQuota("a", time.Now().Add(time.Hour), "rate_limit")
	pool.ClearRegionBlocks("a")
	if _, _, cool := pool.QuotaCooled("a"); !cool {
		t.Fatal("clearing the region block erased the quota cooldown")
	}
}
