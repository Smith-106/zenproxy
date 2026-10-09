package config

import (
	"strings"
	"testing"

	"zenproxy/internal/model"
)

// loadConfig writes a YAML file and loads it through the real path, so
// ${ENV} expansion and validation are exercised exactly as shipped.
func loadConfig(t *testing.T, body string) (Config, error) {
	t.Helper()
	return Load(writeConfig(t, body))
}

const gatewayBase = `
server:
  admin_token: admintoken
upstreams:
  zen:
    base_url: https://opencode.ai/zen/v1
accounts:
  - id: zen-01
    upstream: zen
    api_key: sk-one
    enabled: true
  - id: zen-02
    upstream: zen
    api_key: sk-two
    enabled: true
`

func TestPoolsAreDerivedFromAccounts(t *testing.T) {
	// A config that never mentions pools must still group one-per-upstream,
	// which is what keeps every pre-existing deployment valid.
	cfg, err := loadConfig(t, gatewayBase)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.PoolIDs()
	if len(got) != 1 || got[0] != "zen" {
		t.Fatalf("PoolIDs = %v, want [zen] (the upstream name)", got)
	}
	if pol := cfg.PolicyFor("zen"); pol.Strategy != StrategyRR || pol.OnRateLimit != RotateBoth {
		t.Errorf("default policy = %+v, want round-robin + rotate-both", pol)
	}
}

func TestExplicitPoolOverridesTheUpstreamName(t *testing.T) {
	cfg, err := loadConfig(t, gatewayBase+`
  # pool is set on the account, not on the upstream, so accounts on different
  # upstreams can still be interchangeable
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_ = cfg
	cfg2, err := loadConfig(t, `
server:
  admin_token: admintoken
upstreams:
  a: {base_url: "https://a.example/v1"}
  b: {base_url: "https://b.example/v1"}
accounts:
  - {id: one, upstream: a, pool: shared, api_key: k1, enabled: true}
  - {id: two, upstream: b, pool: shared, api_key: k2, enabled: true}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg2.PoolIDs(); len(got) != 1 || got[0] != "shared" {
		t.Fatalf("PoolIDs = %v, want [shared] — two upstreams, one pool", got)
	}
}

func TestGatewayRejectsAKeyForAnUnknownPool(t *testing.T) {
	_, err := loadConfig(t, gatewayBase+`
gateway:
  enabled: true
  keys:
    - id: main
      key: sk-client
      pool: nonexistent
`)
	if err == nil {
		t.Fatal("a key pointing at a pool that has no accounts must be rejected")
	}
	// Naming the known pools is the difference between an actionable error and
	// a scavenger hunt through the YAML.
	if !strings.Contains(err.Error(), "zen") {
		t.Errorf("error should list the known pools, got: %v", err)
	}
}

func TestGatewayRejectsDuplicateKeyIDsAndBlankKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys string
		want string
	}{
		{"duplicate id", `
    - {id: main, key: k1, pool: zen}
    - {id: main, key: k2, pool: zen}`, "duplicate id"},
		{"blank key", `
    - {id: main, key: "", pool: zen}`, "key is empty"},
		{"missing pool", `
    - {id: main, key: k1}`, "pool is required"},
	} {
		_, err := loadConfig(t, gatewayBase+`
gateway:
  enabled: true
  keys:`+tc.keys+"\n")
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestGatewayRejectsAnUnsetEnvironmentKey(t *testing.T) {
	// Shipping the literal ${...} upstream as a credential would look like a
	// working config right up until every request 401s.
	_, err := loadConfig(t, gatewayBase+`
gateway:
  enabled: true
  keys:
    - {id: main, key: "${ZENPROXY_KEY_THAT_IS_NOT_SET}", pool: zen}
`)
	if err == nil {
		t.Fatal("an unexpanded ${VAR} key must be rejected")
	}
	if !strings.Contains(err.Error(), "ZENPROXY_KEY_THAT_IS_NOT_SET") {
		t.Errorf("error should name the variable, got: %v", err)
	}
}

func TestGatewayEnabledWithoutKeysIsRejected(t *testing.T) {
	_, err := loadConfig(t, gatewayBase+"\ngateway:\n  enabled: true\n")
	if err == nil {
		t.Fatal("enabling the gateway with no keys would let anyone in")
	}
	if !strings.Contains(err.Error(), "gateway.keys is empty") {
		t.Errorf("error = %v", err)
	}
}

func TestPoolPolicyValidation(t *testing.T) {
	for _, tc := range []struct{ field, value, want string }{
		{"strategy", "magic", "unknown strategy"},
		{"on_rate_limit", "explode", "unknown on_rate_limit"},
	} {
		_, err := loadConfig(t, gatewayBase+`
pools:
  - id: zen
    `+tc.field+`: `+tc.value+"\n")
		if err == nil {
			t.Errorf("%s=%s: expected an error", tc.field, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: error = %v, want %q", tc.field, tc.value, err, tc.want)
		}
	}
}

func TestModelListRouteFor(t *testing.T) {
	cfg, err := loadConfig(t, gatewayBase+`
model_list:
  - {model_name: friendly, pool: zen, upstream_model: vendor/real}
  - {model_name: selfnamed, pool: zen}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// upstream_model defaults to the client-facing name.
	got, ok := cfg.RouteFor("selfnamed")
	if !ok || got.UpstreamModel != "selfnamed" {
		t.Errorf("selfnamed => %+v,%v; upstream_model should default to model_name", got, ok)
	}
	got, ok = cfg.RouteFor("friendly")
	if !ok || got.UpstreamModel != "vendor/real" {
		t.Errorf("friendly => %+v,%v", got, ok)
	}
	if _, ok := cfg.RouteFor("unlisted"); ok {
		t.Error("an unlisted model resolved with no wildcard present")
	}
}

func TestModelListWildcardIsTheLastResort(t *testing.T) {
	cfg, err := loadConfig(t, gatewayBase+`
model_list:
  - {model_name: "*", pool: zen}
  - {model_name: named, pool: zen, upstream_model: real}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The exact match must win regardless of order, or a wildcard written
	// first would shadow every later entry.
	got, ok := cfg.RouteFor("named")
	if !ok || got.UpstreamModel != "real" {
		t.Errorf("named => %+v,%v; the exact route must beat the wildcard", got, ok)
	}
	got, ok = cfg.RouteFor("anything")
	if !ok || got.UpstreamModel != "anything" {
		t.Errorf("anything => %+v,%v", got, ok)
	}
}

func TestModelListDefaultsToTheFirstEnabledAccountsPool(t *testing.T) {
	cfg, err := loadConfig(t, `
server: {admin_token: t}
upstreams:
  zen: {base_url: "https://opencode.ai/zen/v1"}
accounts:
  - {id: off, upstream: zen, pool: unused, api_key: k, enabled: false}
  - {id: on, upstream: zen, pool: live, api_key: k, enabled: true}
model_list:
  - {model_name: m}
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := cfg.RouteFor("m")
	if !ok || got.Pool != "live" {
		t.Errorf("m => pool %q, want %q — an unqualified route must pick a pool that can serve it", got.Pool, "live")
	}
}

func TestPoolIDsAreSortedAndDeduped(t *testing.T) {
	cfg := Config{Accounts: []model.Account{
		{ID: "c", Upstream: "z"},
		{ID: "a", Upstream: "m"},
		{ID: "b", Upstream: "m"},
	}}
	got := cfg.PoolIDs()
	if len(got) != 2 || got[0] != "m" || got[1] != "z" {
		t.Fatalf("PoolIDs = %v, want [m z]", got)
	}
}
