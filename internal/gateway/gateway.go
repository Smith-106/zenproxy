// Package gateway turns "address one account by its URL path" into one OpenAI
// shaped endpoint with one key.
//
// The problem it solves: every gateway downstream of zenproxy (new-api,
// CLIProxyAPI, sub2api, LiteLLM) wants a single Base URL plus a single API key,
// and none of them can express "pick whichever account is healthy and rotate
// its egress IP when it is not". zenproxy can, so it exposes that ability as an
// ordinary upstream: one URL, one key, no account names in the path.
//
// A key is bound to a pool, not to an account. That binding is the whole point:
// it is what lets the daemon choose among interchangeable accounts without the
// client ever naming one.
package gateway

import (
	"crypto/subtle"
	"net/http"
	"sort"
	"strings"

	"zenproxy/internal/config"
)

// Key is one client credential and the pool it may draw from.
type Key struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Pool string `json:"pool"`
}

// View is the API-safe projection of the gateway for the admin panel. It never
// carries a key: the panel needs to show which keys exist, not what they are.
type View struct {
	Enabled    bool      `json:"enabled"`
	RequireKey bool      `json:"require_key"`
	Keys       []Key     `json:"keys"`
	ModelList  []Route   `json:"model_list"`
	Pools      []PoolRef `json:"pools"`
}

// Route is the API-safe projection of one model_list entry.
type Route struct {
	ModelName     string `json:"model_name"`
	Pool          string `json:"pool"`
	UpstreamModel string `json:"upstream_model"`
}

// PoolRef is a pool with the routes that name it.
type PoolRef struct {
	ID     string   `json:"id"`
	Models []string `json:"models"`
}

// Gateway resolves credentials and model names. It holds secrets, so it is
// never marshalled directly.
type Gateway struct {
	enabled    bool
	requireKey bool

	keys  map[string]Key
	order []Key

	routes   []config.ModelRoute
	wildcard *config.ModelRoute
}

// New builds the resolver from the gateway block and the model list.
func New(cfg config.Gateway, routes []config.ModelRoute) *Gateway {
	g := &Gateway{
		enabled: cfg.Enabled,
		keys:    make(map[string]Key, len(cfg.Keys)),
	}
	if cfg.RequireKey != nil {
		g.requireKey = *cfg.RequireKey
	} else {
		g.requireKey = len(cfg.Keys) > 0
	}
	for _, k := range cfg.Keys {
		if k.Key == "" {
			continue
		}
		// A duplicate key would silently make one of the two ids unreachable,
		// so the first one wins and config validation is what reports the
		// collision; the resolver stays deterministic either way.
		if _, dup := g.keys[k.Key]; dup {
			continue
		}
		kk := Key{ID: k.ID, Name: k.Name, Pool: k.Pool}
		g.keys[k.Key] = kk
		g.order = append(g.order, kk)
	}
	sort.Slice(g.order, func(i, j int) bool { return g.order[i].ID < g.order[j].ID })

	for _, r := range routes {
		if r.ModelName == "*" {
			rr := r
			g.wildcard = &rr
			continue
		}
		g.routes = append(g.routes, r)
	}
	return g
}

// Enabled reports whether the unified ingress should be served at all.
func (g *Gateway) Enabled() bool { return g != nil && g.enabled }

// RequireKey reports whether anonymous /v1/* traffic must be refused.
func (g *Gateway) RequireKey() bool { return g != nil && g.requireKey }

// HasKeys reports whether any credential is configured.
func (g *Gateway) HasKeys() bool { return g != nil && len(g.keys) > 0 }

// Authenticate resolves the caller's credential.
//
// Both header shapes are accepted because both are in the wild for the same
// endpoint: OpenAI clients send `Authorization: Bearer …` while Anthropic
// clients send `x-api-key: …`, and /v1/messages is served here too.
//
// The comparison is constant time. A key is a bearer secret, and a byte-by-byte
// early exit would leak it a character at a time to anyone who can measure.
func (g *Gateway) Authenticate(r *http.Request) (Key, bool) {
	if g == nil || len(g.keys) == 0 {
		return Key{}, false
	}
	presented := bearerToken(r.Header.Get("Authorization"))
	if presented == "" {
		presented = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if presented == "" {
		return Key{}, false
	}
	for candidate, k := range g.keys {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(presented)) == 1 {
			return k, true
		}
	}
	return Key{}, false
}

// bearerToken pulls the credential out of an Authorization header, tolerating
// the scheme being absent or differently cased.
func bearerToken(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if strings.ContainsRune(h, ' ') {
		// Some other scheme (Basic, Token…) is not ours to interpret.
		return ""
	}
	return h
}

// ResolveModel maps the name a client asked for onto a pool and the upstream's
// own name for it.
//
// The second result is false when nothing matches, which is the caller's signal
// to reject the request rather than forward a name the pool cannot serve.
//
// A gateway with no model_list at all passes every name through. That is not a
// convenience: "one URL and one key, the pool picks the account" is the whole
// point of this ingress, and forcing an operator to enumerate model names
// before anything works would make the simple case the hard one.
func (g *Gateway) ResolveModel(name string) (config.ModelRoute, bool) {
	if g == nil || name == "" {
		return config.ModelRoute{}, false
	}
	for _, r := range g.routes {
		if r.ModelName == name {
			return r, true
		}
	}
	if g.wildcard != nil {
		return config.ModelRoute{
			ModelName:     name,
			Pool:          g.wildcard.Pool,
			UpstreamModel: name,
		}, true
	}
	if len(g.routes) == 0 {
		return config.ModelRoute{ModelName: name, UpstreamModel: name}, true
	}
	return config.ModelRoute{}, false
}

// Routes lists the explicitly named routes of a pool, sorted. A wildcard route
// contributes nothing here because its model names are not knowable from the
// config — the caller merges the upstream's own listing for that case.
func (g *Gateway) Routes(pool string) []config.ModelRoute {
	if g == nil {
		return nil
	}
	var out []config.ModelRoute
	for _, r := range g.routes {
		if pool == "" || r.Pool == pool {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelName < out[j].ModelName })
	return out
}

// HasWildcard reports whether unknown names are allowed through, either
// explicitly via "*" or implicitly because no model_list was configured at all.
func (g *Gateway) HasWildcard(pool string) bool {
	if g == nil {
		return false
	}
	if g.wildcard != nil {
		return pool == "" || g.wildcard.Pool == pool
	}
	return len(g.routes) == 0
}

// WildcardPool names the pool an unknown model would be routed to.
func (g *Gateway) WildcardPool() string {
	if g == nil || g.wildcard == nil {
		return ""
	}
	return g.wildcard.Pool
}

// View projects the gateway for the admin API.
func (g *Gateway) View() View {
	if g == nil {
		return View{}
	}
	v := View{
		Enabled:    g.enabled,
		RequireKey: g.requireKey,
		Keys:       append([]Key(nil), g.order...),
	}
	for _, pool := range g.poolsOfRoutes() {
		ref := PoolRef{ID: pool}
		for _, r := range g.Routes(pool) {
			ref.Models = append(ref.Models, r.ModelName)
		}
		v.Pools = append(v.Pools, ref)
	}
	for _, r := range g.routes {
		v.ModelList = append(v.ModelList, Route{
			ModelName:     r.ModelName,
			Pool:          r.Pool,
			UpstreamModel: r.UpstreamModel,
		})
	}
	return v
}

func (g *Gateway) poolsOfRoutes() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range g.routes {
		if r.Pool == "" || seen[r.Pool] {
			continue
		}
		seen[r.Pool] = true
		out = append(out, r.Pool)
	}
	sort.Strings(out)
	return out
}
