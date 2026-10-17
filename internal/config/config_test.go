package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimal = `
server:
  listen: "127.0.0.1:9000"
  admin_token: "${ZEN_ADMIN_TOKEN}"
upstreams:
  zen:
    base_url: "https://opencode.ai/zen/v1"
proxies:
  resin:
    enabled: true
    scheme: "socks5h"
    host: "127.0.0.1:2260"
    platform: "Default"
    token: "${RESIN_PROXY_TOKEN}"
    accounts: ["slot-01", "slot-02"]
accounts:
  - id: zen-01
    upstream: zen
    api_key: "${ZEN_KEY_01}"
    proxy: auto
    enabled: true
`

// configWith writes the fixture with literal substitutions applied and a real
// temp state path, failing loudly if a target stops matching.
func configWith(t *testing.T, pairs ...string) string {
	t.Helper()
	body := minimal
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(body, pairs[i]) {
			t.Fatalf("substitution target not found in the fixture: %q", pairs[i])
		}
		body = strings.Replace(body, pairs[i], pairs[i+1], 1)
	}
	body += "state_file: '" + filepath.Join(t.TempDir(), "state.json") + "'\n"
	return writeConfig(t, body)
}

func TestLoadExpandsEnvironmentVariables(t *testing.T) {
	t.Setenv("ZEN_ADMIN_TOKEN", "admin-secret")
	t.Setenv("RESIN_PROXY_TOKEN", "resin-token")
	t.Setenv("ZEN_KEY_01", "sk-abc123456789")

	path := configWith(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.AdminToken != "admin-secret" {
		t.Errorf("admin_token = %q", cfg.Server.AdminToken)
	}
	if cfg.Proxies.Resin.Token != "resin-token" {
		t.Errorf("resin token = %q", cfg.Proxies.Resin.Token)
	}
	if got := cfg.Accounts[0].APIKey; got != "sk-abc123456789" {
		t.Errorf("api_key = %q", got)
	}
	if got := cfg.Upstreams["zen"].BaseURL; got != "https://opencode.ai/zen/v1" {
		t.Errorf("base_url = %q", got)
	}
}

// A literal "${...}" that survived to runtime means a missing variable, and
// shipping that would send the string "${ZEN_KEY_01}" upstream as the API key.
func TestLoadRefusesUnexpandedVariable(t *testing.T) {
	t.Setenv("ZEN_ADMIN_TOKEN", "admin-secret")
	t.Setenv("RESIN_PROXY_TOKEN", "resin-token")
	// ZEN_KEY_01 intentionally unset.

	path := configWith(t)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unexpanded variable")
	}
	if !strings.Contains(err.Error(), "ZEN_KEY_01") {
		t.Errorf("error = %v, want it to name the missing variable", err)
	}
}

func TestLoadRejectsMissingAdminToken(t *testing.T) {
	path := configWith(t, `admin_token: "${ZEN_ADMIN_TOKEN}"`, `admin_token: ""`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error: an empty admin token would leave the console open")
	}
}

func TestLoadRejectsEnabledAccountWithoutKey(t *testing.T) {
	t.Setenv("ZEN_KEY_01", "sk-abc123456789")
	t.Setenv("RESIN_PROXY_TOKEN", "resin-token")
	path := configWith(t, `    api_key: "${ZEN_KEY_01}"`, `    api_key: ""`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an enabled account with no key")
	}
	if !strings.Contains(err.Error(), "zen-01") {
		t.Errorf("error = %v, want it to name the account", err)
	}
}

func TestLoadRejectsUnknownUpstreamReference(t *testing.T) {
	t.Setenv("ZEN_KEY_01", "sk-abc123456789")
	t.Setenv("RESIN_PROXY_TOKEN", "resin-token")
	path := configWith(t, "    upstream: zen", "    upstream: nope")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an unknown upstream reference")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %v", err)
	}
}

// A disabled account without a key is legitimate: it is a placeholder.
func TestLoadAllowsDisabledAccountWithoutKey(t *testing.T) {
	t.Setenv("ZEN_ADMIN_TOKEN", "admin-secret")
	t.Setenv("RESIN_PROXY_TOKEN", "resin-token")
	path := configWith(t,
		`    api_key: "${ZEN_KEY_01}"`, `    api_key: ""`,
		"    proxy: auto\n    enabled: true", "    proxy: auto\n    enabled: false")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestResinProxyURLUsesStickyIdentityFormat(t *testing.T) {
	r := Resin{Enabled: true, Scheme: "socks5h", Host: "127.0.0.1:2260", Platform: "Default", Token: "tok"}
	got := r.ProxyURL("slot-01")
	if got != "socks5h://Default.slot-01:tok@127.0.0.1:2260" {
		t.Fatalf("ProxyURL = %q", got)
	}
	// Unset fields fall back to sane defaults rather than producing "://@".
	r2 := Resin{Enabled: true, Host: "h:1", Token: "t"}
	if got := r2.ProxyURL("a"); got != "socks5h://Default.a:t@h:1" {
		t.Fatalf("ProxyURL with defaults = %q", got)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	cfg := Config{}
	cfg.normalize()
	if cfg.Server.Listen == "" {
		t.Error("listen must get a default")
	}
	if cfg.Policy.MaxAttempts < 1 {
		t.Error("max_attempts must get a default")
	}
	if cfg.Policy.UpstreamTimeout <= 0 || cfg.Health.Interval <= 0 || cfg.Health.Timeout <= 0 {
		t.Error("timeouts must get defaults")
	}
	if cfg.StateFile == "" {
		t.Error("state_file must get a default")
	}
	if cfg.Policy.MaxBodyBytes <= 0 {
		t.Error("max_body_bytes must get a default")
	}
}

func TestDefaultEnablesTheFreeTierEnvelope(t *testing.T) {
	// The envelope costs nothing when it is not needed — only the listed models
	// are rewritten — so it ships on, with the free models known at build time.
	d := Default()
	if !d.FreeTier.Enabled {
		t.Error("the free-tier envelope must be on by default")
	}
	if len(d.FreeTier.Models) == 0 {
		t.Fatal("the default free-model list must not be empty")
	}
	found := false
	for _, m := range d.FreeTier.Models {
		if m == "big-pickle" {
			found = true
		}
	}
	if !found {
		t.Errorf("default free models look wrong: %v", d.FreeTier.Models)
	}

	// An empty list in a config file must not silently disable the envelope for
	// every model but the ones the upstream happens to refuse first.
	empty := Config{FreeTier: FreeTier{Enabled: true}}
	empty.normalize()
	if len(empty.FreeTier.Models) == 0 {
		t.Error("normalize must restore the default model list")
	}
}

func TestFreeTierBlockDuration(t *testing.T) {
	// The park duration is an availability knob: a health probe cannot clear a
	// park (probes never exercise the gate), so an over-long value keeps the
	// pool dead long after the gate reopened. 2026-10-17 measured exactly that
	// with the old hardcoded 15m. It has to be settable, and a missing key has
	// to land on the documented default rather than on an unbounded park.
	if got := Default().FreeTier.BlockDuration; got != 15*time.Minute {
		t.Errorf("default block_duration = %v, want 15m", got)
	}

	// Omitted and explicitly non-positive both fall back to the default.
	for _, zero := range []FreeTier{
		{Enabled: true},
		{Enabled: true, BlockDuration: -time.Second},
	} {
		c := Config{FreeTier: zero}
		c.normalize()
		if c.FreeTier.BlockDuration != 15*time.Minute {
			t.Errorf("normalize block_duration = %v, want the 15m fallback",
				c.FreeTier.BlockDuration)
		}
	}

	// A set value survives normalize untouched — yaml.v3 parses "2m" straight
	// into a time.Duration, so this is the whole path a real config file takes.
	c := Config{FreeTier: FreeTier{Enabled: true, BlockDuration: 2 * time.Minute}}
	c.normalize()
	if c.FreeTier.BlockDuration != 2*time.Minute {
		t.Errorf("explicit block_duration = %v, want 2m", c.FreeTier.BlockDuration)
	}
}

func TestExampleConfigStaysLoadable(t *testing.T) {
	// The example files are documentation that is easy to break while editing,
	// and deploy/config.example.yaml is the one that actually becomes a running
	// config on a server — a key that only exists in the README is not a key.
	t.Setenv("ZEN_ADMIN_TOKEN", "example-token")
	t.Setenv("ZEN_KEY_01", "sk-example-not-a-real-key")
	t.Setenv("ZEN_KEY_02", "sk-example-not-a-real-key-2")
	t.Setenv("RESIN_PROXY_TOKEN", "example-proxy-token")
	// The gateway block ships disabled, but its key is still an unexpanded
	// ${VAR} until this is set — and an unexpanded credential is an error
	// whether or not the feature that uses it is switched on, exactly as
	// accounts[].api_key has always behaved.
	t.Setenv("ZENPROXY_KEY", "sk-example-gateway-key")

	for _, path := range []string{
		filepath.Join("..", "..", "config.example.yaml"),
		filepath.Join("..", "..", "deploy", "config.example.yaml"),
	} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s no longer loads: %v", path, err)
		}
		if !cfg.FreeTier.Enabled || len(cfg.FreeTier.Models) == 0 {
			t.Errorf("%s must document a working free_tier block", path)
		}
		if cfg.Health.Timeout <= 0 {
			t.Errorf("%s lost its health.timeout example value", path)
		}
		if cfg.StateFile == "" || cfg.Server.Listen == "" {
			t.Errorf("%s lost state_file or server.listen", path)
		}
		// A gateway block that no longer parses would be invisible until an
		// operator flipped enabled: true on a live box.
		if len(cfg.Gateway.Keys) == 0 {
			t.Errorf("%s must document a gateway.keys block", path)
		}
		if got := cfg.PoolIDs(); len(got) == 0 {
			t.Errorf("%s produces no pools, so no gateway key could ever resolve", path)
		}
		for _, k := range cfg.Gateway.Keys {
			if !slices.Contains(cfg.PoolIDs(), k.Pool) {
				t.Errorf("%s: gateway key %q points at pool %q, which no account belongs to", path, k.ID, k.Pool)
			}
		}
	}
}

func TestDefaultPolicyRotatesOnTransport(t *testing.T) {
	d := Default()
	if !d.Policy.RotateOnTransport {
		t.Error("a dead exit must rotate by default")
	}
	found := map[int]bool{}
	for _, s := range d.Policy.RotateOnStatus {
		found[s] = true
	}
	for _, want := range []int{407, 429, 502, 503, 504} {
		if !found[want] {
			t.Errorf("default rotate_on_status is missing %d", want)
		}
	}
	if d.Health.FailureThreshold < 1 {
		t.Error("failure_threshold must be at least 1")
	}
	if d.Health.Quarantine <= 0 || d.Health.MaxBackoff < d.Health.Quarantine {
		t.Error("quarantine backoff bounds look wrong")
	}
}

func TestExpandEnvLeavesUnknownLiteralInPlace(t *testing.T) {
	t.Setenv("KNOWN", "yes")
	if got := expandEnv("${KNOWN}/${UNKNOWN_XYZ}"); got != "yes/${UNKNOWN_XYZ}" {
		t.Fatalf("expandEnv = %q", got)
	}
}

func TestHealthDefaultsAreDurations(t *testing.T) {
	d := Default()
	if d.Health.Interval != 60*time.Second {
		t.Fatalf("interval = %v", d.Health.Interval)
	}
}
