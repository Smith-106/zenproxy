package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// gwHarness is the unified ingress wired to a controllable upstream.
//
// The upstream distinguishes accounts by the Authorization header it receives,
// which is exactly how a real upstream tells credentials apart, so "which
// account was used" needs no instrumentation of the engine itself.
type gwHarness struct {
	engine *Engine
	reg    *registry.Registry
	exits  []*mockExit
	srv    *httptest.Server

	mu    sync.Mutex
	seen  []string // the Authorization header of every upstream request
	model []string // the model name of every upstream request
}

// gwOptions lets each test choose the pool policy and the gateway config.
type gwOptions struct {
	gateway   config.Gateway
	modelList []config.ModelRoute
	pools     []config.PoolPolicy
	accounts  []model.Account
	upstream  http.Handler
	exits     int
}

func newGW(t *testing.T, o gwOptions) *gwHarness {
	t.Helper()
	h := &gwHarness{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Get("Authorization"))
		h.model = append(h.model, probe.Model)
		h.mu.Unlock()
		o.upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)

	n := o.exits
	if n == 0 {
		n = 2
	}
	specs := make([]proxypool.Spec, 0, n)
	for i := 0; i < n; i++ {
		e := newMockExit(t)
		h.exits = append(h.exits, e)
		specs = append(specs, proxypool.Spec{ID: exitName(i), URL: e.srv.URL})
	}
	log := testLogger()
	pool, err := proxypool.NewPool(specs, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	// A freshly built member starts healthy, so every exit here is usable from
	// the first request and the rotation assertions measure the chooser rather
	// than the health checker.

	reg := registry.New("", pool, log)
	for _, a := range o.accounts {
		if err := reg.Upsert(a); err != nil {
			t.Fatalf("Upsert(%s): %v", a.ID, err)
		}
	}

	eng := New(reg, pool, Options{
		Upstreams: map[string]model.Upstream{"zen": {BaseURL: up.URL + "/zen/v1"}},
		Policy: config.Policy{
			MaxAttempts:       3,
			RotateOnStatus:    []int{407, 429, 500, 502, 503, 504},
			RotateOnTransport: true,
			MaxBodyBytes:      1 << 20,
			StreamIdleTimeout: 2 * time.Second,
			UpstreamTimeout:   30 * time.Second,
		},
		Log: log,
		// Off, so these tests measure routing rather than the free-tier
		// envelope, which has its own file.
		FreeTier:  config.FreeTier{Enabled: false},
		Gateway:   o.gateway,
		Pools:     o.pools,
		ModelList: o.modelList,
	})

	h.engine, h.reg = eng, reg
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/v1" || strings.HasPrefix(p, "/v1/") {
			eng.ServeGateway(w, r, p)
			return
		}
		account, rest := splitAccount(p)
		eng.ServeReverse(w, r, account, rest)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func exitName(i int) string { return "exit-" + string(rune('a'+i)) }

func (h *gwHarness) calls() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...), append([]string(nil), h.model...)
}

// post sends one chat completion through the unified ingress.
func (h *gwHarness) post(t *testing.T, key, model string) *http.Response {
	t.Helper()
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// alwaysOK is an upstream that serves everyone.
func alwaysOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"ok","choices":[]}`))
}

// refuseKey answers 429 for one credential and 200 for everyone else. This is
// the shape of a per-key quota, and it is what the account switch exists for.
func refuseKey(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+key {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		alwaysOK(w, r)
	})
}

func gwConfig(keys ...config.GatewayKey) config.Gateway {
	req := true
	return config.Gateway{Enabled: true, Keys: keys, RequireKey: &req}
}

func zenAccounts(ids ...string) []model.Account {
	out := make([]model.Account, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.Account{
			ID: id, Upstream: "zen", Pool: "pool", APIKey: "key-" + id,
			Proxy: "auto", Weight: 1, Enabled: true,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// authentication
// ---------------------------------------------------------------------------

func TestGatewayRefusesAnonymousAndUnknownKeys(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a"),
		upstream: http.HandlerFunc(alwaysOK),
	})

	for _, tc := range []struct{ name, key string }{
		{"no key", ""},
		{"wrong key", "not-the-key"},
		{"key with the wrong prefix", "Bearer secret-key"},
	} {
		resp := h.post(t, tc.key, "m")
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Bearer challenge", tc.name, got)
		}
	}

	resp := h.post(t, "secret-key", "m")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct key: status = %d, want 200", resp.StatusCode)
	}
}

func TestGatewayAcceptsTheAnthropicHeaderShape(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a"),
		upstream: http.HandlerFunc(alwaysOK),
	})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"m","messages":[]}`))
	// Anthropic clients send x-api-key, and /v1/messages is served here too.
	req.Header.Set("x-api-key", "secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// the reason this ingress exists: switching accounts inside one request
// ---------------------------------------------------------------------------

func TestGatewaySwitchesAccountsWhenOneKeyIsRateLimited(t *testing.T) {
	// One credential is spent; the pool holds another. The client must not see
	// the 429, because the whole point of the pool is that it can absorb it.
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a", "b"),
		pools:    []config.PoolPolicy{{ID: "pool", OnRateLimit: config.RotateAccount}},
		modelList: []config.ModelRoute{
			{ModelName: "m", Pool: "pool", UpstreamModel: "m"},
		},
		upstream: refuseKey("key-a"),
	})

	resp := h.post(t, "secret-key", "m")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the pool should have absorbed the 429", resp.StatusCode)
	}
	seen, _ := h.calls()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (one refusal, one success): %v", len(seen), seen)
	}
	if seen[0] != "Bearer key-a" || seen[1] != "Bearer key-b" {
		t.Errorf("accounts tried = %v, want [key-a key-b]", seen)
	}
}

func TestGatewayRotateBothBurnsExitsBeforeSwitchingAccount(t *testing.T) {
	// rotate-both is the "quota is limited per IP" answer: try another IP for
	// the same credential first, and only blame the credential once this
	// account has spent its share of exits.
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a", "b"),
		pools:    []config.PoolPolicy{{ID: "pool", OnRateLimit: config.RotateBoth}},
		modelList: []config.ModelRoute{
			{ModelName: "m", Pool: "pool", UpstreamModel: "m"},
		},
		upstream: refuseKey("key-a"),
		exits:    3,
	})

	resp := h.post(t, "secret-key", "m")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	seen, _ := h.calls()
	// Two attempts on account a (one per exit), then account b succeeds.
	want := []string{"Bearer key-a", "Bearer key-a", "Bearer key-b"}
	if len(seen) != len(want) {
		t.Fatalf("accounts tried = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("accounts tried = %v, want %v", seen, want)
		}
	}
}

func TestGatewayRotateExitKeepsTheCredential(t *testing.T) {
	// With rotate-exit the credential never changes: only the egress IP does.
	// A pool of one account must still make two attempts out of two exits.
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a"),
		pools:    []config.PoolPolicy{{ID: "pool", OnRateLimit: config.RotateExit}},
		modelList: []config.ModelRoute{
			{ModelName: "m", Pool: "pool", UpstreamModel: "m"},
		},
		upstream: refuseKey("key-a"),
		exits:    3,
	})

	resp := h.post(t, "secret-key", "m")
	defer resp.Body.Close()
	// Both attempts are refused, so the client sees the upstream's own error.
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 passed through", resp.StatusCode)
	}
	seen, _ := h.calls()
	if len(seen) != 2 {
		t.Fatalf("attempts = %d, want 2 (the per-account exit budget): %v", len(seen), seen)
	}
	for _, a := range seen {
		if a != "Bearer key-a" {
			t.Errorf("rotate-exit changed the credential to %q", a)
		}
	}
	// Two different exits must have carried them, or no rotation happened.
	hits := 0
	for _, e := range h.exits {
		if e.hits.Load() > 0 {
			hits++
		}
	}
	if hits < 2 {
		t.Errorf("only %d exit(s) carried traffic; rotate-exit did not move the account", hits)
	}
}

// ---------------------------------------------------------------------------
// model routing
// ---------------------------------------------------------------------------

func TestGatewayRewritesTheModelName(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a"),
		modelList: []config.ModelRoute{
			{ModelName: "friendly-name", Pool: "pool", UpstreamModel: "upstream/real-name"},
		},
		upstream: http.HandlerFunc(alwaysOK),
	})

	resp := h.post(t, "secret-key", "friendly-name")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, models := h.calls()
	if len(models) != 1 || models[0] != "upstream/real-name" {
		t.Fatalf("upstream saw model %v, want [upstream/real-name]", models)
	}
}

func TestGatewayWildcardPassesUnknownNamesThrough(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:   gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts:  zenAccounts("a"),
		modelList: []config.ModelRoute{{ModelName: "*", Pool: "pool", UpstreamModel: "*"}},
		upstream:  http.HandlerFunc(alwaysOK),
	})

	resp := h.post(t, "secret-key", "something-nobody-listed")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, models := h.calls()
	// A wildcard route that maps to itself must not rewrite: the configured
	// UpstreamModel is "*", and sending that upstream would be nonsense.
	if len(models) != 1 || models[0] != "something-nobody-listed" {
		t.Fatalf("upstream saw model %v, want the original name", models)
	}
}

func TestGatewayRejectsAModelThePoolCannotServe(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:   gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts:  zenAccounts("a"),
		modelList: []config.ModelRoute{{ModelName: "known", Pool: "pool", UpstreamModel: "known"}},
		upstream:  http.HandlerFunc(alwaysOK),
	})

	resp := h.post(t, "secret-key", "unknown")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unlisted model with no wildcard", resp.StatusCode)
	}
	if seen, _ := h.calls(); len(seen) != 0 {
		t.Errorf("the request reached the upstream anyway: %v", seen)
	}
}

func TestGatewayKeyCannotReachAnotherPool(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway: gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: []model.Account{
			{ID: "a", Upstream: "zen", Pool: "pool", APIKey: "key-a", Proxy: "auto", Weight: 1, Enabled: true},
			{ID: "b", Upstream: "zen", Pool: "other", APIKey: "key-b", Proxy: "auto", Weight: 1, Enabled: true},
		},
		modelList: []config.ModelRoute{
			{ModelName: "mine", Pool: "pool", UpstreamModel: "mine"},
			{ModelName: "theirs", Pool: "other", UpstreamModel: "theirs"},
		},
		upstream: http.HandlerFunc(alwaysOK),
	})

	resp := h.post(t, "secret-key", "theirs")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a key must not reach a pool it does not own", resp.StatusCode)
	}
}

func TestGatewayReportsNoAccountRatherThan404(t *testing.T) {
	// An empty pool is an operator problem, not a client one, and the status
	// code has to say so or the operator will hunt for a routing bug.
	h := newGW(t, gwOptions{
		gateway: gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "empty"}),
		accounts: []model.Account{
			{ID: "a", Upstream: "zen", Pool: "empty", APIKey: "key-a", Proxy: "auto", Weight: 1, Enabled: false},
		},
		modelList: []config.ModelRoute{{ModelName: "m", Pool: "empty", UpstreamModel: "m"}},
		upstream:  http.HandlerFunc(alwaysOK),
	})

	resp := h.post(t, "secret-key", "m")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when every account in the pool is disabled", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// /v1/models
// ---------------------------------------------------------------------------

func TestGatewayModelListReflectsTheKeysPool(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway: gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: []model.Account{
			{ID: "a", Upstream: "zen", Pool: "pool", APIKey: "key-a", Proxy: "auto", Weight: 1, Enabled: true},
			{ID: "b", Upstream: "zen", Pool: "other", APIKey: "key-b", Proxy: "auto", Weight: 1, Enabled: true},
		},
		modelList: []config.ModelRoute{
			{ModelName: "zeta", Pool: "pool", UpstreamModel: "zeta"},
			{ModelName: "alpha", Pool: "pool", UpstreamModel: "alpha"},
			{ModelName: "hidden", Pool: "other", UpstreamModel: "hidden"},
		},
		upstream: http.HandlerFunc(alwaysOK),
	})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listing struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.Object != "list" {
		t.Errorf("object = %q, want \"list\"", listing.Object)
	}
	// The per-item fields are not decoration. An aggregator that imports this as
	// an upstream parses them, and one missing field is enough for it to drop the
	// model from its own listing - which surfaces as "the upstream has no models"
	// rather than as a field it could not read. Asserted per item, so a single
	// entry built on a different path cannot slip through.
	for _, m := range listing.Data {
		if m.Object != "model" {
			t.Errorf("%s: object = %q, want \"model\"", m.ID, m.Object)
		}
		if m.Created == 0 {
			t.Errorf("%s: created is missing; aggregators sort and display it", m.ID)
		}
		if m.OwnedBy == "" {
			t.Errorf("%s: owned_by is missing; it is the only hint of where a model came from", m.ID)
		}
	}
	var got []string
	for _, m := range listing.Data {
		got = append(got, m.ID)
	}
	want := []string{"alpha", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v (sorted, and only this pool's)", got, want)
	}
}

func TestGatewayModelListMergesTheUpstreamsOwnListingUnderAWildcard(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"from-upstream"},{"id":"explicit"}]}`))
			return
		}
		alwaysOK(w, r)
	})
	h := newGW(t, gwOptions{
		gateway:   gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts:  zenAccounts("a"),
		modelList: []config.ModelRoute{{ModelName: "explicit", Pool: "pool", UpstreamModel: "explicit"}, {ModelName: "*", Pool: "pool", UpstreamModel: "*"}},
		upstream:  upstream,
	})

	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got []string
	for _, m := range listing.Data {
		got = append(got, m.ID)
	}
	// The configured name and the discovered one, merged and deduped.
	want := "explicit,from-upstream"
	if strings.Join(got, ",") != want {
		t.Errorf("models = %v, want %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// the old ingress must keep working
// ---------------------------------------------------------------------------

func TestReverseIngressStillWorksAlongsideTheGateway(t *testing.T) {
	h := newGW(t, gwOptions{
		gateway:  gwConfig(config.GatewayKey{ID: "main", Key: "secret-key", Pool: "pool"}),
		accounts: zenAccounts("a"),
		upstream: http.HandlerFunc(alwaysOK),
	})

	// /<account-id>/... is what the deployed new-api channel points at; the
	// unified ingress must not have stolen it.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/a/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reverse ingress status = %d, want 200", resp.StatusCode)
	}
}
