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
	// ClassPolicyGate is the upstream's abuse throttle: 403 "Your access has
	// been restricted due to repeated policy violations." Measured 2026-10-14:
	// all four zen accounts collected these transiently while turns on other
	// exits kept succeeding at the same moment — the flag rides the requesting
	// IP, not the credential, and it fades (later turns through the same
	// account 200 again). As ClassAuth it was untouchable: no rotation, no
	// exit-health signal, the flagged slot kept serving more 403s. It is now
	// its own class: rotate away (and after a strike or two the ordinary
	// quarantine machinery benches the slot until it cools down).
	ClassPolicyGate ErrorClass = "policy_gate"
	// ClassTierGate is FreeTierError. Historically this was treated as a pure
	// client-identity refusal (UA / x-opencode-* / tools / stream). Measured
	// 2026-10 against free-proxy egress: the same enveloped request can 403 on
	// one exit and 200 on another, so rotating the exit is now worthwhile once
	// the envelope has already been applied.
	ClassTierGate ErrorClass = "tier_gate"
	// ClassRegion is a 403 saying the model is unavailable in the exit's
	// country. It is its own class because it is attributable to the exit but
	// NOT the way the others are: it belongs to the exit-and-model pair, so the
	// remedy is to change exit for this model rather than to distrust the exit.
	// Measured, one exit answered RegionError for one free model and 200 for
	// another in the same second. Folded into ClassAuth, as it was, it produced
	// no rotation at all and told the client the model was unavailable when
	// another exit would have served it.
	ClassRegion    ErrorClass = "region"
	ClassClient    ErrorClass = "client" // 400/404/422: the request itself is wrong
	ClassCancelled ErrorClass = "cancelled"
	ClassUnknown   ErrorClass = "unknown"
)

// Retryable reports whether a different egress exit (or another account) has
// any chance of turning this failure into a success.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassTransport, ClassProxyAuth, ClassRateLimit, ClassServer, ClassRegion, ClassTierGate, ClassPolicyGate, ClassUnknown:
		return true
	default:
		return false
	}
}

// IPAttributable reports whether rotating the exit IP is a sensible response.
//
// ClassTierGate is included as of 2026-10: FreeTierError is still primarily a
// request-envelope check, but free-proxy egress has been observed to return it
// intermittently for an already-enveloped request that succeeds on another
// exit. The forwarder still applies the envelope before rotating, so a pure
// identity miss is not "fixed" by burning IPs — rotation only helps the
// exit-correlated refusals.
//
// ClassRegion is the one class where the exit is only half the story: rotating
// is right, but it has to be a rotation *for this model*, which is why the
// rotation carries the model name. Rotating blind would just as likely land on
// another exit refused for the same model.
func (c ErrorClass) IPAttributable() bool {
	switch c {
	case ClassTransport, ClassProxyAuth, ClassRateLimit, ClassServer, ClassRegion, ClassTierGate, ClassPolicyGate:
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
	// LastDurationMS is how long the client waited on the last request,
	// including any exit changes in that request. It is not a probe latency.
	LastDurationMS int64 `json:"last_duration_ms,omitempty"`
	// LastHeaderMS is when the client could first see a byte of that same
	// request. The rest of LastDurationMS is the body still coming. Zero
	// means this process did not record it.
	LastHeaderMS int64 `json:"last_header_ms,omitempty"`
	// LastAttempts is how many upstream tries that same request used.
	// One means the first exit answered. More means the client waited
	// through a failure before the status it finally saw.
	LastAttempts int `json:"last_attempts,omitempty"`
	UpstreamMsgs int64      `json:"upstream_calls"`
}

// ProxyHealth is the runtime health record of one egress exit.
type ProxyHealth struct {
	ID      string `json:"id"`
	Addr    string `json:"addr"`
	Scheme  string `json:"scheme"`
	Healthy bool   `json:"healthy"`
	ExitIP  string `json:"exit_ip,omitempty"`
	// ExitIPNote is a fixed word for why the address cell is empty even though
	// the tunnel just succeeded. It is not a health failure.
	ExitIPNote string `json:"exit_ip_note,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	// Slow is the same cut as the summary card: still in rotation, picked after
	// the exits that answered inside max_latency.
	Slow            bool       `json:"slow,omitempty"`
	Failures        int64      `json:"failures"`
	SoftFails       int64      `json:"soft_failures"`
	ConsecFails     int        `json:"consecutive_failures"`
	QuarantinedTill *time.Time `json:"quarantined_until,omitempty"`
	// QuarantineClass is timeout, unreachable, tls, or other while the nap is
	// still running. It is the word the panel can show after a restart, when
	// the raw dial error is gone on purpose.
	QuarantineClass string `json:"quarantine_class,omitempty"`
	// QuotaUntil is when the upstream said this exit's allowance returns. It is
	// separate from QuarantinedTill because the two mean opposite things about
	// the exit: quarantine says the exit is broken, quota says the exit is fine
	// and the upstream is refusing it anyway. An exit can be both, and an
	// operator reading the panel needs to tell them apart — one is repaired by
	// fixing the exit, the other has no fix at all but waiting.
	QuotaUntil  *time.Time `json:"quota_until,omitempty"`
	QuotaReason string     `json:"quota_reason,omitempty"`
	QuotaHits   int        `json:"quota_hits,omitempty"`
	// RegionBlockedFor names the models this exit's country is refused for.
	//
	// Scoped to the model because that is what was measured: one exit served one
	// free model and was refused another in the same second. So an exit can be
	// listed here and remain perfectly healthy for everything else, which is why
	// this is deliberately not a health field — a panel that showed it as down
	// would send an operator to debug a working proxy.
	RegionBlockedFor []string   `json:"region_blocked_for,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	LastCheck        *time.Time `json:"last_check,omitempty"`
	BoundAccounts    []string   `json:"bound_accounts,omitempty"`
	// GatePassUntil is when this exit's free-tier admission expires, and
	// GatePasses how many admissions it has had in this process's lifetime.
	//
	// Exposed because this is the one signal an operator cannot derive from the
	// rest of the panel: the exit looks identical to 1,600 others (healthy,
	// fast, unblocked) yet it is one of the few the gate actually serves. The
	// measured set was tiny — six exits out of 1,613 — so "which exits can
	// serve free models right now" needs to be answerable without grepping logs.
	GatePassUntil *time.Time `json:"gate_pass_until,omitempty"`
	GatePasses    int        `json:"gate_passes,omitempty"`
	// GateScore and GateDrops rank the admitted exits against each other. Among
	// the six measured admissions the share of streams that died mid-response
	// varied threefold, so "admitted" alone does not say whether a client will
	// actually get an answer: GateScore is what selection weights by, GateDrops
	// is the raw count of streams this exit failed to carry.
	GateScore int `json:"gate_score,omitempty"`
	GateDrops int `json:"gate_drops,omitempty"`
	// Retired exits are out of rotation for good. Quarantine expires on its
	// own; retirement does not, which is the whole point: a dead IP that keeps
	// coming back every 30 minutes is not a pool member, it is noise.
	Retired       bool       `json:"retired"`
	RetiredReason string     `json:"retired_reason,omitempty"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
	// Prunable mirrors what a prune pass would retire right now, so the panel
	// can show the operator the damage before they commit to it.
	Prunable bool `json:"prunable"`
	// PeerRetired means the other replica's state file has retired this exit
	// and this one has not. PeerKeeps is the opposite. Both are omitted when
	// there is no live peer file.
	PeerRetired bool `json:"peer_retired,omitempty"`
	PeerKeeps   bool `json:"peer_keeps,omitempty"`
	// PeerQuota means the other replica has this exit in a quota cooldown and
	// this one does not. PeerQuotaHere is the opposite. A missing peer record
	// counts as not cooled, same as ReplicaSplit.
	PeerQuota     bool `json:"peer_quota,omitempty"`
	PeerQuotaHere bool `json:"peer_quota_here,omitempty"`
	// PeerRetiredClass is timeout, unreachable, tls, or other. It is the other
	// replica's retirement reason folded to a word, never the dial text.
	PeerRetiredClass string `json:"peer_retired_class,omitempty"`
}

// PruneReport is the outcome of one prune pass over the egress pool.
type PruneReport struct {
	// Refused is non-empty when the pass ran nothing: the outage breaker was
	// open and the operator did not force it, so the pool was left untouched.
	Refused        string   `json:"refused,omitempty"`
	Retired        []string `json:"retired"`
	AlreadyRetired []string `json:"already_retired"`
	UsableAfter    int      `json:"usable_after"`
	// PoolEmpty is what it says: the prune took every exit out of rotation.
	// Traffic will keep flowing through the least-bad retired exit, but the
	// operator needs to know the pool is gone rather than guess it.
	PoolEmpty bool `json:"pool_empty"`
}

var (
	panelURL      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	panelUserinfo = regexp.MustCompile(`[^\s/@]+:[^\s/@]+@`)
	panelIPv4     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
)

// httpStatusLine is "http 400 ...", a vendor sentence. "http proxy: ..." is a
// dial that happens to start with the scheme word, and must not be skipped.
func httpStatusLine(low string) bool {
	if !strings.HasPrefix(low, "http ") || len(low) < 6 {
		return false
	}
	return low[5] >= '0' && low[5] <= '9'
}

// DialClass folds a socket dump into the same four words the quarantine
// counter uses. An HTTP status line is not a dial, so it returns "" and the
// caller keeps the vendor sentence.
func DialClass(msg string) string {
	low := strings.ToLower(strings.TrimSpace(msg))
	if low == "" || httpStatusLine(low) {
		return ""
	}
	switch {
	case strings.Contains(low, "certificate") || strings.Contains(low, "x509") || strings.Contains(low, "tls:"):
		return "tls"
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		return "timeout"
	case strings.Contains(low, "proxyconnect") || strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host") || strings.Contains(low, "unreachable") ||
		strings.Contains(low, "socks"):
		return "unreachable"
	default:
		return ""
	}
}

var (
	retirePoison = regexp.MustCompile(`^(\d+) TLS poison strikes\b`)
	retireStreak = regexp.MustCompile(`^(\d+) consecutive failures\b`)
)

// PanelRetireReason is what the admin API may say about a retirement. The
// count stays. The dial sentence after "last:" does not: it is the same dump
// DialClass already folds, and it does not help the operator.
func PanelRetireReason(reason string) string {
	if m := retirePoison.FindStringSubmatch(reason); m != nil {
		return "tls poison " + m[1]
	}
	if m := retireStreak.FindStringSubmatch(reason); m != nil {
		return "consecutive " + m[1]
	}
	if class := DialClass(reason); class != "" {
		return class
	}
	return ScrubPanel(reason)
}

// PanelReason is the only failure text the admin API may return. A dial
// becomes one of the four words. Anything else loses URLs, proxy userinfo,
// and IPv4 addresses before it reaches the browser.
func PanelReason(msg string) string {
	if class := DialClass(msg); class != "" {
		return class
	}
	return ScrubPanel(msg)
}

// ScrubPanel removes addresses from a sentence that is still worth reading,
// such as a retirement count that quotes the last dial.
func ScrubPanel(msg string) string {
	msg = panelURL.ReplaceAllString(msg, "<url>")
	msg = panelUserinfo.ReplaceAllString(msg, "")
	msg = panelIPv4.ReplaceAllString(msg, "<exit>")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}
