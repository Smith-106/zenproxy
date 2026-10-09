package forward

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/gateway"
	"zenproxy/internal/model"
	"zenproxy/internal/registry"
)

// gatewayExitsPerAccount is how many different exit IPs one account may burn
// before the chooser gives up on it and moves to another account.
//
// Two is a deliberate compromise. One would mean a single unlucky IP condemns
// the account; more than two spends the pool's good exits on an account that
// may be the thing that is actually broken.
const gatewayExitsPerAccount = 2

// gatewayMaxAttempts bounds the total work one gateway request may do. Without
// a ceiling, a large pool would turn a slow upstream into a request that takes
// minutes to fail.
const gatewayMaxAttempts = 8

// ServeGateway serves the unified ingress:
//
//	POST /v1/chat/completions
//	POST /v1/messages
//	GET  /v1/models
//	Authorization: Bearer <gateway key>
//
// The account and its exit are chosen here, which is the whole difference from
// reverse mode: the client names a pool indirectly, by presenting a key, and
// never names an account at all.
func (e *Engine) ServeGateway(w http.ResponseWriter, r *http.Request, rest string) {
	if !e.gw.Enabled() {
		writeJSONError(w, http.StatusNotFound,
			"the unified /v1 ingress is disabled; enable gateway.enabled or use /<account-id>/<path>")
		return
	}

	key, ok := e.gw.Authenticate(r)
	if !ok {
		if !e.gw.HasKeys() && !e.gw.RequireKey() {
			// Explicitly opened by the operator; fall through with an empty pool
			// so the route table decides.
			key = gateway.Key{}
		} else {
			// WWW-Authenticate is not decoration: several clients only send the
			// credential after being challenged once.
			w.Header().Set("WWW-Authenticate", `Bearer realm="zenproxy"`)
			writeJSONError(w, http.StatusUnauthorized,
				"missing or unknown API key; send Authorization: Bearer <key>")
			return
		}
	}

	if isModelsPath(rest) {
		e.writeModelList(w, r, key.Pool)
		return
	}

	body, truncated, err := readBody(r, e.pol.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	if truncated {
		// Routing needs the model name, which lives in the part that was not
		// buffered. Refusing is honest; guessing would misroute silently.
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			"request body is too large for the unified ingress (it must be buffered to resolve the model); use /<account-id>/<path> to stream it through untouched")
		return
	}

	asked := modelField(body)
	if asked == "" {
		writeJSONError(w, http.StatusBadRequest, `the request body has no "model" field`)
		return
	}
	route, ok := e.gw.ResolveModel(asked)
	if !ok {
		writeJSONError(w, http.StatusNotFound,
			fmt.Sprintf("model %q is not in model_list and no \"*\" route accepts unknown names", asked))
		return
	}
	if route.Pool != "" && key.Pool != "" && route.Pool != key.Pool {
		writeJSONError(w, http.StatusForbidden,
			fmt.Sprintf("key %q may use pool %q, but model %q is served by pool %q", key.ID, key.Pool, asked, route.Pool))
		return
	}
	pool := route.Pool
	if pool == "" {
		pool = key.Pool
	}

	// Rewrite the name only when the route maps it somewhere else. A route that
	// is its own upstream model must leave the bytes alone, so a deployment
	// that only passes names through keeps sending them verbatim.
	if route.UpstreamModel != asked && route.UpstreamModel != "" {
		if rewritten, err := setModelField(body, route.UpstreamModel); err == nil {
			body = rewritten
		} else {
			writeJSONError(w, http.StatusBadRequest, "could not rewrite the model field: "+err.Error())
			return
		}
	}

	pol := e.policyFor(pool)
	// Keyed on the upstream model, not the name the caller asked for: the
	// upstream decides geography from the model it receives, and if a route
	// renamed it, only the rewritten name appears in the refusal. proxyLoop
	// reads the same value back out of the rewritten body, so the pairing the
	// chooser picks with and the pairing the refusal records are the same key.
	ch := e.newPoolChooser(pool, pol, rest, route.UpstreamModel)
	e.log.Info("gateway routing",
		"key", key.ID, "pool", pool, "model", asked, "upstream_model", route.UpstreamModel,
		"strategy", pol.Strategy, "on_rate_limit", pol.OnRateLimit)
	e.proxyLoop(w, r, ch, body, false)
}

// isModelsPath matches the model-listing endpoint, tolerating a trailing slash
// and the /v1 prefix being present or absent.
func isModelsPath(rest string) bool {
	p := strings.TrimSuffix(rest, "/")
	return p == "/v1/models" || p == "/models"
}

// modelField reads the "model" string out of a request body.
func modelField(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// setModelField replaces "model" in place, leaving every other field byte for
// byte as the client wrote it. Decoding into a map and re-encoding is what
// keeps temperature, tools and provider options intact; only key order moves,
// which no upstream inspects.
func setModelField(body []byte, name string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(name)
	if err != nil {
		return nil, err
	}
	obj["model"] = raw
	return json.Marshal(obj)
}

// poolChooser walks a pool for one gateway request.
type poolChooser struct {
	eng     *Engine
	pool    string
	pol     config.PoolPolicy
	rest    string
	members []*registry.Entry
	current *registry.Entry
	// mdl is the model the caller asked for. The gateway resolves it from the
	// route before any attempt is made, so unlike the reverse ingress there is
	// no window where it is unknown. It is kept on the chooser rather than
	// threaded through first() because the first pick happens before the loop
	// that would otherwise be able to supply it.
	mdl      string
	exitsFor map[string]int
	tried    map[string]bool
}

func (e *Engine) newPoolChooser(pool string, pol config.PoolPolicy, rest, mdl string) *poolChooser {
	return &poolChooser{
		eng:      e,
		pool:     pool,
		pol:      pol,
		rest:     rest,
		mdl:      mdl,
		members:  e.reg.PoolMembers(pool),
		exitsFor: map[string]int{},
		tried:    map[string]bool{},
	}
}

func (c *poolChooser) budget() int {
	n := len(c.members) * gatewayExitsPerAccount
	if c.pol.OnRateLimit == config.RotateAccount {
		n = len(c.members) // no per-account exit retries under this policy
	}
	if n < 1 {
		n = 1
	}
	if n > gatewayMaxAttempts {
		n = gatewayMaxAttempts
	}
	return n
}

func (c *poolChooser) first() (candidate, bool) {
	ent := c.eng.pickAccount(c.pool, c.pol.Strategy, nil, c.mdl)
	if ent == nil {
		return candidate{}, false
	}
	c.current = ent
	return c.build(ent)
}

// next decides what "switch" means for this pool.
//
//   - rotate-exit keeps the credential and changes the IP, which is the right
//     answer when the upstream limits per IP.
//   - rotate-account keeps the IP and changes the credential, which is the
//     right answer when the upstream limits per key (and the only one left once
//     every good IP for this account has been tried).
//   - rotate-both does the first, then the second, which is why it needs the
//     per-account exit counter.
func (c *poolChooser) next(failed *registry.Entry, _ model.ErrorClass, exitID, mdl string) (candidate, bool) {
	if failed == nil {
		return candidate{}, false
	}
	switch c.pol.OnRateLimit {
	case config.RotateAccount:
		c.tried[failed.ID] = true
		return c.switchAccount(mdl)
	default:
		c.exitsFor[failed.ID]++
		if c.pol.OnRateLimit == config.RotateBoth && c.exitsFor[failed.ID] >= gatewayExitsPerAccount {
			// This account has spent its share of exits; the failure now looks
			// more like the account than like the IP.
			c.tried[failed.ID] = true
			if cand, ok := c.switchAccount(mdl); ok {
				return cand, true
			}
			// Nothing else in the pool: fall back to moving this one again
			// rather than giving up while exits remain.
			c.eng.rotateExitFor(failed.ID, exitID, mdl)
			return c.build(failed)
		}
		c.eng.rotateExitFor(failed.ID, exitID, mdl)
		return c.build(failed)
	}
}

func (c *poolChooser) switchAccount(mdl string) (candidate, bool) {
	ent := c.eng.pickAccount(c.pool, c.pol.Strategy, c.tried, mdl)
	if ent == nil {
		return candidate{}, false
	}
	c.current = ent
	return c.build(ent)
}

// build resolves where this account sends the request.
func (c *poolChooser) build(ent *registry.Entry) (candidate, bool) {
	up, ok := c.eng.ups[ent.Upstream]
	if !ok {
		c.eng.log.Warn("account references an unknown upstream",
			"account", ent.ID, "upstream", ent.Upstream)
		return candidate{}, false
	}
	target, err := joinUpstreamPath(up.BaseURL, c.rest)
	if err != nil {
		c.eng.log.Warn("cannot build the upstream URL", "account", ent.ID, "err", err)
		return candidate{}, false
	}
	return candidate{ent: ent, up: up, target: target}, true
}

// writeModelList answers GET /v1/models.
//
// It reports what the key's pool can actually serve. Named routes come from the
// config; a "*" route contributes nothing by itself, so in that case the
// upstream's own listing is fetched through one of the pool's accounts and
// merged, which is what makes the endpoint useful to a downstream gateway that
// auto-imports a channel's model list.
func (e *Engine) writeModelList(w http.ResponseWriter, r *http.Request, pool string) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, rt := range e.gw.Routes(pool) {
		add(rt.ModelName)
	}
	if e.gw.HasWildcard(pool) {
		if pool == "" {
			pool = e.gw.WildcardPool()
		}
		for _, id := range e.fetchUpstreamModels(r, pool) {
			add(id)
		}
	}
	sort.Strings(ids)

	data := make([]map[string]any, 0, len(ids))
	now := time.Now().Unix()
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  now,
			"owned_by": "zenproxy",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// fetchUpstreamModels asks one account in the pool for its /v1/models listing.
// It is strictly best-effort: a gateway that cannot reach the upstream right
// now should still be able to answer with the names it knows from config.
func (e *Engine) fetchUpstreamModels(r *http.Request, pool string) []string {
	ent := e.reg.PickInPool(pool, "", nil)
	if ent == nil {
		return nil
	}
	up, ok := e.ups[ent.Upstream]
	if !ok {
		return nil
	}
	target, err := joinUpstreamPath(up.BaseURL, "/v1/models")
	if err != nil {
		return nil
	}
	req, cancel, err := e.buildRequest(r, target.String(), nil, 0, ent, up, nil)
	if err != nil {
		return nil
	}
	defer cancel()
	req.Method = http.MethodGet
	// A model listing is a GET: carrying the client's body would make some
	// upstreams reject it outright.
	req.Body = nil
	req.GetBody = nil
	req.ContentLength = 0

	proxy := e.reg.ExitFor(ent.ID)
	client := e.clientFor(proxy)
	// Short and separate from UpstreamTimeout: this call decorates a listing,
	// it is not the request the user is waiting for.
	client.Timeout = 20 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		e.log.Warn("could not list upstream models", "pool", pool, "account", ent.ID, "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil
	}
	out := make([]string, 0, len(listing.Data))
	for _, m := range listing.Data {
		out = append(out, m.ID)
	}
	return out
}

// policyFor returns a pool's rotation policy.
func (e *Engine) policyFor(pool string) config.PoolPolicy {
	if e.poolPolicy == nil {
		return config.PoolPolicy{ID: pool, Strategy: config.StrategyRR, OnRateLimit: config.RotateBoth}
	}
	return e.poolPolicy(pool)
}

// Gateway exposes the resolver for the admin panel.
func (e *Engine) Gateway() *gateway.Gateway { return e.gw }

// upstreamURLFor is a test seam: it reports where a pool would send a path.
func (e *Engine) upstreamURLFor(pool, rest string) (*url.URL, bool) {
	ent := e.reg.PickInPool(pool, "", nil)
	if ent == nil {
		return nil, false
	}
	up, ok := e.ups[ent.Upstream]
	if !ok {
		return nil, false
	}
	target, err := joinUpstreamPath(up.BaseURL, rest)
	if err != nil {
		return nil, false
	}
	return target, true
}
