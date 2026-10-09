package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zenproxy/internal/admin"
	"zenproxy/internal/config"
	"zenproxy/internal/forward"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// These tests cover the routing table in newMux, which is the one part of the
// unified ingress that package forward cannot reach: inside internal/forward
// the tests build their own mux, so nothing would notice if the real binary
// let splitAccount claim "/v1" first. That mistake produces
// "unknown or disabled account: v1" for every gateway client — a 404 that
// looks like the caller's fault.

type routerFixture struct {
	mux    *http.ServeMux
	srv    *httptest.Server
	seen   []string // Authorization headers the upstream received
	models []string // model names the upstream received
	reg    *registry.Registry
}

func newRouter(t *testing.T, gw config.Gateway, routes []config.ModelRoute) *routerFixture {
	t.Helper()
	f := &routerFixture{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		f.seen = append(f.seen, r.Header.Get("Authorization"))
		f.models = append(f.models, probe.Model)
		if strings.HasSuffix(r.URL.Path, "/models") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"from-upstream"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","path":"` + r.URL.Path + `"}`))
	}))
	t.Cleanup(up.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := proxypool.NewPool(nil, config.Health{Timeout: time.Second}, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	reg := registry.New("", pool, log)
	for _, id := range []string{"zen-01", "zen-02"} {
		if err := reg.Upsert(model.Account{
			ID: id, Upstream: "zen", Pool: "zen", APIKey: "key-" + id,
			Proxy: "direct", Weight: 1, Enabled: true,
		}); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	cfg := config.Config{
		Server:    config.Server{Listen: "127.0.0.1:0", AdminToken: "test-admin"},
		Upstreams: map[string]model.Upstream{"zen": {BaseURL: up.URL + "/zen/v1"}},
		Gateway:   gw,
		ModelList: routes,
		StateFile: "",
	}
	eng := forward.New(reg, pool, forward.Options{
		Upstreams: cfg.Upstreams,
		Policy: config.Policy{
			MaxAttempts: 2, RotateOnStatus: []int{429}, MaxBodyBytes: 1 << 20,
			StreamIdleTimeout: time.Second, UpstreamTimeout: 10 * time.Second,
		},
		Log:       log,
		FreeTier:  config.FreeTier{Enabled: false},
		Gateway:   gw,
		ModelList: routes,
	})

	f.reg = reg
	f.mux = newMux(eng, admin.New(reg, pool, cfg, eng.FreeTier(), log))
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	return f
}

func gatewayOn() config.Gateway {
	req := true
	return config.Gateway{
		Enabled: true,
		Keys: []config.GatewayKey{
			{ID: "main", Key: "sk-gateway-secret", Pool: "zen"},
		},
		RequireKey: &req,
	}
}

func TestRouterSendsV1ToTheGatewayNotToTheAccountSplitter(t *testing.T) {
	f := newRouter(t, gatewayOn(), nil)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"big-pickle","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-gateway-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (%s); /v1/* was probably claimed by the account splitter", resp.StatusCode, body)
	}
	// The account's key must reach the upstream — never the gateway key. If the
	// client credential were forwarded, the gateway would be handing its own
	// secret to every upstream it talks to.
	if len(f.seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1: %v", len(f.seen), f.seen)
	}
	if f.seen[0] == "Bearer sk-gateway-secret" {
		t.Fatal("the gateway key was forwarded upstream")
	}
	if !strings.HasPrefix(f.seen[0], "Bearer key-zen-0") {
		t.Errorf("upstream Authorization = %q, want an account key", f.seen[0])
	}
}

func TestRouterStillServesReverseModeForAnAccountPath(t *testing.T) {
	f := newRouter(t, gatewayOn(), nil)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/zen-01/v1/chat/completions",
		strings.NewReader(`{"model":"big-pickle"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reverse mode status = %d, want 200", resp.StatusCode)
	}
	if len(f.seen) != 1 || f.seen[0] != "Bearer key-zen-01" {
		t.Errorf("upstream saw %v, want the zen-01 key", f.seen)
	}
}

func TestRouterRefusesAV1RequestWithoutAKey(t *testing.T) {
	// The unified ingress must not be an open relay, and this is the layer
	// where an unauthenticated request would otherwise reach the accounts.
	f := newRouter(t, gatewayOn(), nil)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"big-pickle"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if len(f.seen) != 0 {
		t.Errorf("an unauthenticated request reached the upstream: %v", f.seen)
	}
}

func TestRouterKeepsHealthAndAdminReachable(t *testing.T) {
	f := newRouter(t, gatewayOn(), nil)

	resp, err := http.Get(f.srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	// A bare "/" has no account in it and no /v1 prefix, so it must fail with
	// usage text rather than being mistaken for anything else.
	resp2, err := http.Get(f.srv.URL + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("/ = %d, want 404 with usage text", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "/v1/chat/completions") {
		t.Errorf("the usage message no longer mentions the unified ingress: %s", body)
	}
}

func TestRouterGatewayDisabledSaysSoRatherThanUnknownAccount(t *testing.T) {
	// With the feature off, /v1/* must explain that, not report "unknown
	// account: v1" — that message sends the operator hunting for a config typo
	// in the wrong file.
	f := newRouter(t, config.Gateway{}, nil)

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "disabled") {
		t.Errorf("body = %s, want it to say the /v1 ingress is disabled", body)
	}
}

func TestRouterModelListReportsDiscoverableModels(t *testing.T) {
	// No model_list means pass-through, so the listing must include what the
	// upstream itself reports — that is what a downstream gateway imports.
	f := newRouter(t, gatewayOn(), nil)

	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-gateway-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "from-upstream" {
		t.Errorf("data = %+v, want the upstream's own listing", listing.Data)
	}
}
