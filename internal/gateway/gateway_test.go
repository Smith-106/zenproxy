package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

func cfg(keys ...config.GatewayKey) config.Gateway {
	req := true
	return config.Gateway{Enabled: true, Keys: keys, RequireKey: &req}
}

func TestAuthenticateAcceptsBothHeaderShapes(t *testing.T) {
	g := New(cfg(config.GatewayKey{ID: "main", Key: "sk-live-abc", Pool: "p"}), nil)

	cases := []struct {
		name   string
		header string
		value  string
		want   bool
	}{
		{"bearer", "Authorization", "Bearer sk-live-abc", true},
		{"bearer lowercase scheme", "Authorization", "bearer sk-live-abc", true},
		{"bare token", "Authorization", "sk-live-abc", true},
		{"anthropic header", "x-api-key", "sk-live-abc", true},
		{"wrong key", "Authorization", "Bearer sk-live-abd", true == false},
		{"empty", "Authorization", "", false},
		{"different scheme", "Authorization", "Basic sk-live-abc", false},
		{"prefix only", "Authorization", "Bearer sk-live", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if c.value != "" {
			r.Header.Set(c.header, c.value)
		}
		got, ok := g.Authenticate(r)
		if ok != c.want {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.want)
			continue
		}
		if ok && got.ID != "main" {
			t.Errorf("%s: resolved key id %q, want main", c.name, got.ID)
		}
	}
}

func TestAuthenticateNeverResolvesAnEmptyCredential(t *testing.T) {
	// A key configured as "" would otherwise match every anonymous request, so
	// New must drop it. This is the difference between a gateway and an open
	// relay.
	g := New(cfg(config.GatewayKey{ID: "broken", Key: "", Pool: "p"}), nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if _, ok := g.Authenticate(r); ok {
		t.Fatal("an empty configured key authenticated an anonymous request")
	}
	if g.HasKeys() {
		t.Error("HasKeys = true for a config whose only key is empty")
	}
}

func TestResolveModel(t *testing.T) {
	routes := []config.ModelRoute{
		{ModelName: "friendly", Pool: "p", UpstreamModel: "vendor/real"},
	}
	g := New(cfg(config.GatewayKey{ID: "main", Key: "k", Pool: "p"}), routes)

	got, ok := g.ResolveModel("friendly")
	if !ok || got.UpstreamModel != "vendor/real" || got.Pool != "p" {
		t.Errorf("friendly => %+v,%v", got, ok)
	}
	if _, ok := g.ResolveModel("other"); ok {
		t.Error("an unlisted model resolved despite there being a model_list and no wildcard")
	}
}

func TestResolveModelPassesEverythingThroughWithoutAModelList(t *testing.T) {
	// "One URL and one key, the pool picks the account" must work with no
	// model_list at all, otherwise the simplest deployment is the one that
	// silently 404s.
	g := New(cfg(config.GatewayKey{ID: "main", Key: "k", Pool: "p"}), nil)
	got, ok := g.ResolveModel("whatever-the-client-sends")
	if !ok {
		t.Fatal("no model_list should mean pass-through, not rejection")
	}
	if got.UpstreamModel != "whatever-the-client-sends" || got.Pool != "" {
		t.Errorf("got %+v; the pool must come from the key, not the route", got)
	}
	if !g.HasWildcard("p") {
		t.Error("HasWildcard must report true when no model_list is configured")
	}
}

func TestResolveModelWildcardKeepsTheOriginalName(t *testing.T) {
	g := New(cfg(config.GatewayKey{ID: "main", Key: "k", Pool: "p"}),
		[]config.ModelRoute{{ModelName: "*", Pool: "p", UpstreamModel: "*"}})
	got, ok := g.ResolveModel("some-model")
	if !ok || got.UpstreamModel != "some-model" {
		t.Errorf("got %+v,%v; a wildcard must not send the literal \"*\" upstream", got, ok)
	}
}

func TestViewNeverExposesAKey(t *testing.T) {
	g := New(cfg(config.GatewayKey{ID: "main", Key: "sk-super-secret", Pool: "p", Name: "console"}),
		[]config.ModelRoute{{ModelName: "m", Pool: "p", UpstreamModel: "m"}})

	blob, err := json.Marshal(g.View())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(blob), "sk-super-secret") {
		t.Fatalf("the admin view leaked the key: %s", blob)
	}
	var v View
	if err := json.Unmarshal(blob, &v); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(v.Keys) != 1 || v.Keys[0].ID != "main" || v.Keys[0].Pool != "p" {
		t.Errorf("keys = %+v", v.Keys)
	}
	if len(v.ModelList) != 1 || v.ModelList[0].ModelName != "m" {
		t.Errorf("model_list = %+v", v.ModelList)
	}
}

func TestRequireKeyDefaultsToTrueWhenAKeyExists(t *testing.T) {
	no := false
	g := New(config.Gateway{
		Enabled: true,
		Keys:    []config.GatewayKey{{ID: "main", Key: "k", Pool: "p"}},
	}, nil)
	if !g.RequireKey() {
		t.Error("a gateway with keys must require one by default")
	}
	g = New(config.Gateway{
		Enabled:    true,
		Keys:       []config.GatewayKey{{ID: "main", Key: "k", Pool: "p"}},
		RequireKey: &no,
	}, nil)
	if g.RequireKey() {
		t.Error("require_key: false must be honoured")
	}
}

func TestNilGatewayIsInert(t *testing.T) {
	// The engine holds a *Gateway even when the feature is unconfigured, so
	// every method has to survive a nil receiver rather than panic at request
	// time.
	var g *Gateway
	if g.Enabled() || g.RequireKey() || g.HasKeys() {
		t.Error("a nil gateway reported itself as active")
	}
	if _, ok := g.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Error("a nil gateway authenticated a request")
	}
	if _, ok := g.ResolveModel("m"); ok {
		t.Error("a nil gateway resolved a model")
	}
	if g.HasWildcard("p") {
		t.Error("a nil gateway claimed a wildcard")
	}
}
