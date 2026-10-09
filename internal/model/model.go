// Package model holds the shared data types of the zenproxy egress daemon.
//
// zenproxy exists to fill a gap in the gateways that front it: neither new-api
// nor LiteLLM can rotate a channel's outbound proxy, because the setting is a
// single static value. Whenever an exit dies, the whole channel dies with it.
// This daemon turns that static value into a health-checked pool and offers
// the gateway two ways to address one account:
//
//	reverse  http://<zenproxy>:9000/<account-id>/<upstream-path>
//	forward  http://<account-id>:x@<zenproxy>:9000
//
// Both ingresses share one listener and one account registry.
package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// reservedAccountIDs collide with zenproxy's own routes: an account named
// "admin" would be unreachable in reverse mode, and the gateway would get a
// confusing 404 instead of an upstream response.
var reservedAccountIDs = map[string]bool{
	"admin":   true,
	"healthz": true,
	"metrics": true,
}

// validAccountID keeps an id usable as a URL path segment. Without this an id
// like "zen 01" or "a/b" would silently produce an unreachable Base URL.
var validAccountID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateAccountID checks that an id can be used both as a path segment and
// as a proxy credential username.
func ValidateAccountID(id string) error {
	if id == "" {
		return fmt.Errorf("account id is required")
	}
	if !validAccountID.MatchString(id) {
		return fmt.Errorf("account id %q is not URL-safe: use letters, digits, dot, dash or underscore, starting with a letter or digit", id)
	}
	if reservedAccountIDs[strings.ToLower(id)] {
		return fmt.Errorf("account id %q collides with zenproxy's own /%s route; pick another id", id, strings.ToLower(id))
	}
	return nil
}

// Upstream describes one opencode.ai API surface.
//
//	zen      -> https://opencode.ai/zen/v1        (OpenAI /chat/completions + Anthropic /messages)
//	zen-go   -> https://opencode.ai/zen/go/v1     (OpenCode Go subscription)
type Upstream struct {
	BaseURL string            `yaml:"base_url" json:"base_url"`
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
}

// Account is one credential in the pool. Exactly one API key == one logical
// account, which matches how LiteLLM models it: one deployment per account.
type Account struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	Upstream string `yaml:"upstream" json:"upstream"`
	APIKey   string `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	// Pool is the group of interchangeable accounts this one belongs to. The
	// gateway entry picks an account from a pool, so a pool is what makes
	// "switch to another account" possible when an exit cannot be rotated any
	// further. Empty means "the same as Upstream", which keeps the older
	// one-upstream-per-account configs working unchanged.
	Pool string `yaml:"pool,omitempty" json:"pool,omitempty"`
	// Proxy is either "auto" (let the pool pick and stick), "direct" (no proxy)
	// or the id of a specific pool member.
	Proxy   string            `yaml:"proxy,omitempty" json:"proxy,omitempty"`
	Weight  int               `yaml:"weight,omitempty" json:"weight,omitempty"`
	Enabled bool              `yaml:"enabled" json:"enabled"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Runtime-only, never taken from config.
	CreatedAt time.Time `yaml:"-" json:"created_at,omitempty"`
}

// EffectivePool is the pool an account belongs to, defaulting to its upstream
// name so that a config written before pools existed groups exactly the way it
// used to: one pool per upstream.
func (a Account) EffectivePool() string {
	if a.Pool != "" {
		return a.Pool
	}
	return a.Upstream
}

// ErrorClass buckets upstream failures so that rotation decisions and the
// admin panel can tell "the egress IP is bad" apart from "the account is bad"
// and from "the provider is refusing this kind of client at all".
type ErrorClass string

const (
	ClassOK        ErrorClass = "ok"
	ClassTransport ErrorClass = "transport"  // dial/TLS/timeout: the egress path is suspect
	ClassProxyAuth ErrorClass = "proxy_auth" // 407 from the egress proxy: this exit is dead
	ClassRateLimit ErrorClass = "rate_limit" // 429: quota or IP throttling
	ClassServer    ErrorClass = "server"     // 5xx / 520-524: retry elsewhere
	ClassAuth      ErrorClass = "auth"       // 401/403 with a credential-shaped body
	ClassTierGate  ErrorClass = "tier_gate"  // FreeTierError: client-identity gate, NOT an IP problem
	ClassClient    ErrorClass = "client"     // 400/404/422: the request itself is wrong
	ClassCancelled ErrorClass = "cancelled"
	ClassUnknown   ErrorClass = "unknown"
)

// Retryable reports whether a different egress exit (or another account) has
// any chance of turning this failure into a success.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassTransport, ClassProxyAuth, ClassRateLimit, ClassServer, ClassUnknown:
		return true
	default:
		return false
	}
}

// IPAttributable reports whether rotating the exit IP is a sensible response.
// A FreeTierError is deliberately excluded: it is decided from the request's
// client identity, so burning a fresh IP on it only wastes exits.
func (c ErrorClass) IPAttributable() bool {
	switch c {
	case ClassTransport, ClassProxyAuth, ClassRateLimit, ClassServer:
		return true
	default:
		return false
	}
}

// Stats is the per-account runtime counter block.
type Stats struct {
	Requests     int64      `json:"requests"`
	Success      int64      `json:"success"`
	Failures     int64      `json:"failures"`
	Rotations    int64      `json:"rotations"`
	LastError    string     `json:"last_error,omitempty"`
	LastClass    ErrorClass `json:"last_class,omitempty"`
	LastUsed     *time.Time `json:"last_used,omitempty"`
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	UpstreamMsgs int64      `json:"upstream_calls"`
}

// ProxyHealth is the runtime health record of one egress exit.
type ProxyHealth struct {
	ID              string     `json:"id"`
	Addr            string     `json:"addr"`
	Scheme          string     `json:"scheme"`
	Healthy         bool       `json:"healthy"`
	ExitIP          string     `json:"exit_ip,omitempty"`
	LatencyMS       int64      `json:"latency_ms"`
	Failures        int64      `json:"failures"`
	SoftFails       int64      `json:"soft_failures"`
	ConsecFails     int        `json:"consecutive_failures"`
	QuarantinedTill *time.Time `json:"quarantined_until,omitempty"`
	// QuotaUntil is when the upstream said this exit's allowance returns. It is
	// separate from QuarantinedTill because the two mean opposite things about
	// the exit: quarantine says the exit is broken, quota says the exit is fine
	// and the upstream is refusing it anyway. An exit can be both, and an
	// operator reading the panel needs to tell them apart — one is repaired by
	// fixing the exit, the other has no fix at all but waiting.
	QuotaUntil    *time.Time `json:"quota_until,omitempty"`
	QuotaReason   string     `json:"quota_reason,omitempty"`
	QuotaHits     int        `json:"quota_hits,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastCheck     *time.Time `json:"last_check,omitempty"`
	BoundAccounts []string   `json:"bound_accounts,omitempty"`
	// Retired exits are out of rotation for good. Quarantine expires on its
	// own; retirement does not, which is the whole point: a dead IP that keeps
	// coming back every 30 minutes is not a pool member, it is noise.
	Retired       bool       `json:"retired"`
	RetiredReason string     `json:"retired_reason,omitempty"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
	// Prunable mirrors what a prune pass would retire right now, so the panel
	// can show the operator the damage before they commit to it.
	Prunable bool `json:"prunable"`
}

// PruneReport is the outcome of one prune pass over the egress pool.
type PruneReport struct {
	Retired        []string `json:"retired"`
	AlreadyRetired []string `json:"already_retired"`
	UsableAfter    int      `json:"usable_after"`
	// PoolEmpty is what it says: the prune took every exit out of rotation.
	// Traffic will keep flowing through the least-bad retired exit, but the
	// operator needs to know the pool is gone rather than guess it.
	PoolEmpty bool `json:"pool_empty"`
}
