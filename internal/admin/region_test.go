package admin

import (
	"net/http"
	"testing"
)

// An operator looking at a 403 needs to be able to answer "is this model
// unavailable everywhere, or only from some exits" without probing by hand.
// Nothing else in the panel exposes it: proxies and pools both describe exits as
// healthy, because a region-refused exit answers promptly and truthfully.
func TestStatusReportsRegionBlocksAsPairs(t *testing.T) {
	s, pool := quotaServer(t, "slot-01", "slot-02")
	if !pool.BlockRegion("slot-01", "muse-spark") {
		t.Fatal("the fixture could not record a region block")
	}

	body := decode(t, authed(s, http.MethodGet, "/admin/api/status", ""))
	region, ok := body["region"].(map[string]any)
	if !ok {
		t.Fatalf("status has no region block: %v", body)
	}
	if got := region["count"].(float64); got != 1 {
		t.Errorf("region.count = %v, want 1", got)
	}
	blocked, ok := region["blocked"].([]any)
	if !ok || len(blocked) != 1 {
		t.Fatalf("region.blocked = %v, want one entry", region["blocked"])
	}
	entry := blocked[0].(map[string]any)
	if entry["exit_id"] != "slot-01" {
		t.Errorf("exit_id = %v, want slot-01", entry["exit_id"])
	}
	// The model is the half that makes this useful. Reporting the exit alone
	// would suggest the exit is out of service, which is the exact misreading
	// the pair-scoped storage exists to prevent.
	if entry["model"] != "muse-spark" {
		t.Errorf("model = %v, want muse-spark", entry["model"])
	}
	if entry["recheck_after"] == nil || entry["recheck_after"] == "" {
		t.Errorf("no recheck hint on %v", entry)
	}
}

// The status must never imply the exit is unhealthy. A panel that showed a
// region-blocked exit as down would send an operator to debug a perfectly good
// proxy.
func TestARegionBlockDoesNotReportTheExitAsUnhealthy(t *testing.T) {
	s, pool := quotaServer(t, "slot-01")
	pool.BlockRegion("slot-01", "muse-spark")

	body := decode(t, authed(s, http.MethodGet, "/admin/api/proxies", ""))
	proxies, ok := body["exits"].([]any)
	if !ok || len(proxies) != 1 {
		t.Fatalf("exits = %v, want one entry", body["exits"])
	}
	px := proxies[0].(map[string]any)
	if healthy, _ := px["healthy"].(bool); !healthy {
		t.Errorf("a region-blocked exit reports healthy=%v; the block is not a health fact", px["healthy"])
	}
	// It must still be visible, though — as the models it cannot serve.
	for_, ok := px["region_blocked_for"].([]any)
	if !ok || len(for_) != 1 || for_[0] != "muse-spark" {
		t.Errorf("region_blocked_for = %v, want [muse-spark]", px["region_blocked_for"])
	}
	// And it must not be dressed up as a quota problem: the two have different
	// lifetimes and an operator who reads one as the other waits for nothing.
	if _, present := px["quota_until"]; present {
		t.Errorf("a region block was reported as a quota cooldown: %v", px["quota_until"])
	}
}

// Clearing exists because a country reopening cannot be observed from inside the
// proxy. Without it the only remedies would be waiting out the trust window or
// restarting the process.
func TestClearRegionEndsBlocksAndSaysHowMany(t *testing.T) {
	s, pool := quotaServer(t, "slot-01")
	pool.BlockRegion("slot-01", "muse-spark")
	pool.BlockRegion("slot-01", "another")

	body := decode(t, authed(s, http.MethodPost, "/admin/api/proxies/slot-01/clear-region", ""))
	if got := body["cleared"].(float64); got != 2 {
		t.Errorf("cleared = %v, want 2", got)
	}
	if changed, _ := body["changed"].(bool); !changed {
		t.Error("changed = false after clearing two live blocks")
	}
	if pool.RegionBlocked("slot-01", "muse-spark") {
		t.Error("the block survived the clear")
	}

	// Idempotent: a second call is a no-op the operator is told about, not an
	// error.
	body = decode(t, authed(s, http.MethodPost, "/admin/api/proxies/slot-01/clear-region", ""))
	if got := body["cleared"].(float64); got != 0 {
		t.Errorf("second clear reported %v, want 0", got)
	}
	if changed, _ := body["changed"].(bool); changed {
		t.Error("changed = true when nothing was cleared")
	}
}

// A typo must be told apart from a no-op, the same way uncool does it.
func TestClearRegionOnAnUnknownExitIsANotFound(t *testing.T) {
	s, _ := quotaServer(t, "slot-01")
	rec := authed(s, http.MethodPost, "/admin/api/proxies/nope/clear-region", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 so a typo is distinguishable from a no-op", rec.Code)
	}
}
