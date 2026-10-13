package registry

import (
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
type SessionExitCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]sessionExitEntry
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
	sessionID = strings.TrimSpace(sessionID)
	if r.sessions == nil || !r.sessions.Enabled() || sessionID == "" {
		return r.ExitForModel(accountID, model)
	}

	if bound, ok := r.sessions.GetAndRefresh(accountID, sessionID, model); ok {
		if px, ok := r.pool.Get(bound); ok {
			if healthy, _ := r.pool.Healthy(bound); healthy && r.exitServesModel(bound, model) {
				// Keep the account binding aligned so avoidSpentExit / admin
				// views still see the live exit.
				r.setBinding(accountID, bound)
				r.pool.Bind(bound, accountID)
				if r.log != nil {
					r.log.Info("session-exit: cache hit",
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

	px := r.ExitForModel(accountID, model)
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
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" && r.sessions != nil {
		r.sessions.Invalidate(accountID, sessionID, model)
	}
	px := r.RotateFor(accountID, excludeExit, model)
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
