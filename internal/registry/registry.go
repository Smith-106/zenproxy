// Package registry owns the account table and the account -> exit binding.
//
// The binding is an *affinity*, not an identity mapping: an account stays on
// the exit it last succeeded through so that long conversations do not hop
// networks mid-stream, and that affinity is dropped the moment the exit looks
// unhealthy. Exits are a shared availability pool, not a per-account identity
// slot.
package registry

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
)

// file is the on-disk state. It contains API keys, so it is written 0600.
//
// Bindings and stats are per replica. Both processes used to rewrite the whole
// document, so the later save erased the other side's affinity and counters,
// and a key edited on the panel (which is only published on replica A) could
// be put back to the old value by replica B.
type file struct {
	Version    int                      `json:"version"`
	Accounts   []model.Account          `json:"accounts"`
	Bindings   map[string]string        `json:"bindings"`
	Stats      map[string]*model.Stats  `json:"stats"`
	AccountRev map[string]time.Time     `json:"account_rev,omitempty"`
	Deleted    map[string]time.Time     `json:"deleted,omitempty"`
	Replicas   map[string]*replicaSlice `json:"replicas,omitempty"`
}

// replicaSlice is one process's affinity and counters. The account records
// above it stay shared, because a key has to be the same whichever replica
// answers.
type replicaSlice struct {
	SavedAt  time.Time               `json:"saved_at"`
	Bindings map[string]string       `json:"bindings,omitempty"`
	Stats    map[string]*model.Stats `json:"stats,omitempty"`
}

// ReplicaAccounts is the panel's view of one replica's counters. It carries
// no account id and no error text: those can quote an address.
type ReplicaAccounts struct {
	Name     string    `json:"name"`
	Self     bool      `json:"self"`
	Requests int64     `json:"requests"`
	Success  int64     `json:"success"`
	Failures int64     `json:"failures"`
	SavedAt  time.Time `json:"saved_at,omitempty"`
}

// Entry is one account plus its runtime state.
type Entry struct {
	model.Account
	Stats model.Stats
}

// View is the API-safe projection of an account: no credentials.
type View struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	Upstream        string `json:"upstream"`
	Pool            string `json:"pool"`
	Enabled         bool   `json:"enabled"`
	Proxy           string `json:"proxy"`
	Weight          int    `json:"weight"`
	HasKey          bool   `json:"has_key"`
	KeyHint         string `json:"key_hint,omitempty"`
	BoundExit       string `json:"bound_exit,omitempty"`
	BoundExitIP     string `json:"bound_exit_ip,omitempty"`
	BoundExitIPNote string `json:"bound_exit_ip_note,omitempty"`
	// These repeat the bound exit's row marks. The account table is four rows;
	// the exit table is the whole pool, and the marks were only on the exit.
	BoundSlow        bool              `json:"bound_slow,omitempty"`
	BoundGate        bool              `json:"bound_gate,omitempty"`
	BoundPrunable    bool              `json:"bound_prunable,omitempty"`
	BoundQuarantine  bool              `json:"bound_quarantine,omitempty"`
	BoundPeerRetired bool              `json:"bound_peer_retired,omitempty"`
	SessionPins      int               `json:"session_pins,omitempty"`
	SessionExits     int               `json:"session_exits,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Stats            model.Stats       `json:"stats"`
}

// Registry is the concurrency-safe account table.
type Registry struct {
	mu       sync.RWMutex
	accounts map[string]*Entry
	bindings map[string]string
	order    []string

	// rrCursor is the round-robin position per pool. It lives here rather than
	// in the caller so that two concurrent requests cannot both be told "you
	// are next", which is what a caller-side cursor would do.
	rrCursor map[string]int

	// sessions is the CPA-style session→exit sticky table. Nil / disabled TTL
	// means ExitForSession falls straight through to account affinity.
	sessions *SessionExitCache

	path string
	pool *proxypool.Pool
	log  *slog.Logger

	// replica namespaces bindings and stats. accountRev stamps an edit so a
	// stale sibling cannot write an older key back. deleted is the matching
	// tombstone.
	replica    string
	accountRev map[string]time.Time
	deleted    map[string]time.Time
}

// New creates an empty registry bound to a state file path.
func New(path string, pool *proxypool.Pool, log *slog.Logger) *Registry {
	return &Registry{
		accounts:   make(map[string]*Entry),
		bindings:   make(map[string]string),
		rrCursor:   make(map[string]int),
		path:       path,
		pool:       pool,
		log:        log,
		replica:    replicaName(),
		accountRev: make(map[string]time.Time),
		deleted:    make(map[string]time.Time),
	}
}

// SetReplica names this process's slice of the shared state file. Tests use it
// to put two registries on one machine; production uses the hostname chosen in
// New.
func (r *Registry) SetReplica(name string) {
	r.mu.Lock()
	r.replica = name
	r.mu.Unlock()
}

// SetSessionExitCache installs the session→exit sticky table. Safe to call
// once at startup; a nil cache disables session stickiness.
// SessionPinStats counts live conversation pins. The panel uses it to say
// that 换出口 only moves the account's current binding.
// RebindSessionPins moves one account's conversation pins onto exitID.
// Automatic rotation must not call this: it would drag every conversation
// off the exit that was still serving it.
func (r *Registry) RebindSessionPins(accountID, exitID string) int {
	if r == nil || r.sessions == nil {
		return 0
	}
	return r.sessions.RebindAccount(accountID, exitID)
}

func (r *Registry) SessionPinStats() SessionPinStats {
	if r == nil || r.sessions == nil {
		return SessionPinStats{}
	}
	return r.sessions.PinStats()
}

func (r *Registry) SetSessionExitCache(c *SessionExitCache) {
	r.mu.Lock()
	r.sessions = c
	r.mu.Unlock()
}

// Load reads the state file. A missing file is not an error: the config's
// seed accounts will be applied instead.
func (r *Registry) Load() error {
	if r.path == "" {
		return nil
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state %s: %w", r.path, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse state %s: %w", r.path, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountRev == nil {
		r.accountRev = make(map[string]time.Time)
	}
	for id, rev := range f.AccountRev {
		r.accountRev[id] = rev
	}
	if r.deleted == nil {
		r.deleted = make(map[string]time.Time)
	}
	for id, at := range f.Deleted {
		r.deleted[id] = at
	}
	bindings := f.Bindings
	stats := f.Stats
	if f.Replicas != nil {
		// A file that already has per-replica slices must not hand A's counters
		// to B just because B has not saved yet. The top-level maps are only
		// the fallback for a file written before replicas existed.
		bindings, stats = nil, nil
		if sl := f.Replicas[r.replica]; sl != nil {
			bindings = sl.Bindings
			stats = sl.Stats
		}
	}
	for _, a := range f.Accounts {
		if a.ID == "" {
			continue
		}
		if at, gone := r.deleted[a.ID]; gone && !r.accountRev[a.ID].After(at) {
			continue
		}
		e := &Entry{Account: a}
		if st, ok := stats[a.ID]; ok && st != nil {
			e.Stats = *st
		}
		r.accounts[a.ID] = e
		r.order = append(r.order, a.ID)
	}
	for account, exit := range bindings {
		if exit == "" {
			continue
		}
		r.bindings[account] = exit
		if r.pool != nil {
			r.pool.Bind(exit, account)
		}
	}
	return nil
}

// Seed inserts accounts from the config that are not already known. Existing
// state always wins, so an operator editing the file by hand cannot clobber
// keys that were rotated through the admin API.
func (r *Registry) Seed(accounts []model.Account) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	added := 0
	for _, a := range accounts {
		if a.ID == "" {
			continue
		}
		if _, exists := r.accounts[a.ID]; exists {
			continue
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now().UTC()
		}
		r.accounts[a.ID] = &Entry{Account: a}
		r.order = append(r.order, a.ID)
		r.accountRev[a.ID] = time.Now().UTC()
		delete(r.deleted, a.ID)
		added++
	}
	return added
}

// Save writes the state atomically with 0600 permissions.
//
// The write is a merge, not a replacement. Account edits with a newer stamp
// win; bindings and counters are stored under this replica's name so the other
// process keeps its own. A sibling that saved a newer key while we were not
// looking is copied into memory, which is how replica B learns a rotation
// done on the panel without a restart.
func (r *Registry) Save() error {
	if r.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	unlock, err := lockState(r.path)
	if err != nil {
		return err
	}
	defer unlock()

	snap := r.snapshot()
	disk, err := readState(r.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	merged := mergeAccounts(snap, disk)
	merged.Replicas = disk.Replicas
	if merged.Replicas == nil {
		merged.Replicas = make(map[string]*replicaSlice)
	}
	merged.Replicas[snap.replica] = &replicaSlice{
		SavedAt:  time.Now().UTC(),
		Bindings: snap.bindings,
		Stats:    snap.stats,
	}
	// Top-level bindings and stats stay populated for a reader that has not
	// learned about replicas yet. They are this process's slice, not a union.
	merged.Bindings = snap.bindings
	merged.Stats = snap.stats

	body, err := json.MarshalIndent(merged.file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.adopt(merged.adopt)
	return nil
}

// Get returns one account entry.
func (r *Registry) Get(id string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.accounts[id]
	return e, ok
}

// List snapshots every account for the admin API, sorted by id.
func (r *Registry) List() []View {
	pins := r.accountPins()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]View, 0, len(r.accounts))
	for _, e := range r.accounts {
		out = append(out, r.viewLocked(e, pins))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// View projects a single account without credentials.
func (r *Registry) View(id string) (View, bool) {
	pins := r.accountPins()
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.accounts[id]
	if !ok {
		return View{}, false
	}
	return r.viewLocked(e, pins), true
}

func (r *Registry) accountPins() map[string]SessionAccountPins {
	if r.sessions == nil {
		return nil
	}
	return r.sessions.AccountPins()
}

func (r *Registry) viewLocked(e *Entry, pins map[string]SessionAccountPins) View {
	v := View{
		ID:        e.ID,
		Name:      e.Name,
		Upstream:  e.Upstream,
		Pool:      e.EffectivePool(),
		Enabled:   e.Enabled,
		Proxy:     e.Proxy,
		Weight:    e.Weight,
		HasKey:    e.APIKey != "",
		KeyHint:   keyHint(e.APIKey),
		BoundExit: r.bindings[e.ID],
		Headers:   e.Headers,
		Stats:     e.Stats,
	}
	if p, ok := pins[e.ID]; ok {
		v.SessionPins = p.Pins
		v.SessionExits = p.Exits
	}
	v.Stats.LastError = model.PanelReason(v.Stats.LastError)
	if v.BoundExit != "" && r.pool != nil {
		for _, h := range r.pool.Health() {
			if h.ID == v.BoundExit {
				v.BoundExitIP = h.ExitIP
				v.BoundExitIPNote = h.ExitIPNote
				v.BoundSlow = h.Slow
				v.BoundGate = h.GatePassUntil != nil
				v.BoundPrunable = h.Prunable
				v.BoundQuarantine = h.QuarantinedTill != nil
				v.BoundPeerRetired = h.PeerRetired
				break
			}
		}
	}
	return v
}

// keyHint exposes just enough of a key to tell two accounts apart.
func keyHint(key string) string {
	if len(key) <= 8 {
		return ""
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// Upsert creates or replaces an account. An empty APIKey keeps the stored one,
// so the admin UI can edit metadata without ever re-typing the key.
func (r *Registry) Upsert(a model.Account) error {
	if err := model.ValidateAccountID(a.ID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, exists := r.accounts[a.ID]
	if !exists {
		a.CreatedAt = time.Now().UTC()
		e = &Entry{Account: a}
		r.accounts[a.ID] = e
		r.order = append(r.order, a.ID)
		r.accountRev[a.ID] = time.Now().UTC()
		delete(r.deleted, a.ID)
		return nil
	}
	if a.APIKey == "" {
		a.APIKey = e.APIKey
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = e.CreatedAt
	}
	e.Account = a
	r.accountRev[a.ID] = time.Now().UTC()
	delete(r.deleted, a.ID)
	return nil
}

// Delete removes an account and releases its binding.
func (r *Registry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.accounts[id]; !ok {
		return false
	}
	delete(r.accounts, id)
	if exit, ok := r.bindings[id]; ok {
		if r.pool != nil {
			r.pool.Unbind(exit, id)
		}
		delete(r.bindings, id)
	}
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	r.deleted[id] = time.Now().UTC()
	delete(r.accountRev, id)
	return true
}

// PoolMembers lists the enabled accounts of one pool, sorted by id. This is the
// candidate set a gateway request chooses from, and it is deliberately a
// snapshot: the caller then asks for one member per attempt.
func (r *Registry) PoolMembers(poolID string) []*Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Entry
	for _, id := range r.order {
		e, ok := r.accounts[id]
		if !ok || !e.Enabled {
			continue
		}
		if e.EffectivePool() == poolID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Pools lists every pool the accounts produce, sorted.
func (r *Registry) Pools() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range r.accounts {
		id := e.EffectivePool()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// PickInPool chooses the next account to try.
//
// exclude names the accounts already attempted for this request, so a retry
// never lands back on the account that just failed. strategy is either
// "weighted" (probability proportional to weight, and therefore the right
// choice when one account is bigger than another) or anything else, which
// means round-robin over the remaining members.
//
// A nil result means the pool has nothing left worth trying.
func (r *Registry) PickInPool(poolID, strategy string, exclude map[string]bool) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	var members []*Entry
	for _, id := range r.order {
		e, ok := r.accounts[id]
		if !ok || !e.Enabled || e.EffectivePool() != poolID || exclude[id] {
			continue
		}
		members = append(members, e)
	}
	if len(members) == 0 {
		return nil
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	if strategy == "weighted" {
		return pickWeighted(members)
	}
	cur := r.rrCursor[poolID]
	pick := members[cur%len(members)]
	r.rrCursor[poolID] = (cur + 1) % len(members)
	return pick
}

// pickWeighted draws one member with probability proportional to its weight.
// An account with no weight counts as 1, so a config that never sets weights
// degrades to a uniform draw rather than to "only the first account".
func pickWeighted(members []*Entry) *Entry {
	total := 0
	for _, e := range members {
		total += e.Weight
	}
	if total <= 0 {
		return members[0]
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(total)))
	if err != nil {
		return members[0]
	}
	at := int(n.Int64())
	for _, e := range members {
		at -= e.Weight
		if at < 0 {
			return e
		}
	}
	return members[len(members)-1]
}

// ExitFor resolves the exit an account should use right now.
//
// Precedence:
//  1. an existing binding, as long as that exit is still usable — this is the
//     session affinity, so a long conversation does not change IP mid-stream;
//  2. the account's configured exit, when it names one and that exit is healthy;
//  3. a fresh pick from the pool.
//
// The configured exit is therefore a *preference*, not a life sentence: a
// sick pinned exit is abandoned rather than used forever, otherwise rotation
// could never rescue a pinned account.
//
// A nil result means "dial directly".
// ExitFor resolves the exit an account should egress through, without regard to
// the model. Callers that know the model should use ExitForModel.
func (r *Registry) ExitFor(accountID string) *proxypool.Proxy {
	return r.ExitForModel(accountID, "")
}

// ExitForModel resolves the exit an account should egress through for one model.
//
// The model matters because a region block belongs to the exit-and-model pair,
// not to the exit: measured, one exit answered 403 RegionError for one free
// model and 200 for another in the same second. Resolving without the model
// would keep handing back an exit the upstream is going to refuse.
func (r *Registry) ExitForModel(accountID, model string) *proxypool.Proxy {
	return r.exitForModel(accountID, model, false)
}

// ExitForModelGated is ExitForModel for a request the free-tier gate will
// judge. It only changes how the pool is asked to choose (see
// proxypool.PickForAccountGated); an account's explicitly configured exit still
// wins, because that is configuration, not a heuristic.
func (r *Registry) ExitForModelGated(accountID, model string) *proxypool.Proxy {
	return r.exitForModel(accountID, model, true)
}

func (r *Registry) exitForModel(accountID, model string, gated bool) *proxypool.Proxy {
	r.mu.Lock()
	e, ok := r.accounts[accountID]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	mode := e.Proxy
	bound := r.bindings[accountID]
	r.mu.Unlock()

	switch mode {
	case "direct", "none":
		return nil
	}
	if r.pool == nil || r.pool.Len() == 0 {
		return nil
	}

	if bound != "" {
		if px, ok := r.pool.Get(bound); ok {
			if healthy, _ := r.pool.Healthy(bound); healthy && r.exitServesModel(bound, model) {
				r.pool.Bind(bound, accountID)
				return px
			}
		}
	}

	if mode != "auto" && mode != "" {
		if px, ok := r.pool.Get(mode); ok {
			if healthy, _ := r.pool.Healthy(mode); healthy && r.exitServesModel(mode, model) {
				r.setBinding(accountID, mode)
				return px
			}
			r.log.Warn("preferred exit is unhealthy, falling back to the pool",
				"account", accountID, "exit", mode)
		} else {
			r.log.Warn("account references unknown exit, falling back to the pool",
				"account", accountID, "exit", mode)
		}
	}

	px := r.pool.PickForAccountGated(accountID, model, "", gated)
	if px == nil {
		return nil
	}
	r.setBinding(accountID, px.ID)
	return px
}

// exitServesModel reports whether a healthy exit is currently allowed to
// carry this model. Affinity used to check only Healthy, which kept handing
// back a region-blocked exit and forced every request to pay a real 403 (or
// rely on avoidSpentExit). Quota coolings are deliberately NOT checked here:
// avoidSpentExit owns that decision so it can refuse to churn when every
// exit is spent. An empty model means "cannot judge" and always allows.
func (r *Registry) exitServesModel(exitID, model string) bool {
	if r.pool == nil || strings.TrimSpace(model) == "" {
		return true
	}
	if r.pool.RegionBlocked(exitID, model) {
		return false
	}
	// Free-tier blocks are exit-scoped (same envelope, different IP). Keeping a
	// binding on a blocked exit made every rotation rediscover the same 403.
	if r.pool.FreeTierBlocked(exitID) {
		return false
	}
	return true
}

// BoundExit reports which exit an account is currently bound to, without
// resolving anything or moving the binding.
//
// It exists so that callers asking "is this account parked somewhere bad?" can
// ask it as a plain question. Resolving instead would answer a different
// question — "where would this account go next" — and would bind as a side
// effect, which is the wrong thing for a predicate that runs once per account
// in a pool while deciding which ones to skip.
func (r *Registry) BoundExit(accountID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindings[accountID]
}

// Bind pins an account to a specific exit immediately.
func (r *Registry) Bind(accountID, exitID string) error {
	if r.pool == nil || r.pool.Len() == 0 {
		return errors.New("no exits configured")
	}
	if _, ok := r.pool.Get(exitID); !ok {
		return fmt.Errorf("unknown exit %q", exitID)
	}
	if _, ok := r.Get(accountID); !ok {
		return fmt.Errorf("unknown account %q", accountID)
	}
	r.setBinding(accountID, exitID)
	return nil
}

// Rotate moves an account off excludeExit and returns the replacement.
func (r *Registry) Rotate(accountID, excludeExit string) *proxypool.Proxy {
	return r.RotateFor(accountID, excludeExit, "")
}

// RotateFor moves an account off excludeExit, avoiding exits the upstream
// refuses for this model, and returns the replacement.
//
// Without the model the replacement is chosen on health and quota alone, which
// means a rotation triggered by a region refusal can land on another exit that
// is refused for the same model — turning one wasted attempt into two, and the
// caller would see the same 403 with no sign that anything moved.
func (r *Registry) RotateFor(accountID, excludeExit, model string) *proxypool.Proxy {
	return r.rotateFor(accountID, excludeExit, model, false)
}

// RotateForGated is RotateFor for a request the free-tier gate will judge: the
// replacement may be an admitted exit even while health has it quarantined,
// because a probe failure is weaker evidence than a real 2xx through the same
// exit. See proxypool.PickForAccountGated for the measurement.
func (r *Registry) RotateForGated(accountID, excludeExit, model string) *proxypool.Proxy {
	return r.rotateFor(accountID, excludeExit, model, true)
}

func (r *Registry) rotateFor(accountID, excludeExit, model string, gated bool) *proxypool.Proxy {
	if r.pool == nil || r.pool.Len() == 0 {
		return nil
	}
	r.mu.Lock()
	if prev, ok := r.bindings[accountID]; ok && prev != "" {
		r.pool.Unbind(prev, accountID)
	}
	r.mu.Unlock()

	px := r.pool.PickForAccountGated(accountID, model, excludeExit, gated)
	if px == nil {
		return nil
	}
	r.setBinding(accountID, px.ID)
	r.mu.Lock()
	if e, ok := r.accounts[accountID]; ok {
		e.Stats.Rotations++
	}
	r.mu.Unlock()
	return px
}

func (r *Registry) setBinding(accountID, exitID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Release the exit this account is leaving. Bind only ever adds, so without
	// this the pool's per-exit account set grows a stale entry every time an
	// account moves, and a stale entry is indistinguishable from a real one:
	// measured in production, three accounts left four binding records behind,
	// and an exit reported an occupant that had moved off it long before. The
	// same oversight would eventually mark every exit as taken and silently
	// disable the preference for keeping accounts apart.
	if prev, ok := r.bindings[accountID]; ok && prev != "" && prev != exitID {
		if r.pool != nil {
			r.pool.Unbind(prev, accountID)
		}
	}
	r.bindings[accountID] = exitID
	if r.pool != nil {
		r.pool.Bind(exitID, accountID)
	}
}

// Record folds one attempt's outcome into the account statistics.
func (r *Registry) Record(accountID string, class model.ErrorClass, err error) {
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.accounts[accountID]
	if !ok {
		return
	}
	e.Stats.UpstreamMsgs++
	e.Stats.LastUsed = &now
	e.Stats.LastClass = class
	// The class decides, not the error: a caller that records a refusal
	// without attaching an error must not be counted as a success, or the
	// console would show a healthy account that the upstream keeps rejecting.
	if class == model.ClassOK {
		e.Stats.Success++
		e.Stats.LastSuccess = &now
		e.Stats.LastError = ""
		return
	}
	e.Stats.Failures++
	if err != nil {
		e.Stats.LastError = err.Error()
	} else {
		e.Stats.LastError = string(class)
	}
}

// BeginRequest counts a request against an account before it is sent.
func (r *Registry) BeginRequest(accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.accounts[accountID]; ok {
		e.Stats.Requests++
	}
}

// Stats returns a copy of one account's counters.
func (r *Registry) Stats(accountID string) (model.Stats, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.accounts[accountID]
	if !ok {
		return model.Stats{}, false
	}
	return e.Stats, true
}

// Snapshot returns the stats block for every account.
func (r *Registry) Snapshot() map[string]model.Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]model.Stats, len(r.accounts))
	for id, e := range r.accounts {
		out[id] = e.Stats
	}
	return out
}

// EnabledIDs lists ids of enabled accounts, for status output.
func (r *Registry) EnabledIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, id := range r.order {
		if e, ok := r.accounts[id]; ok && e.Enabled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

type accountSnap struct {
	replica  string
	accounts map[string]model.Account
	rev      map[string]time.Time
	deleted  map[string]time.Time
	bindings map[string]string
	stats    map[string]*model.Stats
	order    []string
}

type adopted struct {
	id      string
	account model.Account
	rev     time.Time
	drop    bool
}

type mergedState struct {
	file
	adopt []adopted
}

func (r *Registry) snapshot() accountSnap {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := accountSnap{
		replica:  r.replica,
		accounts: make(map[string]model.Account, len(r.accounts)),
		rev:      make(map[string]time.Time, len(r.accountRev)),
		deleted:  make(map[string]time.Time, len(r.deleted)),
		bindings: make(map[string]string, len(r.bindings)),
		stats:    make(map[string]*model.Stats, len(r.accounts)),
		order:    append([]string(nil), r.order...),
	}
	if s.replica == "" {
		s.replica = "local"
	}
	for id, e := range r.accounts {
		s.accounts[id] = e.Account
		st := e.Stats
		s.stats[id] = &st
	}
	for id, rev := range r.accountRev {
		s.rev[id] = rev
	}
	for id, at := range r.deleted {
		s.deleted[id] = at
	}
	for id, exit := range r.bindings {
		s.bindings[id] = exit
	}
	return s
}

func readState(path string) (file, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return file{}, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return file{}, err
	}
	return f, nil
}

func mergeAccounts(snap accountSnap, disk file) mergedState {
	byID := make(map[string]model.Account, len(disk.Accounts)+len(snap.accounts))
	for _, a := range disk.Accounts {
		if a.ID != "" {
			byID[a.ID] = a
		}
	}
	diskRev := disk.AccountRev
	if diskRev == nil {
		diskRev = map[string]time.Time{}
	}
	deleted := make(map[string]time.Time, len(disk.Deleted)+len(snap.deleted))
	for id, at := range disk.Deleted {
		deleted[id] = at
	}
	for id, at := range snap.deleted {
		if prev, ok := deleted[id]; !ok || at.After(prev) {
			deleted[id] = at
		}
	}

	var adopt []adopted
	for _, id := range snap.order {
		local, ok := snap.accounts[id]
		if !ok {
			continue
		}
		localRev := snap.rev[id]
		if at, gone := deleted[id]; gone && !localRev.After(at) {
			delete(byID, id)
			continue
		}
		chosen := local
		if diskA, onDisk := byID[id]; onDisk {
			chosen = preferAccount(local, diskA, localRev, diskRev[id])
			if diskRev[id].After(localRev) || chosen.APIKey != local.APIKey {
				rev := diskRev[id]
				if localRev.After(rev) {
					rev = localRev
				}
				adopt = append(adopt, adopted{id: id, account: chosen, rev: rev})
			}
		}
		byID[id] = chosen
	}
	for id, diskA := range byID {
		if _, ours := snap.accounts[id]; ours {
			continue
		}
		if at, gone := deleted[id]; gone && !diskRev[id].After(at) {
			delete(byID, id)
			adopt = append(adopt, adopted{id: id, drop: true, rev: at})
			continue
		}
		adopt = append(adopt, adopted{id: id, account: diskA, rev: diskRev[id]})
	}
	for id, at := range deleted {
		rev := diskRev[id]
		if localRev, ok := snap.rev[id]; ok && localRev.After(rev) {
			rev = localRev
		}
		if !rev.After(at) {
			delete(byID, id)
		}
	}

	accounts := make([]model.Account, 0, len(byID))
	seen := make(map[string]bool, len(byID))
	for _, id := range snap.order {
		if a, ok := byID[id]; ok && !seen[id] {
			accounts = append(accounts, a)
			seen[id] = true
		}
	}
	for _, a := range disk.Accounts {
		if seen[a.ID] {
			continue
		}
		if kept, ok := byID[a.ID]; ok {
			accounts = append(accounts, kept)
			seen[a.ID] = true
		}
	}
	revOut := make(map[string]time.Time, len(byID))
	for id := range byID {
		rev := diskRev[id]
		if local, ok := snap.rev[id]; ok && local.After(rev) {
			rev = local
		}
		if !rev.IsZero() {
			revOut[id] = rev
		}
	}
	delOut := make(map[string]time.Time)
	for id, at := range deleted {
		if _, ok := byID[id]; !ok && !at.IsZero() {
			delOut[id] = at
		}
	}
	return mergedState{
		file: file{
			Version:    1,
			Accounts:   accounts,
			AccountRev: revOut,
			Deleted:    delOut,
		},
		adopt: adopt,
	}
}

// preferAccount keeps the newer edit, and never replaces a stored key with an
// empty one. An empty key in an upsert means "leave the stored key alone".
func preferAccount(local, disk model.Account, localRev, diskRev time.Time) model.Account {
	newer, older := local, disk
	if diskRev.After(localRev) {
		newer, older = disk, local
	}
	if newer.APIKey == "" {
		newer.APIKey = older.APIKey
	}
	return newer
}

func (r *Registry) adopt(items []adopted) {
	if len(items) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		if r.accountRev[item.id].After(item.rev) && !item.rev.IsZero() {
			continue
		}
		if item.drop {
			delete(r.accounts, item.id)
			for i, oid := range r.order {
				if oid == item.id {
					r.order = append(r.order[:i], r.order[i+1:]...)
					break
				}
			}
			if !item.rev.IsZero() {
				r.deleted[item.id] = item.rev
			}
			continue
		}
		if e, ok := r.accounts[item.id]; ok {
			e.Account = item.account
		} else {
			r.accounts[item.id] = &Entry{Account: item.account}
			r.order = append(r.order, item.id)
		}
		if !item.rev.IsZero() {
			r.accountRev[item.id] = item.rev
		}
		delete(r.deleted, item.id)
	}
}

// ReplicaAccountCounters reports this process's counters from memory and each
// sibling's counters from the shared file. A sibling that has not saved for
// three minutes is a dead container, same rule as the exit-state files.
func (r *Registry) ReplicaAccountCounters() []ReplicaAccounts {
	r.mu.RLock()
	self := ReplicaAccounts{Name: r.replica, Self: true}
	for _, e := range r.accounts {
		self.Requests += e.Stats.Requests
		self.Success += e.Stats.Success
		self.Failures += e.Stats.Failures
	}
	path, replica := r.path, r.replica
	r.mu.RUnlock()

	out := []ReplicaAccounts{self}
	if path == "" {
		return out
	}
	disk, err := readState(path)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(disk.Replicas))
	for name := range disk.Replicas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == replica {
			continue
		}
		sl := disk.Replicas[name]
		if sl == nil || sl.SavedAt.IsZero() || time.Since(sl.SavedAt) > 3*time.Minute {
			continue
		}
		row := ReplicaAccounts{Name: name, SavedAt: sl.SavedAt}
		for _, st := range sl.Stats {
			if st == nil {
				continue
			}
			row.Requests += st.Requests
			row.Success += st.Success
			row.Failures += st.Failures
		}
		out = append(out, row)
	}
	return out
}

func replicaName() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "local"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ':
			return '-'
		default:
			return r
		}
	}, host)
}

func lockState(path string) (func(), error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if time.Now().After(deadline) {
			_ = os.Remove(lockPath)
			f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, fmt.Errorf("lock state %s: %w", path, err)
			}
			_ = f.Close()
			return func() { _ = os.Remove(lockPath) }, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// RunSaver snapshots state to disk periodically and on shutdown.
func (r *Registry) RunSaver(ctxDone <-chan struct{}, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctxDone:
			if err := r.Save(); err != nil {
				r.log.Error("final state save failed", "err", err)
			}
			return
		case <-t.C:
			if err := r.Save(); err != nil {
				r.log.Error("state save failed", "err", err)
			}
		}
	}
}
