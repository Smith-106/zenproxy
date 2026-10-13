// Package config loads the zenproxy YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"zenproxy/internal/model"
)

// Config is the whole on-disk configuration surface.
type Config struct {
	Server    Server                    `yaml:"server"`
	Upstreams map[string]model.Upstream `yaml:"upstreams"`
	Proxies   Proxies                   `yaml:"proxies"`
	Health    Health                    `yaml:"health"`
	Policy    Policy                    `yaml:"policy"`
	Accounts  []model.Account           `yaml:"accounts"`
	FreeTier  FreeTier                  `yaml:"free_tier"`
	StateFile string                    `yaml:"state_file"`
	// Gateway, Pools and ModelList turn the daemon from "address one account
	// by its path" into a single OpenAI-shaped endpoint with one key. They are
	// additive: a config without them behaves exactly as before.
	Gateway   Gateway      `yaml:"gateway"`
	Pools     []PoolPolicy `yaml:"pools"`
	ModelList []ModelRoute `yaml:"model_list"`
}

// Gateway is the unified ingress: one URL and one key, with the account and
// the exit chosen automatically. It is the shape every downstream gateway
// (new-api, CLIProxyAPI, sub2api, LiteLLM) can consume as an upstream.
type Gateway struct {
	Enabled bool `yaml:"enabled"`
	// Keys are the credentials clients present as `Authorization: Bearer …`.
	// Each key is bound to one pool, which is what makes "this key can use
	// these accounts" expressible without naming an account in the URL.
	Keys []GatewayKey `yaml:"keys"`
	// RequireKey refuses unauthenticated /v1/* traffic. It defaults to true
	// whenever at least one key is configured, because an open gateway on a
	// routable listener is an open relay.
	RequireKey *bool `yaml:"require_key"`
}

// GatewayKey is one client credential and the pool it may draw from.
type GatewayKey struct {
	ID   string `yaml:"id"`
	Key  string `yaml:"key"`
	Pool string `yaml:"pool"`
	// Name is shown in the admin panel instead of the key itself.
	Name string `yaml:"name,omitempty"`
}

// PoolPolicy overlays behaviour on a pool. Pools themselves are derived from
// the accounts (see model.Account.EffectivePool), so a config that never
// mentions pools still groups accounts exactly one-per-upstream, as before.
type PoolPolicy struct {
	ID string `yaml:"id"`
	// Strategy picks the next account: "round-robin" or "weighted".
	Strategy string `yaml:"strategy"`
	// OnRateLimit decides what a 429 (or another IP-attributable failure)
	// switches. This is the reactive half of "quota is limited per IP, switch
	// automatically": the upstream tells us the IP is spent and we move.
	//
	//	rotate-exit     move this account to a different exit IP
	//	rotate-account  move to a different account in the same pool
	//	rotate-both     try a new IP first, then another account
	OnRateLimit string `yaml:"on_rate_limit"`
}

// ModelRoute maps the name a client asks for onto a pool and, optionally, the
// upstream's own name for it. It mirrors LiteLLM's model_list, including the
// "*" wildcard entry, so a LiteLLM config translates without rethinking it.
type ModelRoute struct {
	ModelName     string `yaml:"model_name"`
	Pool          string `yaml:"pool"`
	UpstreamModel string `yaml:"upstream_model,omitempty"`
}

// Rotation policies accepted by PoolPolicy.OnRateLimit.
const (
	RotateExit     = "rotate-exit"
	RotateAccount  = "rotate-account"
	RotateBoth     = "rotate-both"
	StrategyRR     = "round-robin"
	StrategyWeight = "weighted"
)

// Server holds the HTTP listener settings.
type Server struct {
	Listen      string        `yaml:"listen"`
	AdminToken  string        `yaml:"admin_token"`
	ReadTimeout time.Duration `yaml:"read_timeout"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
}

// Proxies is the egress pool definition.
//
// Resin mode expands one pool member per name in Accounts, all sharing the
// same listener and token, and deriving credentials as
//
//	<Platform>.<name> : <token>
//
// which is Resin's sticky-identity format: the same Account always lands on
// the same exit node while it stays alive.
type Proxies struct {
	Resin  Resin    `yaml:"resin"`
	Static []string `yaml:"static"`
}

// Resin describes a Resin forward-proxy listener.
type Resin struct {
	Enabled  bool     `yaml:"enabled"`
	Scheme   string   `yaml:"scheme"`
	Host     string   `yaml:"host"`
	Platform string   `yaml:"platform"`
	Token    string   `yaml:"token"`
	Accounts []string `yaml:"accounts"`
}

// Health configures the background egress checker.
type Health struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	ProbeURL         string        `yaml:"probe_url"`
	Reachability     string        `yaml:"reachability"`
	FailureThreshold int           `yaml:"failure_threshold"`
	Quarantine       time.Duration `yaml:"quarantine"`
	MaxBackoff       time.Duration `yaml:"max_backoff"`
	// MaxLatency demotes an exit whose last probe took longer than this. It is
	// the single most useful knob for a pool built from unreliable nodes,
	// where a "working" exit can still take tens of seconds. 0 disables the
	// ceiling and keeps every reachable exit in the round-robin.
	MaxLatency time.Duration `yaml:"max_latency"`
	// RetireAfter takes an exit out of rotation permanently once it has failed
	// this many times in a row with no success in between — "permanently"
	// meaning it does not come back on a timer the way quarantine does. That is
	// how genuinely dead IPs leave a pool instead of being retried forever
	// every 30 minutes. A single success resets the counter, so an exit that
	// still works now and then is never retired. 0 disables retirement.
	RetireAfter int `yaml:"retire_after"`
	// ProbeConcurrency caps how many exits one probe round dials at once.
	// 0 defaults to 64. Pools in the thousands of exits must keep this well
	// below the provider's concurrent-connection allowance — an unbounded
	// round is what tripped "CONNECT reply: 429 Not Enough Connections".
	ProbeConcurrency int `yaml:"probe_concurrency"`
}

// Policy configures retry and rotation behaviour.
type Policy struct {
	MaxAttempts       int           `yaml:"max_attempts"`
	RotateOnStatus    []int         `yaml:"rotate_on_status"`
	RotateOnTransport bool          `yaml:"rotate_on_transport"`
	MaxBodyBytes      int64         `yaml:"max_body_bytes"`
	StreamIdleTimeout time.Duration `yaml:"stream_idle_timeout"`
	UpstreamTimeout   time.Duration `yaml:"upstream_timeout"`
	// AllowedHosts is the forward-proxy allowlist. It is also implicitly
	// extended with every upstream hostname. Leave it empty only when the
	// listener is bound to loopback.
	AllowedHosts []string `yaml:"allowed_hosts"`
	// SessionExitTTL is the CPA-style sticky TTL for session→exit bindings.
	// Explicit conversation keys (session_id / prompt_cache_key /
	// x-opencode-session) pin an account onto one resin exit for this long.
	// Zero disables session→exit stickiness and keeps the legacy account→exit
	// binding only. Default 1h matches CLIProxyAPI's SessionAffinitySelector.
	SessionExitTTL time.Duration `yaml:"session_exit_ttl"`
}

// FreeTier configures the official-client envelope that lets a request which
// did not come from the OpenCode CLI use opencode.ai's free models. The
// mechanism, and the evidence behind it, are in internal/forward/freegate.go
// and docs/FREE-TIER.md.
//
// It is on by default because it costs nothing when it is not needed: only the
// models listed below are rewritten, so a deployment that serves paid models
// sends them byte for byte as the client wrote them.
type FreeTier struct {
	Enabled bool `yaml:"enabled"`
	// Models are the model ids expected to sit behind the free-tier gate. A
	// model missing from this list is still recovered on its first refusal, and
	// the log names it — so this list is a head start, not a hard boundary.
	Models []string `yaml:"models"`
	// UserAgent overrides the mirrored client User-Agent. Set it when the
	// upstream starts refusing the pinned version.
	UserAgent string `yaml:"user_agent"`
}

// DefaultFreeModels are the free-tier models known at the time of writing.
func DefaultFreeModels() []string {
	return []string{
		"big-pickle",
		"deepseek-v4-flash-free",
		"laguna-s-2.1-free",
		"ling-3.0-flash-free",
		"mimo-v2.5-free",
		"nemotron-3-ultra-free",
		"north-mini-code-free",
	}
}

// Default returns a config with sane production defaults filled in.
func Default() Config {
	return Config{
		Server: Server{
			Listen:      "127.0.0.1:9000",
			ReadTimeout: 30 * time.Second,
			IdleTimeout: 120 * time.Second,
		},
		Upstreams: map[string]model.Upstream{
			"zen":    {BaseURL: "https://opencode.ai/zen/v1"},
			"zen-go": {BaseURL: "https://opencode.ai/zen/go/v1"},
		},
		Proxies: Proxies{Resin: Resin{Scheme: "socks5h", Platform: "Default"}},
		Health: Health{
			Interval:         60 * time.Second,
			Timeout:          8 * time.Second,
			ProbeURL:         "https://api.ipify.org?format=json",
			Reachability:     "opencode.ai:443",
			FailureThreshold: 3,
			Quarantine:       5 * time.Minute,
			MaxBackoff:       30 * time.Minute,
			RetireAfter:      20,
		},
		Policy: Policy{
			MaxAttempts:       3,
			RotateOnStatus:    []int{403, 407, 429, 500, 502, 503, 504, 520, 522, 524},
			RotateOnTransport: true,
			MaxBodyBytes:      32 << 20,
			StreamIdleTimeout: 5 * time.Minute,
			UpstreamTimeout:   10 * time.Minute,
			SessionExitTTL:    time.Hour,
		},
		StateFile: "./data/state.json",
		FreeTier: FreeTier{
			Enabled: true,
			Models:  DefaultFreeModels(),
		},
	}
}

// Load reads path, expands ${ENV} references, and fills defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	raw = []byte(expandEnv(string(raw)))
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, cfg.Validate()
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} with the environment value, leaving unknown
// variables untouched so that Validate can report them instead of silently
// shipping an empty credential.
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

func (c *Config) normalize() {
	d := Default()
	if c.Server.Listen == "" {
		c.Server.Listen = d.Server.Listen
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = d.Server.ReadTimeout
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = d.Server.IdleTimeout
	}
	if len(c.Upstreams) == 0 {
		c.Upstreams = d.Upstreams
	}
	c.Proxies.Resin = c.Proxies.Resin.withDefaults()
	h, dh := &c.Health, d.Health
	if h.Interval == 0 {
		h.Interval = dh.Interval
	}
	if h.Timeout == 0 {
		h.Timeout = dh.Timeout
	}
	if h.ProbeURL == "" {
		h.ProbeURL = dh.ProbeURL
	}
	if h.Reachability == "" {
		h.Reachability = dh.Reachability
	}
	if h.FailureThreshold == 0 {
		h.FailureThreshold = dh.FailureThreshold
	}
	if h.Quarantine == 0 {
		h.Quarantine = dh.Quarantine
	}
	if h.MaxBackoff == 0 {
		h.MaxBackoff = dh.MaxBackoff
	}
	p, dp := &c.Policy, d.Policy
	if p.MaxAttempts == 0 {
		p.MaxAttempts = dp.MaxAttempts
	}
	if len(p.RotateOnStatus) == 0 {
		p.RotateOnStatus = dp.RotateOnStatus
	}
	if p.MaxBodyBytes == 0 {
		p.MaxBodyBytes = dp.MaxBodyBytes
	}
	if p.StreamIdleTimeout == 0 {
		p.StreamIdleTimeout = dp.StreamIdleTimeout
	}
	if p.UpstreamTimeout == 0 {
		p.UpstreamTimeout = dp.UpstreamTimeout
	}
	if p.SessionExitTTL == 0 {
		// Absent key → CPA-like 1h sticky. Explicit disable is session_exit_ttl
		// set to a negative duration in YAML (e.g. -1s), normalized below.
		p.SessionExitTTL = dp.SessionExitTTL
	}
	if p.SessionExitTTL < 0 {
		p.SessionExitTTL = 0
	}
	if c.StateFile == "" {
		c.StateFile = d.StateFile
	}
	if len(c.FreeTier.Models) == 0 {
		// An empty list would silently disable the envelope for every model but
		// the ones the upstream happens to refuse first. Defaults restore it.
		c.FreeTier.Models = d.FreeTier.Models
	}
	for name, up := range c.Upstreams {
		up.BaseURL = strings.TrimRight(up.BaseURL, "/")
		c.Upstreams[name] = up
	}
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.Proxy == "" {
			a.Proxy = "auto"
		}
		if a.Weight == 0 {
			a.Weight = 1
		}
		if a.Upstream == "" {
			a.Upstream = "zen"
		}
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if p.Strategy == "" {
			p.Strategy = StrategyRR
		}
		if p.OnRateLimit == "" {
			p.OnRateLimit = RotateBoth
		}
	}
	for i := range c.ModelList {
		m := &c.ModelList[i]
		if m.UpstreamModel == "" {
			m.UpstreamModel = m.ModelName
		}
		if m.Pool == "" {
			// An unqualified route means "the pool named after the model's own
			// upstream", which for the historical single-upstream config is
			// simply that upstream.
			m.Pool = DefaultPoolID(c.Accounts)
		}
	}
	// The gateway refuses anonymous traffic as soon as it has any key: an open
	// /v1/* on a routable listener is an open relay to the accounts behind it.
	if c.Gateway.RequireKey == nil {
		v := true
		c.Gateway.RequireKey = &v
	}
}

// DefaultPoolID names the pool an unqualified route or key falls back to. It
// is the pool of the first enabled account, so a single-pool config never has
// to name its pool at all.
func DefaultPoolID(accounts []model.Account) string {
	for _, a := range accounts {
		if a.Enabled {
			return a.EffectivePool()
		}
	}
	if len(accounts) > 0 {
		return accounts[0].EffectivePool()
	}
	return ""
}

// PoolIDs lists every pool the accounts actually produce, sorted, deduped.
func (c Config) PoolIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range c.Accounts {
		id := a.EffectivePool()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// PolicyFor returns the policy of a pool, with defaults filled in when the
// pool was never declared (its policy is then entirely default).
func (c Config) PolicyFor(pool string) PoolPolicy {
	for _, p := range c.Pools {
		if p.ID == pool {
			if p.Strategy == "" {
				p.Strategy = StrategyRR
			}
			if p.OnRateLimit == "" {
				p.OnRateLimit = RotateBoth
			}
			return p
		}
	}
	return PoolPolicy{ID: pool, Strategy: StrategyRR, OnRateLimit: RotateBoth}
}

// RouteFor resolves the model a client asked for. The second result is false
// when no route matches; the caller then decides whether to pass the name
// through untouched (which is what a "*" route authorises).
func (c Config) RouteFor(model string) (ModelRoute, bool) {
	for _, m := range c.ModelList {
		if m.ModelName == model {
			return m, true
		}
	}
	for _, m := range c.ModelList {
		if m.ModelName == "*" {
			return ModelRoute{ModelName: model, Pool: m.Pool, UpstreamModel: model}, true
		}
	}
	return ModelRoute{}, false
}

// Validate fails fast on the mistakes that would otherwise show up as a
// confusing 401 hours later.
func (c Config) Validate() error {
	var problems []string
	if c.Server.AdminToken == "" {
		problems = append(problems, "server.admin_token is empty (set ZEN_ADMIN_TOKEN)")
	}
	if len(c.Upstreams) == 0 {
		problems = append(problems, "no upstreams defined")
	}
	for i, a := range c.Accounts {
		if err := model.ValidateAccountID(a.ID); err != nil {
			problems = append(problems, fmt.Sprintf("accounts[%d]: %s", i, err))
		}
		if _, ok := c.Upstreams[a.Upstream]; !ok {
			problems = append(problems, fmt.Sprintf("account %q references unknown upstream %q", a.ID, a.Upstream))
		}
		if name, ok := unexpandedVar(a.APIKey); ok {
			problems = append(problems, fmt.Sprintf("account %q api_key still contains an unexpanded %s — set that environment variable, or the literal placeholder would be sent upstream as the key", a.ID, name))
		}
		if a.APIKey == "" && a.Enabled {
			problems = append(problems, fmt.Sprintf("account %q is enabled but has no api_key", a.ID))
		}
	}
	if c.Proxies.Resin.Enabled {
		if c.Proxies.Resin.Host == "" {
			problems = append(problems, "proxies.resin.enabled but proxies.resin.host is empty")
		}
		if name, ok := unexpandedVar(c.Proxies.Resin.Token); ok {
			problems = append(problems, "proxies.resin.token still contains an unexpanded "+name+" (set that environment variable)")
		} else if c.Proxies.Resin.Token == "" {
			problems = append(problems, "proxies.resin.token is empty (set RESIN_PROXY_TOKEN)")
		}
		if len(c.Proxies.Resin.Accounts) == 0 {
			problems = append(problems, "proxies.resin.enabled but proxies.resin.accounts is empty")
		}
	}
	if name, ok := unexpandedVar(c.Server.AdminToken); ok {
		problems = append(problems, "server.admin_token still contains an unexpanded "+name)
	}
	problems = append(problems, c.validateGateway()...)
	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// validateGateway checks the unified ingress. Every one of these mistakes would
// otherwise surface as a 401 at request time, where the cause is invisible.
func (c Config) validateGateway() []string {
	var problems []string
	known := map[string]bool{}
	for _, id := range c.PoolIDs() {
		known[id] = true
	}
	for i, p := range c.Pools {
		if p.ID == "" {
			problems = append(problems, fmt.Sprintf("pools[%d]: id is required", i))
			continue
		}
		if !known[p.ID] {
			problems = append(problems, fmt.Sprintf("pools[%d]: pool %q has no accounts (an account joins a pool via its own `pool:` field, or by sharing an upstream name)", i, p.ID))
		}
		switch p.Strategy {
		case StrategyRR, StrategyWeight:
		default:
			problems = append(problems, fmt.Sprintf("pools[%d] (%s): unknown strategy %q; use %s or %s", i, p.ID, p.Strategy, StrategyRR, StrategyWeight))
		}
		switch p.OnRateLimit {
		case RotateExit, RotateAccount, RotateBoth:
		default:
			problems = append(problems, fmt.Sprintf("pools[%d] (%s): unknown on_rate_limit %q; use %s, %s or %s", i, p.ID, p.OnRateLimit, RotateExit, RotateAccount, RotateBoth))
		}
	}
	seen := map[string]bool{}
	for i, k := range c.Gateway.Keys {
		where := fmt.Sprintf("gateway.keys[%d]", i)
		if k.ID == "" {
			problems = append(problems, where+": id is required")
		} else if seen[k.ID] {
			problems = append(problems, fmt.Sprintf("%s: duplicate id %q", where, k.ID))
		}
		seen[k.ID] = true
		if k.Key == "" {
			problems = append(problems, where+": key is empty")
		} else if name, ok := unexpandedVar(k.Key); ok {
			problems = append(problems, where+" key still contains an unexpanded "+name+" — set that environment variable")
		}
		if k.Pool == "" {
			problems = append(problems, where+": pool is required")
		} else if !known[k.Pool] {
			problems = append(problems, fmt.Sprintf("%s: key %q references unknown pool %q; known pools: %s", where, k.ID, k.Pool, strings.Join(c.PoolIDs(), ", ")))
		}
	}
	if c.Gateway.Enabled && len(c.Gateway.Keys) == 0 {
		problems = append(problems, "gateway.enabled but gateway.keys is empty — clients would have no way to authenticate")
	}
	for i, m := range c.ModelList {
		if m.ModelName == "" {
			problems = append(problems, fmt.Sprintf("model_list[%d]: model_name is required", i))
			continue
		}
		if m.Pool != "" && !known[m.Pool] {
			problems = append(problems, fmt.Sprintf("model_list[%d] (%s): references unknown pool %q; known pools: %s", i, m.ModelName, m.Pool, strings.Join(c.PoolIDs(), ", ")))
		}
	}
	return problems
}

// withDefaults fills the fields that have a sensible fallback so ProxyURL is
// safe to call on a Resin that never went through normalize().
func (r Resin) withDefaults() Resin {
	if r.Scheme == "" {
		r.Scheme = "socks5h"
	}
	if r.Platform == "" {
		r.Platform = "Default"
	}
	return r
}

// unexpandedVar reports whether a value still holds a ${VAR} placeholder and,
// if so, names it. Naming it matters: with dozens of accounts, "some variable
// is unset" is not an actionable error. The placeholder is safe to echo — by
// definition it never expanded into a secret.
func unexpandedVar(s string) (string, bool) {
	i := strings.Index(s, "${")
	if i < 0 {
		return "", false
	}
	rest := s[i+2:]
	if j := strings.IndexByte(rest, '}'); j >= 0 {
		return "${" + rest[:j] + "}", true
	}
	return "${...}", true
}

// ResinProxyURL builds Resin's sticky identity URL for one pool member:
// scheme://Platform.Name:token@host
func (r Resin) ProxyURL(name string) string {
	r = r.withDefaults()
	return fmt.Sprintf("%s://%s.%s:%s@%s", r.Scheme, r.Platform, name, r.Token, r.Host)
}
