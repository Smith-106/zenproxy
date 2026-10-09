// Package config loads the zenproxy YAML configuration.
package config

import (
	"fmt"
	"os"
	"regexp"
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
}

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
			RotateOnStatus:    []int{407, 429, 500, 502, 503, 504, 520, 522, 524},
			RotateOnTransport: true,
			MaxBodyBytes:      32 << 20,
			StreamIdleTimeout: 5 * time.Minute,
			UpstreamTimeout:   10 * time.Minute,
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
	if len(problems) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
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
