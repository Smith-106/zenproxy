package registry

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// poolOf builds a pool whose exits are never dialled by these tests.
func poolOf(t *testing.T, ids ...string) *proxypool.Pool {
	t.Helper()
	specs := make([]proxypool.Spec, 0, len(ids))
	for _, id := range ids {
		specs = append(specs, proxypool.Spec{ID: id, URL: "http://127.0.0.1:9"})
	}
	pool, err := proxypool.NewPool(specs, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool
}

func account(id, key string) model.Account {
	return model.Account{ID: id, Upstream: "zen", APIKey: key, Proxy: "auto", Enabled: true}
}

func TestLoadWithoutPathIsANoOp(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Load(); err != nil {
		t.Fatalf("Load with an empty path: %v", err)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "absent.json"), poolOf(t, "a"), testLogger())
	if err := r.Load(); err != nil {
		t.Fatalf("a missing state file must not be fatal: %v", err)
	}
}

func TestSeedOnlyAddsNewAccounts(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if n := r.Seed([]model.Account{account("zen-01", "k1"), account("zen-02", "k2")}); n != 2 {
		t.Fatalf("first seed added %d, want 2", n)
	}
	// Re-seeding must not clobber console edits.
	if err := r.Upsert(account("zen-01", "rotated-key")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if n := r.Seed([]model.Account{account("zen-01", "k1"), account("zen-03", "k3")}); n != 1 {
		t.Fatalf("second seed added %d, want 1", n)
	}
	e, _ := r.Get("zen-01")
	if e.APIKey != "rotated-key" {
		t.Errorf("seed clobbered an edited key: %q", e.APIKey)
	}
	if ids := r.EnabledIDs(); len(ids) != 3 {
		t.Errorf("EnabledIDs = %v", ids)
	}
}

func TestUpsertWithEmptyKeyPreservesTheStoredOne(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(account("zen-01", "sk-original")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// The console sends an empty api_key when the operator only toggles a field.
	if err := r.Upsert(model.Account{ID: "zen-01", Upstream: "zen", Proxy: "direct", Enabled: false}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	e, _ := r.Get("zen-01")
	if e.APIKey != "sk-original" {
		t.Errorf("api_key = %q, want the stored key to survive", e.APIKey)
	}
	if e.Proxy != "direct" || e.Enabled {
		t.Errorf("other fields were not applied: %+v", e.Account)
	}
}

func TestUpsertRejectsEmptyID(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(model.Account{Upstream: "zen"}); err == nil {
		t.Fatal("expected an error for a missing id")
	}
}

// The API must never hand a full key back to a caller.
func TestViewNeverExposesTheKey(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(account("zen-01", "sk-live-abcdefghijklmnop")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	v, ok := r.View("zen-01")
	if !ok {
		t.Fatal("View returned not-found")
	}
	if v.KeyHint == "" {
		t.Fatal("KeyHint should identify the key well enough to tell accounts apart")
	}
	if strings.Contains(v.KeyHint, "abcdefghijklmnop") {
		t.Fatalf("KeyHint leaked the secret body: %q", v.KeyHint)
	}
	// A short key has no safe hint.
	if err := r.Upsert(account("zen-02", "short")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if v, _ := r.View("zen-02"); v.KeyHint != "" {
		t.Errorf("a short key must not produce a hint, got %q", v.KeyHint)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")

	r := New(path, poolOf(t, "a", "b"), testLogger())
	if err := r.Upsert(account("zen-01", "sk-key-0000000001")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "b"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	r.BeginRequest("zen-01")
	r.Record("zen-01", model.ClassRateLimit, nil)
	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	// Windows reports 0666 for any writable file; the mode bits that matter
	// are enforced by the Linux deployment target.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600 (it holds API keys)", fi.Mode().Perm())
	}

	reloaded := New(path, poolOf(t, "a", "b"), testLogger())
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, ok := reloaded.Get("zen-01")
	if !ok {
		t.Fatal("account lost across a save/load cycle")
	}
	if e.APIKey != "sk-key-0000000001" {
		t.Errorf("api_key = %q", e.APIKey)
	}
	v, _ := reloaded.View("zen-01")
	if v.BoundExit != "b" {
		t.Errorf("binding lost: %q", v.BoundExit)
	}
	st, _ := reloaded.Stats("zen-01")
	if st.Requests != 1 || st.Failures != 1 {
		t.Errorf("stats lost: %+v", st)
	}
}

func TestReplicaSaveKeepsTheOtherSidesKeyAndStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stale := New(path, nil, testLogger())
	stale.SetReplica("zenproxy-b")
	if err := stale.Upsert(account("zen-01", "sk-key-old-value-0001")); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	if err := stale.Save(); err != nil {
		t.Fatalf("stale save: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	panel := New(path, nil, testLogger())
	panel.SetReplica("zenproxy-a")
	if err := panel.Load(); err != nil {
		t.Fatalf("panel load: %v", err)
	}
	if err := panel.Upsert(account("zen-01", "sk-key-new-value-0002")); err != nil {
		t.Fatalf("panel upsert: %v", err)
	}
	panel.BeginRequest("zen-01")
	if err := panel.Save(); err != nil {
		t.Fatalf("panel save: %v", err)
	}

	stale.BeginRequest("zen-01")
	stale.BeginRequest("zen-01")
	stale.Record("zen-01", model.ClassTransport, nil)
	if err := stale.Save(); err != nil {
		t.Fatalf("stale resave: %v", err)
	}
	got, ok := stale.Get("zen-01")
	if !ok || got.APIKey != "sk-key-new-value-0002" {
		t.Fatal("replica B kept the old key after A rotated it")
	}

	again := New(path, nil, testLogger())
	again.SetReplica("zenproxy-a")
	if err := again.Load(); err != nil {
		t.Fatalf("reload A: %v", err)
	}
	reloaded, ok := again.Get("zen-01")
	if !ok || reloaded.APIKey != "sk-key-new-value-0002" {
		t.Fatal("A's key did not survive B's save")
	}
	st, _ := again.Stats("zen-01")
	if st.Requests != 1 {
		t.Fatalf("A's request count = %d, want 1", st.Requests)
	}

	b := New(path, nil, testLogger())
	b.SetReplica("zenproxy-b")
	if err := b.Load(); err != nil {
		t.Fatalf("reload B: %v", err)
	}
	bst, _ := b.Stats("zen-01")
	if bst.Requests != 2 || bst.Failures != 1 {
		t.Fatalf("B's stats = %+v, want 2 requests and 1 failure", bst)
	}
	rows := panel.ReplicaAccountCounters()
	var sawB bool
	for _, row := range rows {
		if row.Name == "zenproxy-b" && !row.Self && row.Requests == 2 && row.Failures == 1 {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("panel counters = %+v, want B's 2 requests and 1 failure", rows)
	}
}

func TestDeleteRemovesTheAccount(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !r.Delete("zen-01") {
		t.Fatal("Delete returned false for an existing account")
	}
	if _, ok := r.Get("zen-01"); ok {
		t.Fatal("account still present after Delete")
	}
	if r.Delete("zen-01") {
		t.Fatal("Delete returned true for an already deleted account")
	}
}

// Affinity: a long conversation must not change IP just because the
// round-robin cursor moved on.
func TestExitForIsStickyPerAccount(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Upsert(account("zen-02", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	first := r.ExitFor("zen-01")
	if first == nil {
		t.Fatal("ExitFor returned nil with a populated pool")
	}
	for i := 0; i < 10; i++ {
		if got := r.ExitFor("zen-01"); got == nil || got.ID != first.ID {
			t.Fatalf("affinity broken: got %v, want %s", got, first.ID)
		}
	}
	// Other accounts advance the cursor without disturbing zen-01.
	for i := 0; i < 5; i++ {
		r.ExitFor("zen-02")
	}
	if got := r.ExitFor("zen-01"); got == nil || got.ID != first.ID {
		t.Fatalf("another account's activity moved zen-01's exit: %v", got)
	}
}

// Regression: an explicitly pinned exit used to be returned forever, which
// made rotation impossible for exactly the accounts an operator pinned.
func TestExitForAbandonsASickPinnedExit(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	if err := r.Upsert(model.Account{
		ID: "zen-01", Upstream: "zen", APIKey: "k", Proxy: "a", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if got := r.ExitFor("zen-01"); got == nil || got.ID != "a" {
		t.Fatalf("a healthy pinned exit must be honoured, got %v", got)
	}
	pool.MarkFailure("a", "connection refused") // threshold is 1 in this pool
	if ok, _ := pool.Healthy("a"); ok {
		t.Fatal("precondition: exit a should be quarantined")
	}
	got := r.ExitFor("zen-01")
	if got == nil {
		t.Fatal("ExitFor returned nil while a healthy exit existed")
	}
	if got.ID == "a" {
		t.Fatal("a quarantined pinned exit must be abandoned, not used forever")
	}
}

func TestExitForDirectModeSkipsThePool(t *testing.T) {
	pool := poolOf(t, "a")
	r := New("", pool, testLogger())
	if err := r.Upsert(model.Account{
		ID: "zen-01", Upstream: "zen", APIKey: "k", Proxy: "direct", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := r.ExitFor("zen-01"); got != nil {
		t.Fatalf("Proxy=direct must dial directly, got %v", got)
	}
}

// Affinity must not keep returning an exit that is region-blocked for the
// asked model. Healthy alone used to win, and every request then paid either
// avoidSpentExit or a real 403 before moving.
func TestExitForModelAbandonsARegionBlockedBinding(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pool.BlockRegion("a", "muse-spark-1.3-contributor-free")

	got := r.ExitForModel("zen-01", "muse-spark-1.3-contributor-free")
	if got == nil {
		t.Fatal("ExitForModel returned nil while healthy exit b remained")
	}
	if got.ID == "a" {
		t.Fatal("affinity kept a region-blocked exit for the asked model")
	}
	// Re-pin to a and ask for a different model: the block must not leak.
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("re-bind: %v", err)
	}
	if got := r.ExitForModel("zen-01", "big-pickle"); got == nil || got.ID != "a" {
		t.Fatalf("region block leaked onto another model: got %v", got)
	}
}

// Quota coolings stay on the binding so avoidSpentExit can decide whether a
// warmer exit exists. ExitForModel must not pre-empt that and churn into
// another spent exit on its own.
func TestExitForModelKeepsAQuotaCooledBinding(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pool.CoolForQuota("a", time.Now().Add(time.Hour), "429")

	got := r.ExitForModel("zen-01", "big-pickle")
	if got == nil || got.ID != "a" {
		t.Fatalf("ExitForModel moved off a cooled binding prematurely: got %v", got)
	}
}

func TestExitForEmptyPoolIsNil(t *testing.T) {
	r := New("", poolOf(t), testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := r.ExitFor("zen-01"); got != nil {
		t.Fatalf("an empty pool must fall back to direct, got %v", got)
	}
}

func TestRotateMovesToADifferentExit(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	next := r.Rotate("zen-01", "a")
	if next == nil {
		t.Fatal("Rotate returned nil")
	}
	if next.ID == "a" {
		t.Fatal("Rotate handed back the exit it was told to avoid")
	}
	if v, _ := r.View("zen-01"); v.BoundExit != next.ID {
		t.Errorf("binding not updated: %q", v.BoundExit)
	}
	st, _ := r.Stats("zen-01")
	if st.Rotations != 1 {
		t.Errorf("Rotations = %d, want 1", st.Rotations)
	}
	// Rotating a single-exit pool has nowhere to go but must still return it.
	only := New("", poolOf(t, "a"), testLogger())
	if err := only.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got := only.Rotate("zen-01", "a"); got == nil {
		t.Fatal("a single-exit pool must still return its only exit rather than failing")
	}
}

func TestBindRejectsUnknownTargets(t *testing.T) {
	pool := poolOf(t, "a")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "nope"); err == nil {
		t.Error("expected an error binding to an unknown exit")
	}
	if err := r.Bind("nope", "a"); err == nil {
		t.Error("expected an error binding an unknown account")
	}
}

func TestStatsSeparatesFailuresFromSuccesses(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	r.BeginRequest("zen-01")
	r.Record("zen-01", model.ClassOK, nil)
	r.BeginRequest("zen-01")
	r.Record("zen-01", model.ClassTierGate, nil)

	st, _ := r.Stats("zen-01")
	if st.Requests != 2 || st.Success != 1 || st.Failures != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if st.LastClass != model.ClassTierGate {
		t.Errorf("LastClass = %q", st.LastClass)
	}
	if st.LastError == "" {
		t.Error("LastError should describe the most recent failure")
	}
}

func TestEnabledIDsSkipsDisabledAccounts(t *testing.T) {
	r := New("", poolOf(t, "a"), testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Upsert(model.Account{ID: "zen-02", Upstream: "zen", APIKey: "k", Enabled: false}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	ids := r.EnabledIDs()
	if len(ids) != 1 || ids[0] != "zen-01" {
		t.Fatalf("EnabledIDs = %v", ids)
	}
}

// Two accounts on one exit are not two chances: they share the per-IP allowance
// and the region refusals, so the redundancy the pool exists to provide is gone.
func TestAccountsAreKeptOnSeparateExits(t *testing.T) {
	pool := poolOf(t, "a", "b", "c")
	r := New("", pool, testLogger())
	order := []string{"zen-01", "zen-02", "zen-03"}
	for _, id := range order {
		if err := r.Upsert(account(id, "k")); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	taken := map[string]string{}
	for _, id := range order {
		px := r.ExitFor(id)
		if px == nil {
			t.Fatalf("ExitFor(%s) returned nil with a populated pool", id)
		}
		if other, dup := taken[px.ID]; dup {
			t.Fatalf("%s and %s both ended up on exit %s", other, id, px.ID)
		}
		taken[px.ID] = id
	}
}

// Bind only ever adds, so the exit an account leaves has to be released
// explicitly. A stale occupant is indistinguishable from a live one, and enough
// of them would mark every exit as taken and quietly disable the separation.
func TestMovingAnAccountReleasesTheExitItLeftBehind(t *testing.T) {
	pool := poolOf(t, "a", "b")
	r := New("", pool, testLogger())
	if err := r.Upsert(account("zen-01", "k")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := r.Bind("zen-01", "a"); err != nil {
		t.Fatalf("Bind(a): %v", err)
	}
	if err := r.Bind("zen-01", "b"); err != nil {
		t.Fatalf("Bind(b): %v", err)
	}
	if got := r.BoundExit("zen-01"); got != "b" {
		t.Fatalf("BoundExit = %q, want b", got)
	}
	for _, h := range pool.Health() {
		for _, a := range h.BoundAccounts {
			if a == "zen-01" && h.ID == "a" {
				t.Fatal("exit a still lists zen-01 after the account moved to b")
			}
		}
	}
}

// The production failure, reproduced: an account only re-resolves its exit when the
// old one goes bad, and the replacement used to be chosen with no regard for what
// anyone else was using. With d free and b and c occupied, moving onto b or c is
// the bug -- one bad exit would have become two accounts sharing a single IP.
//
// Written so it fails without the fix: the round-robin cursor starts untouched
// because all three accounts are pre-bound, so an unfixed Pick would land on c.
func TestAnAccountWhoseExitGoesBadMovesToAnUnoccupiedOne(t *testing.T) {
	pool := poolOf(t, "a", "b", "c", "d")
	r := New("", pool, testLogger())
	for _, id := range []string{"zen-01", "zen-02", "zen-03"} {
		if err := r.Upsert(account(id, "k")); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	for _, b := range []struct{ acct, exit string }{
		{"zen-01", "a"}, {"zen-02", "b"}, {"zen-03", "c"},
	} {
		if err := r.Bind(b.acct, b.exit); err != nil {
			t.Fatalf("Bind(%s, %s): %v", b.acct, b.exit, err)
		}
	}
	pool.MarkFailure("a", "boom")
	if ok, _ := pool.Healthy("a"); ok {
		t.Fatal("the failure threshold is 1, so a must be quarantined by now")
	}
	got := r.ExitFor("zen-01")
	if got == nil {
		t.Fatal("ExitFor returned nil while d was healthy and unoccupied")
	}
	if got.ID != "d" {
		t.Fatalf("zen-01 moved onto %s, which another account already occupies", got.ID)
	}
}
