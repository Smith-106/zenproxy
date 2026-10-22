package registry

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zenproxy/internal/proxypool"
)

// SessionExitCache is the CPA-style sticky table for egress:
//
//	account :: session :: model  →  exit
//
// Explicit conversation keys (session_id / prompt_cache_key / x-opencode-session)
// outrank the legacy account→exit binding. A hit is reused while the exit is
// still usable; a miss or an unusable bound exit falls through to the normal
// account affinity, then rebinds the session. Failover rewrites the mapping
// instead of pinning a dead exit.
//
// ---------------------------------------------------------------------------
// Cross-replica sync. The table is per-process memory; running two replicas
// behind one DNS name means each replica sees only half the traffic and would
// rebind every session it has never heard of — breaking exit affinity, and
// with it the upstream cache warmth (measured: 60% warm turns on a single
// replica collapsed to 1.2% after the replicas split the table). The table
// therefore syncs through a small JSON file beside the registry state file:
// every interval each replica atomically saves its full table (tmp+rename, so
// concurrent writers can never tear the file) and merges whatever any other
// replica has written. The merge is a union with newer-wins on expiry, so
// both replicas converge to the same union within two intervals with no
// coordination protocol. A replica that invalidates a binding (failover) may
// see it resurrect from the other replica's snapshot until its TTL lapses —
// harmless, because every cache hit re-checks pool health and falls through
// to a reselect the moment the bound exit is unusable.
type SessionExitCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	max      int
	entries  map[string]sessionExitEntry
	syncPath string
}

type sessionExitEntry struct {
	exit      string
	expiresAt time.Time
}

// NewSessionExitCache builds a TTL cache. ttl<=0 disables it (every lookup
// misses). max<=0 means unbounded until cleanup.
func NewSessionExitCache(ttl time.Duration, max int) *SessionExitCache {
	if max <= 0 {
		max = 4096
	}
	return &SessionExitCache{
		ttl:     ttl,
		max:     max,
		entries: make(map[string]sessionExitEntry),
	}
}

// Enabled reports whether session→exit stickiness is active.
func (c *SessionExitCache) Enabled() bool {
	return c != nil && c.ttl > 0
}

// SessionPinStats is the count the panel may show. It never includes a
// session id or an exit id.
type SessionPinStats struct {
	Live                   int
	Exits                  int
	AccountsOnSeveralExits int
}

// PinStats counts live pins. A hit rewrites the account's displayed binding,
// so an account pinned to several exits will be pulled back to whichever
// conversation speaks next.
func (c *SessionExitCache) PinStats() SessionPinStats {
	if c == nil || !c.Enabled() {
		return SessionPinStats{}
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	exits := map[string]struct{}{}
	perAccount := map[string]map[string]struct{}{}
	live := 0
	for key, ent := range c.entries {
		if now.After(ent.expiresAt) {
			continue
		}
		live++
		account := key
		if i := strings.Index(key, "::"); i >= 0 {
			account = key[:i]
		}
		exits[ent.exit] = struct{}{}
		set := perAccount[account]
		if set == nil {
			set = map[string]struct{}{}
			perAccount[account] = set
		}
		set[ent.exit] = struct{}{}
	}
	split := 0
	for _, set := range perAccount {
		if len(set) > 1 {
			split++
		}
	}
	return SessionPinStats{Live: live, Exits: len(exits), AccountsOnSeveralExits: split}
}

// SessionAccountPins is one account's pin count. Exit ids stay out of it.
type SessionAccountPins struct {
	Pins    int
	Exits   int
	NotGate int
}

// AccountPins counts live pins per account. The panel uses it so the operator
// can see which 换出口 button moves a pile, not just the pool-wide total.
func (c *SessionExitCache) AccountPins() map[string]SessionAccountPins {
	raw := c.accountPinExits()
	if raw == nil {
		return nil
	}
	out := make(map[string]SessionAccountPins, len(raw))
	for id, exits := range raw {
		p := SessionAccountPins{Exits: len(exits)}
		for _, n := range exits {
			p.Pins += n
		}
		out[id] = p
	}
	return out
}

// accountPinExits counts live pins per account and exit. The registry joins
// this with gate admission and must not put the exit ids on the API.
func (c *SessionExitCache) accountPinExits() map[string]map[string]int {
	if c == nil || !c.Enabled() {
		return nil
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]map[string]int{}
	for key, ent := range c.entries {
		if now.After(ent.expiresAt) || ent.exit == "" {
			continue
		}
		account := key
		if i := strings.Index(key, "::"); i >= 0 {
			account = key[:i]
		}
		slot := out[account]
		if slot == nil {
			slot = map[string]int{}
			out[account] = slot
		}
		slot[ent.exit]++
	}
	return out
}

// RebindAccount moves this account's live pins onto exitID and refreshes
// their TTL, so the next conversation cannot pull the account back to the
// exit it just left. Other accounts are left alone. Pins already on exitID
// are not counted. When a sync file is configured the table is written
// immediately; a sibling with an older expiry adopts the new exit.
func (c *SessionExitCache) RebindAccount(accountID, exitID string) int {
	if c == nil || !c.Enabled() || strings.TrimSpace(accountID) == "" || strings.TrimSpace(exitID) == "" {
		return 0
	}
	now := time.Now()
	prefix := accountID + "::"
	c.mu.Lock()
	n := 0
	for key, ent := range c.entries {
		if now.After(ent.expiresAt) || !strings.HasPrefix(key, prefix) || ent.exit == exitID {
			continue
		}
		ent.exit = exitID
		ent.expiresAt = now.Add(c.ttl)
		c.entries[key] = ent
		n++
	}
	path := c.syncPath
	c.mu.Unlock()
	if n > 0 && path != "" {
		_ = c.SyncOnce(path)
	}
	return n
}

func sessionExitKey(accountID, sessionID, model string) string {
	return accountID + "::" + sessionID + "::" + canonicalModelKey(model)
}

func canonicalModelKey(model string) string {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return "_"
	}
	return model
}

// GetAndRefresh returns the bound exit for a session key and extends its TTL.
func (c *SessionExitCache) GetAndRefresh(accountID, sessionID, model string) (string, bool) {
	if !c.Enabled() || strings.TrimSpace(sessionID) == "" {
		return "", false
	}
	key := sessionExitKey(accountID, sessionID, model)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[key]
	if !ok || now.After(ent.expiresAt) {
		if ok {
			delete(c.entries, key)
		}
		return "", false
	}
	ent.expiresAt = now.Add(c.ttl)
	c.entries[key] = ent
	return ent.exit, true
}

// Set binds a session key to an exit.
func (c *SessionExitCache) Set(accountID, sessionID, model, exitID string) {
	if !c.Enabled() || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(exitID) == "" {
		return
	}
	key := sessionExitKey(accountID, sessionID, model)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictExpiredLocked(now)
		if len(c.entries) >= c.max {
			c.evictOldestLocked()
		}
	}
	c.entries[key] = sessionExitEntry{exit: exitID, expiresAt: now.Add(c.ttl)}
}

// Invalidate drops one session binding when the bound exit failed over.
func (c *SessionExitCache) Invalidate(accountID, sessionID, model string) {
	if c == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	key := sessionExitKey(accountID, sessionID, model)
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidateExit drops every session binding that pointed at exitID.
func (c *SessionExitCache) InvalidateExit(exitID string) {
	if c == nil || exitID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, ent := range c.entries {
		if ent.exit == exitID {
			delete(c.entries, k)
		}
	}
}

func (c *SessionExitCache) evictExpiredLocked(now time.Time) {
	for k, ent := range c.entries {
		if now.After(ent.expiresAt) {
			delete(c.entries, k)
		}
	}
}

func (c *SessionExitCache) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, ent := range c.entries {
		if first || ent.expiresAt.Before(oldest) {
			oldestKey = k
			oldest = ent.expiresAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// ExitForSession resolves egress with CPA-style session stickiness.
//
// Precedence:
//  1. session cache hit whose exit is still usable for the model;
//  2. otherwise ExitForModel (account affinity / pool pick), then bind the
//     session to whatever came back.
//
// An empty sessionID disables the cache path entirely.
func (r *Registry) ExitForSession(accountID, sessionID, model string) *proxypool.Proxy {
	return r.exitForSession(accountID, sessionID, model, false)
}

// ExitForSessionGated is ExitForSession for a request the free-tier gate will
// judge, and it exists because stickiness and gate admission disagree.
//
// The cache is restored from disk across restarts, so it is full of bindings
// made before the pool learned which exits the gate admits. Sticky-wins then
// means every one of those conversations keeps landing on an exit the gate
// refuses, forever: measured right after the admission set was seeded, 342 of
// 346 refusals came from exits OUTSIDE the admission set while the six admitted
// exits served every single success. The binding, not the pick, was the
// bottleneck.
//
// So for a gated request the binding is only kept when it is still an admitted
// exit. Otherwise the session is rebound to a current admission, and the cost is
// exactly what stickiness was protecting — one upstream prompt cache — while the
// benefit is the difference between a refused turn and an answered one.
func (r *Registry) ExitForSessionGated(accountID, sessionID, model string) *proxypool.Proxy {
	return r.exitForSession(accountID, sessionID, model, true)
}

func (r *Registry) exitForSession(accountID, sessionID, model string, preferGatePass bool) *proxypool.Proxy {
	sessionID = strings.TrimSpace(sessionID)
	if r.sessions == nil || !r.sessions.Enabled() || sessionID == "" {
		return r.exitForModel(accountID, model, preferGatePass)
	}

	if bound, ok := r.sessions.GetAndRefresh(accountID, sessionID, model); ok {
		if px, ok := r.pool.Get(bound); ok {
			if healthy, _ := r.pool.Healthy(bound); healthy && r.exitServesModel(bound, model) {
				if preferGatePass {
					// Re-read the admission under the pool's lock on every hit:
					// a 30m admission does expire, and the exit may have been
					// refused since the session was bound to it.
					passing, _ := r.pool.GatePassing(bound)
					blocked := r.pool.FreeTierBlocked(bound)
					// Still healthy (under the failure threshold) but the last
					// real request could not dial. Keeping the pin here is how
					// a session spends every attempt on a proxy that is timing
					// out, while the exit never quite gets quarantined.
					sick := r.pool.RequestSick(bound)
					if !passing || blocked || sick {
						if alt := r.pool.PickForAccountGated(accountID, model, bound, true); alt != nil {
							altPassing, _ := r.pool.GatePassing(alt.ID)
							// Move for a live admission always. Also move off an
							// exit that just failed to carry a request, or is
							// parked, even when nobody else is currently admitted:
							// staying put only repeats the failure.
							if altPassing || sick || blocked {
								r.sessions.Set(accountID, sessionID, model, alt.ID)
								seat := "moved"
								if !r.seatAlreadyAdmitted(accountID, alt.ID) {
									r.setBinding(accountID, alt.ID)
									r.pool.Bind(alt.ID, accountID)
								} else {
									seat = "kept"
								}
								if r.log != nil {
									r.log.Info("session-exit: rebinding to a free-tier admitted exit",
										"account", accountID, "session", truncateSession(sessionID),
										"from", bound, "to", alt.ID, "model", model, "seat", seat)
								}
								return alt
							}
						}
					}
				}
				// A conversation stays on its own exit. The account seat stays
				// where it is when that seat is already a live admission:
				// measured when one old chat dragged zen-03 off the exit
				// it shared with the other accounts.
				if !r.seatAlreadyAdmitted(accountID, bound) {
					r.setBinding(accountID, bound)
					r.pool.Bind(bound, accountID)
				}
				if r.log != nil {
					r.log.Debug("session-exit: cache hit",
						"account", accountID, "session", truncateSession(sessionID), "exit", bound, "model", model)
				}
				return px
			}
		}
		r.sessions.Invalidate(accountID, sessionID, model)
		if r.log != nil {
			r.log.Info("session-exit: cache hit but exit unavailable, reselecting",
				"account", accountID, "session", truncateSession(sessionID), "exit", bound, "model", model)
		}
	}

	px := r.exitForModel(accountID, model, preferGatePass)
	if px != nil {
		r.sessions.Set(accountID, sessionID, model, px.ID)
		if r.log != nil {
			r.log.Info("session-exit: cache miss, new binding",
				"account", accountID, "session", truncateSession(sessionID), "exit", px.ID, "model", model)
		}
	}
	return px
}

// BindSession pins a session to a specific exit, bypassing rotation
// selection. The storm fallback uses this after a direct-exit success so the
// next request of the same conversation starts on the exit that already
// proved usable, instead of re-burning attempts on the flapping proxy layer.
// The pinned entry lives in the same TTL table as any normal binding, and the
// cache-hit path still checks pool health on every hit, so when direct turns
// sick (or its natural TTL lapses) the session flows back to rotation with
// no extra machinery.
// seatAlreadyAdmitted reports that the account is already parked on a
// different live free-tier admission. One conversation must not drag
// that seat along.
func (r *Registry) seatAlreadyAdmitted(accountID, nextID string) bool {
	if r.pool == nil {
		return false
	}
	cur := r.BoundExit(accountID)
	if cur == "" || cur == nextID {
		return false
	}
	passing, _ := r.pool.GatePassing(cur)
	if !passing {
		return false
	}
	healthy, known := r.pool.Healthy(cur)
	if !known || !healthy || r.pool.FreeTierBlocked(cur) || r.pool.RequestSick(cur) {
		return false
	}
	return true
}

func (r *Registry) BindSession(accountID, sessionID, model, exitID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || r.sessions == nil || !r.sessions.Enabled() || exitID == "" {
		return false
	}
	r.sessions.Set(accountID, sessionID, model, exitID)
	if r.log != nil {
		r.log.Info("session-exit: pinned after storm fallback",
			"account", accountID, "session", truncateSession(sessionID), "exit", exitID, "model", model)
	}
	return true
}

// RotateForSession moves off excludeExit and rebinds the session to the
// replacement, mirroring CPA's "sticky auth failed → reselect + rebind".
func (r *Registry) RotateForSession(accountID, sessionID, excludeExit, model string) *proxypool.Proxy {
	return r.rotateForSession(accountID, sessionID, excludeExit, model, false)
}

// RotateForSessionGated is RotateForSession for a request the free-tier gate
// will judge: the replacement comes from the gated pick, which can return an
// admitted exit that health has quarantined.
func (r *Registry) RotateForSessionGated(accountID, sessionID, excludeExit, model string) *proxypool.Proxy {
	return r.rotateForSession(accountID, sessionID, excludeExit, model, true)
}

func (r *Registry) rotateForSession(accountID, sessionID, excludeExit, model string, gated bool) *proxypool.Proxy {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" && r.sessions != nil {
		r.sessions.Invalidate(accountID, sessionID, model)
	}
	px := r.rotateFor(accountID, excludeExit, model, gated)
	if px != nil && sessionID != "" && r.sessions != nil {
		r.sessions.Set(accountID, sessionID, model, px.ID)
		if r.log != nil {
			r.log.Info("session-exit: rebound after rotate",
				"account", accountID, "session", truncateSession(sessionID), "exit", px.ID, "model", model)
		}
	}
	return px
}

func truncateSession(id string) string {
	if len(id) <= 24 {
		return id
	}
	return id[:24]
}

// ---------------------------------------------------------------------------
// File sync (see the cross-replica note at the top of this file).

type sessionFile struct {
	Version int                         `json:"version"`
	Entries map[string]sessionFileEntry `json:"entries"`
}

type sessionFileEntry struct {
	Exit      string `json:"exit"`
	ExpiresAt string `json:"expires_at"` // RFC3339, UTC
}

// SyncOnce merges any entries another replica has written to path, THEN saves
// the full table (atomic tmp+rename). The order matters: save-first would
// clobber the shared file with a possibly-empty table and destroy the union;
// merge-first means a cold boot adopts the union before it ever writes.
func (c *SessionExitCache) SyncOnce(path string) error {
	if c == nil || !c.Enabled() || path == "" {
		return nil
	}
	if err := c.mergeFile(path); err != nil {
		return fmt.Errorf("session-sync: merge %s: %w", path, err)
	}
	if err := c.saveSnapshot(path); err != nil {
		return fmt.Errorf("session-sync: save %s: %w", path, err)
	}
	return nil
}

// WarmFromDisk adopts the shared table without writing — the boot-time path.
func (c *SessionExitCache) WarmFromDisk(path string) error {
	if c == nil || !c.Enabled() || path == "" {
		return nil
	}
	return c.mergeFile(path)
}

// StartFileSync keeps the table shared across replicas and returns for tests;
// the loop dies with the process. It warms the cache from disk before serving.
func (c *SessionExitCache) StartFileSync(path string, interval time.Duration, log *slog.Logger) {
	if c == nil || !c.Enabled() || path == "" || interval <= 0 {
		return
	}
	c.mu.Lock()
	c.syncPath = path
	c.mu.Unlock()
	// Warm start: adopt whatever a previous or sibling replica saved.
	if err := c.WarmFromDisk(path); err != nil && log != nil {
		log.Warn("session-sync: initial merge failed", "path", path, "err", err)
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if err := c.SyncOnce(path); err != nil && log != nil {
				log.Warn("session-sync step failed", "path", path, "err", err)
			}
		}
	}()
}

func (c *SessionExitCache) saveSnapshot(path string) error {
	c.mu.Lock()
	now := time.Now()
	f := sessionFile{Version: 1, Entries: make(map[string]sessionFileEntry, len(c.entries))}
	for k, ent := range c.entries {
		if now.After(ent.expiresAt) {
			continue
		}
		f.Entries[k] = sessionFileEntry{Exit: ent.exit, ExpiresAt: ent.expiresAt.UTC().Format(time.RFC3339)}
	}
	c.mu.Unlock()

	body, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *SessionExitCache) mergeFile(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first boot: nothing to adopt
		}
		return err
	}
	var f sessionFile
	if err := json.Unmarshal(body, &f); err != nil {
		return fmt.Errorf("session-sync: parse %s: %w", path, err)
	}
	now := time.Now()
	c.mu.Lock()
	for k, ent := range f.Entries {
		expires, err := time.Parse(time.RFC3339, ent.ExpiresAt)
		if err != nil || ent.Exit == "" || now.After(expires) {
			continue
		}
		// Union with newer-wins: a live TTL extension from the sibling beats
		// our stale copy; entries only we hold stay (the sibling merges them
		// on its next cycle).
		if local, ok := c.entries[k]; !ok || expires.After(local.expiresAt) {
			c.entries[k] = sessionExitEntry{exit: ent.Exit, expiresAt: expires}
		}
	}
	c.mu.Unlock()
	return nil
}
