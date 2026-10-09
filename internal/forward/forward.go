// Package forward is the proxy core: it resolves an account to an upstream
// request, sends it through that account's currently bound egress exit, and
// rotates the exit when the failure is one an exit can fix.
package forward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/gateway"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// hopHeaders must never be forwarded in either direction.
var hopHeaders = map[string]bool{
	"connection":          true,
	"proxy-connection":    true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// Engine implements both ingress shapes over one account registry.
type Engine struct {
	reg    *registry.Registry
	pool   *proxypool.Pool
	ups    map[string]model.Upstream
	pol    config.Policy
	allow  map[string]bool // allowed CONNECT/forward targets; empty means "no allowlist"
	log    *slog.Logger
	direct *http.Transport
	free   *FreeTier

	// gw resolves the unified /v1 ingress. poolPolicy looks up a pool's
	// rotation policy; it is a func rather than the config slice so that the
	// engine does not hold the whole config (and its credentials) alive.
	gw         *gateway.Gateway
	poolPolicy func(string) config.PoolPolicy
}

// Options carries the engine's static configuration.
type Options struct {
	Upstreams    map[string]model.Upstream
	Policy       config.Policy
	AllowedHosts []string
	FreeTier     config.FreeTier
	Log          *slog.Logger
	// Gateway, Pools and ModelList configure the unified /v1 ingress. Leaving
	// them zero keeps the daemon exactly as it was: address one account by path.
	Gateway   config.Gateway
	Pools     []config.PoolPolicy
	ModelList []config.ModelRoute
}

// New builds the engine.
func New(reg *registry.Registry, pool *proxypool.Pool, opts Options) *Engine {
	policies := make(map[string]config.PoolPolicy, len(opts.Pools))
	for _, p := range opts.Pools {
		policies[p.ID] = p
	}
	e := &Engine{
		reg:  reg,
		pool: pool,
		ups:  opts.Upstreams,
		pol:  opts.Policy,
		log:  opts.Log,
		free: NewFreeTier(opts.FreeTier),
		gw:   gateway.New(opts.Gateway, opts.ModelList),
		poolPolicy: func(id string) config.PoolPolicy {
			if p, ok := policies[id]; ok {
				if p.Strategy == "" {
					p.Strategy = config.StrategyRR
				}
				if p.OnRateLimit == "" {
					p.OnRateLimit = config.RotateBoth
				}
				return p
			}
			return config.PoolPolicy{ID: id, Strategy: config.StrategyRR, OnRateLimit: config.RotateBoth}
		},
		direct: &http.Transport{
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			DisableCompression:    true,
			ExpectContinueTimeout: 2 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
	if len(opts.AllowedHosts) > 0 {
		e.allow = make(map[string]bool, len(opts.AllowedHosts)+2)
		for _, h := range opts.AllowedHosts {
			e.allow[strings.ToLower(strings.TrimSpace(h))] = true
		}
		// The reverse-mode upstreams are implicitly allowed too, so a single
		// allowlist governs both ingress shapes.
		for _, up := range opts.Upstreams {
			if u, err := url.Parse(up.BaseURL); err == nil {
				e.allow[strings.ToLower(u.Hostname())] = true
			}
		}
	}
	return e
}

// ---------------------------------------------------------------------------
// reverse mode: BASE URL = http://zenproxy:9000/<account-id>
// ---------------------------------------------------------------------------

// ServeReverse proxies /<account>/<rest...> to that account's upstream.
func (e *Engine) ServeReverse(w http.ResponseWriter, r *http.Request, accountID, rest string) {
	ent, ok := e.reg.Get(accountID)
	if !ok || !ent.Enabled {
		writeJSONError(w, http.StatusNotFound, "unknown or disabled account: "+accountID)
		return
	}
	up, ok := e.ups[ent.Upstream]
	if !ok {
		writeJSONError(w, http.StatusInternalServerError,
			fmt.Sprintf("account %s references unknown upstream %s", ent.ID, ent.Upstream))
		return
	}
	target, err := joinUpstreamPath(up.BaseURL, rest)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	target.RawQuery = r.URL.RawQuery
	e.proxyWithRotation(w, r, ent, up, target)
}

// joinUpstreamPath appends rest to base, collapsing a duplicated /v1 so that
// both client styles work against the same upstream:
//
//	base https://opencode.ai/zen/v1 + /chat/completions     -> .../zen/v1/chat/completions
//	base https://opencode.ai/zen/v1 + /v1/chat/completions  -> .../zen/v1/chat/completions
func joinUpstreamPath(base, rest string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("bad upstream base_url %q: %w", base, err)
	}
	if rest == "" {
		u.Path = strings.TrimRight(u.Path, "/")
		return u, nil
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/v1") && strings.HasPrefix(rest, "/v1/") {
		rest = strings.TrimPrefix(rest, "/v1")
	}
	u.Path = path + rest
	return u, nil
}

// candidate is one thing worth attempting: an account, the upstream it talks
// to, and where that request goes.
type candidate struct {
	ent    *registry.Entry
	up     model.Upstream
	target *url.URL
}

// chooser decides which account each attempt uses.
//
// The two ingresses want opposite things from it, and that is exactly why it is
// an interface. Reverse mode encodes the account in the URL, so every attempt
// must use that one account and only its egress IP may move. The gateway mode
// has no account in the URL, so "switch" can mean "try a different account" —
// which is the only way to survive an upstream that has spent the credential
// rather than the IP.
//
// budget is the attempt ceiling for this request. It is the chooser's because
// only the chooser knows how many distinct things there are to try.
type chooser interface {
	first() (candidate, bool)
	next(failed *registry.Entry, class model.ErrorClass, exitID string) (candidate, bool)
	budget() int
}

// fixedChooser always answers with the same account: the reverse ingress.
type fixedChooser struct {
	eng     *Engine
	ent     *registry.Entry
	up      model.Upstream
	target  *url.URL
	limit   int
	started bool
}

func (f *fixedChooser) budget() int { return f.limit }

func (f *fixedChooser) first() (candidate, bool) {
	f.started = true
	return candidate{f.ent, f.up, f.target}, true
}

// next keeps the account and moves only its exit, which is precisely what the
// reverse ingress could always do. Retrying the same account on a fresh IP is
// the whole remedy available when the caller named the account.
func (f *fixedChooser) next(_ *registry.Entry, _ model.ErrorClass, exitID string) (candidate, bool) {
	f.eng.rotateExit(f.ent.ID, exitID)
	return candidate{f.ent, f.up, f.target}, true
}

// rotateExit moves an account onto a different egress IP and says so in the
// log. Split out so both choosers describe the same event identically.
func (e *Engine) rotateExit(accountID, exitID string) {
	if exitID != "" {
		// The exit worked — it carried the request and brought back an answer.
		// Only the binding moves, so an account-level quota cannot drain the IP
		// pool.
		e.pool.MarkSoftFailure(exitID, "upstream refused after a successful exchange")
	}
	e.reg.Rotate(accountID, exitID)
}

func (e *Engine) proxyWithRotation(w http.ResponseWriter, r *http.Request, ent *registry.Entry, up model.Upstream, target *url.URL) {
	body, truncated, err := readBody(r, e.pol.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	e.proxyLoop(w, r, &fixedChooser{eng: e, ent: ent, up: up, target: target, limit: e.pol.MaxAttempts}, body, truncated)
}

// proxyLoop is the shared retry loop. The body arrives already read because the
// gateway ingress has to inspect it to resolve the model, and reading it twice
// is not possible on a streaming request.
func (e *Engine) proxyLoop(w http.ResponseWriter, r *http.Request, ch chooser, body []byte, truncated bool) {
	cur, ok := ch.first()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable,
			"no account is available to serve this request")
		return
	}

	// The free tier is decided by the request envelope, not by the exit, so the
	// rewrite happens once, before the first attempt, and every retry reuses it.
	// An oversized body that had to be streamed through unreplayed cannot be
	// rewritten at all, because it never lands in memory as a whole.
	var plan *GatePlan
	if !truncated {
		plan = e.free.Prepare(cur.target.Path, body, false)
	}

	maxAttempts := ch.budget()
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if truncated {
		// Too large to buffer for replay: forward the whole body once by
		// chaining the buffered prefix with the untouched remainder, and give
		// up the retry budget rather than corrupting the request.
		maxAttempts = 1
	}

	var lastMsg string
	gateRetried := false
	// Requests are counted per account actually used, so a pool that had to
	// switch accounts does not credit the one that failed.
	counted := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ent := cur.ent
		if counted != ent.ID {
			e.reg.BeginRequest(ent.ID)
			counted = ent.ID
		}
		up, target := cur.up, cur.target
		exitID := ""
		proxy := e.reg.ExitFor(ent.ID)
		if proxy != nil {
			exitID = proxy.ID
		}

		sent := body
		if plan != nil {
			sent = plan.Body
		}

		var reader io.Reader
		length := int64(len(sent))
		switch {
		case truncated:
			reader = io.MultiReader(bytes.NewReader(body), r.Body)
			length = -1
		case sent != nil:
			reader = bytes.NewReader(sent)
		}

		req, cancel, err := e.buildRequest(r, target.String(), reader, length, ent, up, plan)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		start := time.Now()
		resp, err := e.clientFor(proxy).Do(req)
		if err != nil {
			cancel()
			class := classifyTransport(err)
			e.reg.Record(ent.ID, class, err)
			lastMsg = err.Error()
			if exitID != "" {
				e.pool.MarkFailure(exitID, err.Error())
			}
			e.log.Warn("upstream transport failure",
				"account", ent.ID, "exit", exitID, "attempt", attempt, "class", class, "err", err)
			if attempt < maxAttempts {
				e.reg.Rotate(ent.ID, exitID)
				continue
			}
			break
		}
		if exitID != "" {
			e.pool.MarkSuccess(exitID, time.Since(start))
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e.reg.Record(ent.ID, model.ClassOK, nil)
			if plan != nil && !plan.Stream {
				// The client asked for a single response and the free tier only
				// answers streams, so the stream is folded back here.
				e.writeFolded(w, resp, plan.Surface)
				cancel()
				return
			}
			// An upstream that accepts a streaming request and then goes
			// silent would otherwise hold the connection until the hard
			// UpstreamTimeout. The idle guard is the finer-grained net.
			if e.pol.StreamIdleTimeout > 0 {
				resp.Body = newIdleGuard(resp.Body, e.pol.StreamIdleTimeout, cancel)
			}
			e.writeStream(w, resp)
			cancel()
			return
		}

		// Error responses are small; buffer them so the status can be
		// classified and the body can still be replayed to the client.
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		class := classifyStatus(resp.StatusCode, resp.Header, errBody)
		upstreamErr := fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(errBody, 300))
		e.reg.Record(ent.ID, class, upstreamErr)

		if class == model.ClassTierGate {
			// A model that is free but missing from the configured list is
			// discovered here, on its first refusal. The retry is free: the
			// attempt counter is rewound so that a tier gate does not eat the
			// rotation budget that transport failures need.
			if !gateRetried {
				gateRetried = true
				if p := e.free.Prepare(target.Path, body, true); p != nil {
					e.free.Learn(p.Model)
					plan = p
					e.log.Info("upstream refused this client for a free model; retrying inside the OpenCode envelope",
						"account", ent.ID, "model", p.Model, "template", p.Template, "stream_folded", !p.Stream)
					cancel()
					attempt--
					continue
				}
			}
			e.log.Warn("upstream refused this request on client identity and no exit change can alter that",
				"account", ent.ID, "exit", exitID, "status", resp.StatusCode,
				"enveloped", plan != nil, "hint", "see docs/FREE-TIER.md")
		}
		if attempt < maxAttempts && e.shouldRotate(resp.StatusCode, class) {
			e.log.Info("rotating egress after upstream error",
				"account", ent.ID, "exit", exitID, "status", resp.StatusCode, "class", class)
			// The chooser owns what "switch" means here: another IP for the same
			// account, or another account entirely.
			nextCand, ok := ch.next(ent, class, exitID)
			if !ok {
				writeUpstreamResponse(w, resp, errBody)
				cancel()
				return
			}
			cur = nextCand
			cancel()
			continue
		}
		writeUpstreamResponse(w, resp, errBody)
		cancel()
		return
	}

	e.reg.Record(cur.ent.ID, model.ClassTransport, errors.New(lastMsg))
	writeJSONError(w, http.StatusBadGateway,
		fmt.Sprintf("all %d attempt(s) failed for account %s: %s", maxAttempts, cur.ent.ID, lastMsg))
}

// FreeTier exposes the envelope writer for the dashboard, which reports which
// models were discovered by refusal.
func (e *Engine) FreeTier() *FreeTier { return e.free }
func (e *Engine) clientFor(proxy *proxypool.Proxy) *http.Client {
	t := e.direct
	if proxy != nil {
		t = proxy.Transport()
	}
	return &http.Client{
		Transport: t,
		// A response header must arrive within this window, but the body may
		// stream for as long as it likes.
		Timeout: 0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// buildRequest constructs the upstream request. The returned cancel func must
// be called once the response has been fully consumed, otherwise the timeout
// timer leaks.
func (e *Engine) buildRequest(src *http.Request, target string, body io.Reader, length int64, ent *registry.Entry, up model.Upstream, plan *GatePlan) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(src.Context(), e.pol.UpstreamTimeout)
	req, err := http.NewRequestWithContext(ctx, src.Method, target, body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	// A negative length means "unknown": let the transport use chunked
	// encoding instead of lying about the size.
	req.ContentLength = length

	for k, vals := range src.Header {
		lk := strings.ToLower(k)
		if hopHeaders[lk] || lk == "authorization" || lk == "x-api-key" || lk == "host" || lk == "content-length" {
			continue
		}
		// Accept-Encoding is dropped on purpose. Forwarding the caller's
		// "br, zstd" makes net/http treat the body as the caller's business and
		// hand it over compressed, which breaks both error classification and
		// stream folding — the 403 that carries the free-tier refusal arrives
		// brotli-encoded and reads as noise. Without the header the transport
		// negotiates gzip itself and decompresses transparently, so downstream
		// sees plain text either way.
		if lk == "accept-encoding" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+ent.APIKey)
	if src.Header.Get("x-api-key") != "" {
		req.Header.Set("x-api-key", ent.APIKey)
	}
	// The free tier identifies its client by these headers, so they are stamped
	// before the operator's own overrides: an explicit account or upstream
	// header is a deliberate decision and still wins.
	if plan != nil {
		e.free.ApplyHeaders(req.Header)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range ent.Headers {
		req.Header.Set(k, v)
	}
	return req, cancel, nil
}

func (e *Engine) shouldRotate(status int, class model.ErrorClass) bool {
	if !class.IPAttributable() {
		return false
	}
	if class == model.ClassTransport {
		return e.pol.RotateOnTransport
	}
	for _, s := range e.pol.RotateOnStatus {
		if s == status {
			return true
		}
	}
	return false
}

// idleGuard aborts a streamed body when no bytes arrive for d. It cancels the
// request context on expiry, which unblocks the pending Read.
type idleGuard struct {
	r      io.ReadCloser
	d      time.Duration
	cancel context.CancelFunc
	timer  *time.Timer
}

func newIdleGuard(r io.ReadCloser, d time.Duration, cancel context.CancelFunc) *idleGuard {
	g := &idleGuard{r: r, d: d, cancel: cancel}
	g.timer = time.AfterFunc(d, cancel)
	return g
}

func (g *idleGuard) Read(p []byte) (int, error) {
	g.timer.Reset(g.d)
	n, err := g.r.Read(p)
	if err != nil {
		g.timer.Stop()
	}
	return n, err
}

func (g *idleGuard) Close() error {
	g.timer.Stop()
	return g.r.Close()
}

// writeStream forwards a success response; SSE is flushed chunk by chunk.
func (e *Engine) writeStream(w http.ResponseWriter, resp *http.Response) {
	copyHeaders(w.Header(), resp.Header)
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	resp.Body.Close()
}

// writeFolded reads a streamed reply to the end and answers the client that
// asked for a single JSON object with one. The stream is folded rather than
// replayed, so Content-Length and the transfer encoding are recomputed.
//
// The fold is chosen by the surface that was enveloped: chat and Responses
// stream different events, and the wrong folder does not fail loudly, it
// returns an empty answer.
func (e *Engine) writeFolded(w http.ResponseWriter, resp *http.Response, s surface) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, e.pol.MaxBodyBytes))
	resp.Body.Close()
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "failed to read the upstream stream: "+err.Error())
		return
	}
	out := foldStream(raw)
	if s == surfaceResponses {
		out = foldResponsesStream(raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// writeUpstreamResponse replays a buffered error response verbatim.
func writeUpstreamResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readBody(r *http.Request, limit int64) ([]byte, bool, error) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil, false, nil
	}
	if limit <= 0 {
		limit = 32 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// classifyStatus buckets an upstream error. The order matters: the tier gate
// must be recognised before the generic 403 branch, otherwise the operator
// would see "auth" and go hunting for a bad key.
func classifyStatus(status int, header http.Header, body []byte) model.ErrorClass {
	switch {
	case status == http.StatusProxyAuthRequired:
		return model.ClassProxyAuth
	case status == http.StatusTooManyRequests:
		return model.ClassRateLimit
	case status >= 500:
		return model.ClassServer
	case status == http.StatusForbidden:
		if bytes.Contains(body, []byte("FreeTierError")) || bytes.Contains(body, []byte("only be used from within OpenCode")) {
			return model.ClassTierGate
		}
		if marker := header.Get("Cf-Mitigated"); marker != "" {
			return model.ClassServer
		}
		return model.ClassAuth
	case status == http.StatusUnauthorized:
		return model.ClassAuth
	case status >= 400 && status < 500:
		return model.ClassClient
	default:
		return model.ClassOK
	}
}

// classifyTransport decides whether a dial/TLS failure says something about
// the exit or about the request.
func classifyTransport(err error) model.ErrorClass {
	var nerr net.Error
	if errors.As(err, &nerr) {
		return model.ClassTransport
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return model.ClassCancelled
	}
	msg := err.Error()
	if strings.Contains(msg, "authentication required") || strings.Contains(msg, "407") {
		return model.ClassProxyAuth
	}
	return model.ClassTransport
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"type":"zenproxy_error","message":%q}}`, msg)
}

// WriteError emits the JSON error envelope shared by every ingress.
func WriteError(w http.ResponseWriter, status int, msg string) {
	writeJSONError(w, status, msg)
}
