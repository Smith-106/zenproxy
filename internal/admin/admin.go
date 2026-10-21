// Package admin exposes the management API and its embedded dashboard.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/gateway"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// FreeTierInfo is what the dashboard needs to know about the free-tier
// envelope. It stays an interface so the admin package does not have to import
// the forwarding engine.
type FreeTierInfo interface {
	// Learned lists the models that were enveloped only after the upstream
	// refused them, i.e. the ones the configured list is missing.
	Learned() []string
}

// Server serves the management surface.
type Server struct {
	reg     *registry.Registry
	pool    *proxypool.Pool
	cfg     config.Config
	free    FreeTierInfo
	token   string
	log     *slog.Logger
	started time.Time
}

// New builds the management server. free may be nil when the envelope is off.
func New(reg *registry.Registry, pool *proxypool.Pool, cfg config.Config, free FreeTierInfo, log *slog.Logger) *Server {
	return &Server{
		reg:     reg,
		pool:    pool,
		cfg:     cfg,
		free:    free,
		token:   cfg.Server.AdminToken,
		log:     log,
		started: time.Now(),
	}
}

// Handler returns the admin mux, every route gated behind the admin token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/api/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /admin/api/preflight", s.auth(s.handlePreflight))
	mux.HandleFunc("GET /admin/api/accounts", s.auth(s.handleListAccounts))
	mux.HandleFunc("POST /admin/api/accounts", s.auth(s.handleUpsertAccount))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", s.auth(s.handleDeleteAccount))
	mux.HandleFunc("POST /admin/api/accounts/{id}/rotate", s.auth(s.handleRotateAccount))
	mux.HandleFunc("GET /admin/api/proxies", s.auth(s.handleListProxies))
	mux.HandleFunc("POST /admin/api/proxies/probe", s.auth(s.handleProbe))
	mux.HandleFunc("POST /admin/api/proxies/prune", s.auth(s.handlePrune))
	mux.HandleFunc("POST /admin/api/proxies/{id}/retire", s.auth(s.handleRetireExit))
	mux.HandleFunc("POST /admin/api/proxies/{id}/revive", s.auth(s.handleReviveExit))
	mux.HandleFunc("POST /admin/api/proxies/{id}/uncool", s.auth(s.handleUncoolExit))
	mux.HandleFunc("POST /admin/api/proxies/{id}/clear-region", s.auth(s.handleClearRegion))
	mux.HandleFunc("GET /admin/", s.auth(s.handleUI))
	return mux
}

// auth accepts the token from a bearer header, a dedicated header, or the
// query string. The query form exists only so a browser can open /admin/?token=…
// once; the page then moves it into localStorage and drops it from the URL.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		if token == "" {
			token = r.Header.Get("X-Admin-Token")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if s.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "unauthorized: send X-Admin-Token, Authorization: Bearer, or ?token=",
			})
			return
		}
		next(w, r)
	}
}

// Health is an unauthenticated liveness probe.
func (s *Server) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// boolGauge renders a boolean as a Prometheus gauge value.
func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Metrics renders a minimal Prometheus exposition.
func (s *Server) Metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	fmt.Fprintf(&b, "# TYPE zenproxy_up gauge\nzenproxy_up 1\n")
	fmt.Fprintf(&b, "# TYPE zenproxy_accounts gauge\nzenproxy_accounts %d\n", len(s.reg.List()))
	fmt.Fprintf(&b, "# TYPE zenproxy_exits gauge\nzenproxy_exits %d\n", s.pool.Len())
	fmt.Fprintf(&b, "# TYPE zenproxy_exits_healthy gauge\nzenproxy_exits_healthy %d\n", s.pool.HealthyCount())
	fmt.Fprintf(&b, "# TYPE zenproxy_exits_retired gauge\nzenproxy_exits_retired %d\n", s.pool.RetiredCount())
	tripped, fails, limit := s.pool.OutageState()
	fmt.Fprintf(&b, "# TYPE zenproxy_pool_outage gauge\nzenproxy_pool_outage %d\n", boolGauge(tripped))
	fmt.Fprintf(&b, "# TYPE zenproxy_pool_outage_failures gauge\nzenproxy_pool_outage_failures %d\n", fails)
	fmt.Fprintf(&b, "# TYPE zenproxy_pool_outage_threshold gauge\nzenproxy_pool_outage_threshold %d\n", limit)
	fmt.Fprintf(&b, "# TYPE zenproxy_uptime_seconds gauge\nzenproxy_uptime_seconds %.0f\n", time.Since(s.started).Seconds())
	fmt.Fprintf(&b, "# TYPE zenproxy_account_requests counter\n")
	fmt.Fprintf(&b, "# TYPE zenproxy_account_failures counter\n")
	for id, st := range s.reg.Snapshot() {
		fmt.Fprintf(&b, "zenproxy_account_requests{account=%q} %d\n", id, st.Requests)
		fmt.Fprintf(&b, "zenproxy_account_failures{account=%q} %d\n", id, st.Failures)
	}
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.reg.List()
	enabled := 0
	var totalReq, totalOK, totalFail, totalRot int64
	for _, a := range accounts {
		if a.Enabled {
			enabled++
		}
		totalReq += a.Stats.Requests
		totalOK += a.Stats.Success
		totalFail += a.Stats.Failures
		totalRot += a.Stats.Rotations
	}
	upstreams := make([]string, 0, len(s.cfg.Upstreams))
	for name := range s.cfg.Upstreams {
		upstreams = append(upstreams, name)
	}
	sort.Strings(upstreams)
	var learned []string
	if s.free != nil {
		learned = s.free.Learned()
		sort.Strings(learned)
	}
	cooled := s.pool.QuotaCooledIDs()
	tripped, outageFails, outageLimit := s.pool.OutageState()
	// Computed once: asking twice would allocate twice and could report a count
	// that does not match the list under a concurrent write.
	blocks := s.pool.RegionBlocks()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":   time.Since(s.started).Seconds(),
		"listen":           s.cfg.Server.Listen,
		"accounts":         len(accounts),
		"accounts_enabled": enabled,
		"exits":            s.pool.Len(),
		"exits_healthy":    s.pool.HealthyCount(),
		"exits_watching":   s.pool.WatchCount(),
		"exits_retired":    s.pool.RetiredCount(),
		"exits_slow":       s.pool.SlowCount(),
		"exits_gate":       len(s.pool.GatePassers()),
		"quarantine":       s.pool.QuarantineBreakdown(),
		"outage_breaker": map[string]any{
			"open":                 tripped,
			"consecutive_failures": outageFails,
			"threshold":            outageLimit,
		},
		"requests":         totalReq,
		"success":          totalOK,
		"failures":         totalFail,
		"rotations":        totalRot,
		"upstreams":        upstreams,
		"allowed_hosts":    s.cfg.Policy.AllowedHosts,
		"state_file":       s.cfg.StateFile,
		"replicas":         s.pool.StateSnapshots(),
		"replica_split":    s.pool.StateSplit(),
		"account_replicas": s.reg.ReplicaAccountCounters(),
		// Read-only view of the knobs that shape rotation, so the console can
		// show why the proxy behaves the way it does without opening the YAML.
		// Nothing here is a secret: tokens and keys are deliberately absent.
		"policy": map[string]any{
			"max_attempts":        s.cfg.Policy.MaxAttempts,
			"rotate_on_status":    s.cfg.Policy.RotateOnStatus,
			"rotate_on_transport": s.cfg.Policy.RotateOnTransport,
			"max_body_bytes":      s.cfg.Policy.MaxBodyBytes,
			"stream_idle_timeout": s.cfg.Policy.StreamIdleTimeout.String(),
			"upstream_timeout":    s.cfg.Policy.UpstreamTimeout.String(),
		},
		"health": map[string]any{
			"interval":                s.cfg.Health.Interval.String(),
			"timeout":                 s.cfg.Health.Timeout.String(),
			"failure_threshold":       s.cfg.Health.FailureThreshold,
			"quarantine":              s.cfg.Health.Quarantine.String(),
			"max_backoff":             s.cfg.Health.MaxBackoff.String(),
			"max_latency":             s.cfg.Health.MaxLatency.String(),
			"max_latency_ms":          s.cfg.Health.MaxLatency.Milliseconds(),
			"retire_after":            s.cfg.Health.RetireAfter,
			"tls_poison_retire_after": proxypool.PoisonRetireAfter,
		},
		"free_tier": map[string]any{
			"enabled": s.cfg.FreeTier.Enabled,
			"models":  s.cfg.FreeTier.Models,
			// Models discovered by refusal: add them to free_tier.models in the
			// config to save the wasted first attempt.
			"learned": learned,
		},
		// The unified ingress: one URL, one key. The view lists which keys exist
		// and which pool each may draw from, never the keys themselves.
		"gateway": gateway.New(s.cfg.Gateway, s.cfg.ModelList).View(),
		"pools":   s.poolView(),
		// Which exits the upstream has told us are out of allowance, and when
		// each comes back. This is the question an operator actually asks when
		// every request is being refused: is the pool broken, or is it simply
		// spent — and the two need different answers, one of which is "wait".
		"quota": map[string]any{
			"cooled": cooled,
			"count":  len(cooled),
			"total":  s.pool.Len(),
		},
		// Which exit-and-model pairs the upstream refuses on geography. Scoped to
		// the pair rather than the exit because that is what was measured: an
		// exit can serve one model and be refused another in the same second.
		// Without this the only way to answer "is this model unavailable
		// everywhere, or just from some exits" is to probe by hand.
		"region": map[string]any{
			"blocked": blocks,
			"count":   len(blocks),
		},
	})
}

// poolView reports each pool with its members, its rotation policy and the
// model names routed to it. This is the answer to "which accounts can serve
// this model, and what happens when one of them is rate limited".
func (s *Server) poolView() []map[string]any {
	gw := gateway.New(s.cfg.Gateway, s.cfg.ModelList)
	var out []map[string]any
	for _, id := range s.cfg.PoolIDs() {
		pol := s.cfg.PolicyFor(id)
		var members []string
		for _, a := range s.reg.List() {
			if a.Pool == id {
				members = append(members, a.ID)
			}
		}
		sort.Strings(members)
		models := []string{}
		for _, rt := range gw.Routes(id) {
			models = append(models, rt.ModelName)
		}
		out = append(out, map[string]any{
			"id":            id,
			"strategy":      pol.Strategy,
			"on_rate_limit": pol.OnRateLimit,
			"accounts":      members,
			"models":        models,
			"wildcard":      gw.HasWildcard(id),
		})
	}
	return out
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.reg.List()})
}

type accountRequest struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Upstream string            `json:"upstream"`
	Pool     string            `json:"pool"`
	APIKey   string            `json:"api_key"`
	Proxy    string            `json:"proxy"`
	Weight   *int              `json:"weight"`
	Enabled  *bool             `json:"enabled"`
	Headers  map[string]string `json:"headers"`
}

func (s *Server) handleUpsertAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	// Partial updates are merged over the stored entry: a caller that only
	// sends {id, enabled} must not wipe the upstream, proxy or headers.
	// Rule of thumb: an absent or empty field keeps the old value, because a
	// wiped upstream ("") would route the account straight into a 500 and a
	// wiped proxy into unintended exits. (APIKey keeps the registry's own rule:
	// empty keeps the stored key.)
	prev, hasPrev := s.reg.Get(req.ID)
	upstream := req.Upstream
	if upstream == "" {
		if hasPrev {
			upstream = prev.Upstream
		} else {
			upstream = "zen"
		}
	}
	if _, ok := s.cfg.Upstreams[upstream]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown upstream: " + upstream})
		return
	}
	// An empty pool keeps the stored value, exactly like upstream and proxy.
	// Clearing it would silently move the account into the upstream-named pool
	// and change which gateway keys can reach it.
	poolID := req.Pool
	if poolID == "" && hasPrev {
		poolID = prev.Pool
	}
	proxy := req.Proxy
	if proxy == "" {
		if hasPrev {
			proxy = prev.Proxy
		} else {
			proxy = "auto"
		}
	}
	name := req.Name
	if name == "" && hasPrev {
		name = prev.Name
	}
	weight := 0
	if hasPrev {
		weight = prev.Weight
	}
	if req.Weight != nil {
		weight = *req.Weight
	}
	headers := req.Headers
	if headers == nil && hasPrev {
		headers = prev.Headers
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else if hasPrev {
		enabled = prev.Enabled
	}
	acc := model.Account{
		ID:       req.ID,
		Name:     name,
		Upstream: upstream,
		Pool:     poolID,
		APIKey:   req.APIKey,
		Proxy:    proxy,
		Weight:   weight,
		Enabled:  enabled,
		Headers:  headers,
	}
	if err := s.reg.Upsert(acc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.reg.Save(); err != nil {
		s.log.Error("state save failed after account upsert", "err", err)
	}
	v, _ := s.reg.View(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"account": v})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.reg.Delete(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown account: " + id})
		return
	}
	if err := s.reg.Save(); err != nil {
		s.log.Error("state save failed after delete", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleRotateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ent, ok := s.reg.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown account: " + id})
		return
	}
	prev, _ := s.reg.View(id)
	px := s.reg.Rotate(id, prev.BoundExit)
	if px == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no exit available to rotate to"})
		return
	}
	s.log.Info("manual rotation", "account", ent.ID, "exit", px.ID)
	v, _ := s.reg.View(id)
	writeJSON(w, http.StatusOK, map[string]any{"account": v})
}

func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"exits": s.pool.Health()})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.pool.ProbeAll(ctx)
		s.log.Info("manual probe round finished")
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "probing"})
}

// handlePrune retires every exit that is currently unusable. Idempotent: a
// second call retires nothing and reports the same pool state. While the
// outage breaker is open the pass refuses unless force=1 — a correlated
// provider-wide outage must not be executed pool-wide by one button.
func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	force := r.URL.Query().Get("force") == "1"
	rep := s.pool.Prune(reason, force)
	writeJSON(w, http.StatusOK, map[string]any{
		"report": rep,
		"exits":  s.pool.Health(),
	})
}

func (s *Server) handleRetireExit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.pool.Retire(id, "retired manually from the console"); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retired": id})
}

func (s *Server) handleReviveExit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.pool.Revive(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revived": id})
}

// handleUncoolExit ends a quota cooldown early.
//
// Nothing in the request path calls this: cooldowns expire on their own, and
// that is deliberate, because the upstream stated the deadline and second-
// guessing it would put traffic straight back into a refusal. But an operator
// who has just topped an account up, or who is looking at a Retry-After they
// believe was wrong, has no other way to say so — and without one they would
// wait out a cooldown they know to be stale.
func (s *Server) handleUncoolExit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, _, cool := s.pool.QuotaCooled(id); !cool {
		// Distinguish "not cooled" from "no such exit": the first is a no-op the
		// operator should be told about, the second is a typo.
		if _, known := s.pool.Get(id); !known {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown exit " + id})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"uncooled": id, "changed": false})
		return
	}
	s.pool.ClearQuotaCool(id)
	s.log.Info("quota cooldown cleared by hand", "exit", id)
	writeJSON(w, http.StatusOK, map[string]any{"uncooled": id, "changed": true})
}

// handleClearRegion forgets every region block on one exit.
//
// A region block has no deadline to wait out, so unlike a cooldown it can only
// end two ways: its six-hour trust window running out, or an operator saying a
// country has reopened. Nothing in the request path can observe that, so without
// this endpoint the only remedy would be restarting the process.
//
// It clears the exit rather than one model because the operator's evidence —
// "this model works from here now" — is almost always about one pair, but a
// country reopening lifts every model at once, and asking them to name each
// model would make the common case the tedious one.
func (s *Server) handleClearRegion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, known := s.pool.Get(id); !known {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown exit " + id})
		return
	}
	n := s.pool.ClearRegionBlocks(id)
	s.log.Info("region blocks cleared by hand", "exit", id, "cleared", n)
	writeJSON(w, http.StatusOK, map[string]any{"exit": id, "cleared": n, "changed": n > 0})
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/" && r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(dashboardHTML))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
