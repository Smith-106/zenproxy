// Package forward is the proxy core: it resolves an account to an upstream
// request, sends it through that account's currently bound egress exit, and
// rotates the exit when the failure is one an exit can fix.
package forward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
//
// next takes the model because a region refusal belongs to the exit-and-model
// pair rather than to the exit. Without it a rotation triggered by one would
// pick blind and just as likely land on another exit refused for the same model,
// spending an attempt to be told the same thing.
type chooser interface {
	first() (candidate, bool)
	// sessionID is the CPA-style conversation key for this request; empty
	// disables session→exit rebinding on rotate.
	next(failed *registry.Entry, class model.ErrorClass, exitID, mdl, sessionID string) (candidate, bool)
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
func (f *fixedChooser) next(_ *registry.Entry, _ model.ErrorClass, exitID, mdl, sessionID string) (candidate, bool) {
	f.eng.rotateExitFor(f.ent.ID, sessionID, exitID, mdl)
	return candidate{f.ent, f.up, f.target}, true
}

// rotateExit moves an account onto a different egress IP and says so in the
// log. Split out so both choosers describe the same event identically.
func (e *Engine) rotateExit(accountID, exitID string) {
	e.rotateExitFor(accountID, "", exitID, "")
}

// rotateExitFor is rotateExit with the model, so the replacement can avoid exits
// this model is refused from. sessionID, when set, also rebinds the CPA-style
// session→exit sticky entry.
func (e *Engine) rotateExitFor(accountID, sessionID, exitID, mdl string) {
	if exitID != "" {
		// The exit worked — it carried the request and brought back an answer.
		// Only the binding moves, so an account-level quota cannot drain the IP
		// pool.
		e.pool.MarkSoftFailure(exitID, "upstream refused after a successful exchange")
	}
	e.reg.RotateForSession(accountID, sessionID, exitID, mdl)
}

func (e *Engine) proxyWithRotation(w http.ResponseWriter, r *http.Request, ent *registry.Entry, up model.Upstream, target *url.URL) {
	body, truncated, err := readBody(r, e.pol.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	// The reverse ingress has no account to choose between, so its ledger
	// applies to the exit: if this account is parked on one that cannot serve
	// this request, move it now instead of paying for the refusal and moving
	// after. The model comes out of the body, which is already in hand here and
	// goes upstream unchanged in this mode.
	askedModel := ""
	if !truncated {
		askedModel = modelField(body)
	}
	e.avoidSpentExit(ent.ID, askedModel)
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

	// Some models refuse /chat/completions and only answer on /responses.
	// sub2api's opencode_go/zen mode always forwards via raw chat completions
	// (even for its own /v1/responses clients), so without a rewrite those
	// models are unreachable. Convert once here; free-tier prepare then sees a
	// Responses path/body and builds the matching envelope.
	viaChatToResponses := false
	clientWantedStream := false
	preparePath := cur.target.Path
	if !truncated && surfaceFor(cur.target.Path) == surfaceChat && needsChatViaResponses(modelField(body)) {
		converted, clientStream, convErr := chatToResponsesBody(body)
		if convErr == nil {
			body = converted
			clientWantedStream = clientStream
			preparePath = rewritePathChatToResponses(cur.target.Path)
			viaChatToResponses = true
			e.log.Info("rewriting chat/completions to responses for protocol-limited model",
				"model", modelField(converted), "path", preparePath)
		} else {
			e.log.Warn("chat→responses rewrite skipped", "err", convErr)
		}
	}

	// The free tier is decided by the request envelope, not by the exit, so the
	// rewrite happens once, before the first attempt, and every retry reuses it.
	// An oversized body that had to be streamed through unreplayed cannot be
	// rewritten at all, because it never lands in memory as a whole.
	var plan *GatePlan
	// Real free-tier enveloping (as opposed to a synthesized chat→responses
	// rewrite, which is not a gate matter). Only envelope-gated requests may
	// fail fast on a fully-blocked pool: a paid model or an unenveloped
	// request must keep the ordinary attempt path.
	gated := false
	if !truncated {
		plan = e.free.Prepare(preparePath, body, false)
		gated = plan != nil
		if viaChatToResponses {
			if plan == nil {
				plan = &GatePlan{
					Body:        body,
					Model:       modelField(body),
					Template:    "chat-via-responses",
					Stream:      clientWantedStream,
					Surface:     surfaceResponses,
					ReplyAsChat: true,
				}
			} else {
				plan.ReplyAsChat = true
			}
		}
	}

	// Which model this request is for decides which exits the upstream will
	// serve it from, so it is read once here and threaded through every
	// resolution and rotation below. Empty means "could not tell": an oversized
	// body that was never buffered, or a shape with no model field. Everything
	// downstream treats that as "cannot judge" rather than as a wildcard.
	askedModel := ""
	if !truncated {
		askedModel = modelField(body)
	}
	if plan != nil && plan.Model != "" {
		askedModel = plan.Model
	}
	// When every healthy exit is already unusable for this model, rotating
	// maxAttempts just burns minutes. Quota-cooled pools fail fast with 429 +
	// Retry-After. Free-tier-blocked pools fail with 403 instead: a 429 here
	// was tripping session-sticky's stampede circuit even though no per-IP
	// quota clock was involved.
	if gated && e.pool != nil && !e.pool.HasUncooledExit(askedModel) {
		cools := e.pool.QuotaCooledIDs()
		if len(cools) > 0 {
			until := cools[0].Until
			secs := int(time.Until(until).Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			msg := fmt.Sprintf("Upstream rate limit exceeded, please retry later (all exits cooled until %s)", until.UTC().Format(time.RFC3339))
			e.log.Warn("refusing request: every healthy exit is quota-cooled",
				"model", askedModel, "cooled", len(cools), "until", until.UTC().Format(time.RFC3339),
				"after", cools[0].After)
			writeJSONError(w, http.StatusTooManyRequests, msg)
			return
		}
		e.log.Warn("refusing request: no free-tier-eligible exit available", "model", askedModel)
		writeJSONError(w, http.StatusForbidden, "OpenCode free tier: no eligible egress exit right now; retry shortly")
		return
	}
	// CPA-style explicit session identity. Prefer headers (session-sticky and
	// OpenCode clients already stamp them); fall back to prompt_cache_key in
	// the body so a body-only sticky seed still pins the exit.
	sessionID := inboundSessionID(r.Header, body)

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
	// sameExitTransientRetry: one free retry on the bound exit after a
	// transient transport blip (EOF / reset). Rotating on the first blip
	// drops upstream prompt-cache affinity for the rest of the conversation;
	// a second failure still rotates. Certificate poison never uses this path.
	sameExitTransientRetry := false
	// Requests are counted per account actually used, so a pool that had to
	// switch accounts does not credit the one that failed.
	counted := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ent := cur.ent
		if counted != ent.ID {
			e.reg.BeginRequest(ent.ID)
			counted = ent.ID
		}
		up := cur.up
		targetCopy := *cur.target
		if viaChatToResponses {
			targetCopy.Path = rewritePathChatToResponses(targetCopy.Path)
		}
		target := &targetCopy
		exitID := ""
		proxy := e.reg.ExitForSession(ent.ID, sessionID, askedModel)
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
		resp, err := e.clientFor(proxy, plan != nil).Do(req)
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
				// Sticky exit: one transient blip keeps the binding so a long
				// conversation does not lose upstream cached_tokens on noise.
				// Poison TLS / hard proxy failures rotate immediately.
				if exitID != "" && !sameExitTransientRetry && isTransientTransport(err) {
					sameExitTransientRetry = true
					e.log.Info("retrying same exit after transient transport blip",
						"account", ent.ID, "exit", exitID, "attempt", attempt)
					continue
				}
				e.reg.RotateForSession(ent.ID, sessionID, exitID, askedModel)
				continue
			}
			break
		}
		if exitID != "" {
			e.pool.MarkSuccess(exitID, time.Since(start))
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			e.reg.Record(ent.ID, model.ClassOK, nil)
			if exitID != "" {
				e.pool.ClearFreeTier(exitID)
			}
			if plan != nil && (!plan.Stream || plan.ReplyAsChat) {
				// The client asked for a single response and the free tier only
				// answers streams, so the stream is folded back here.
				// ReplyAsChat also folds: even a streaming chat client gets a
				// synthetic chat SSE built from the folded Responses object,
				// because translating every Responses event live is a larger
				// contract than this bridge needs.
				e.writeFolded(w, resp, plan)
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

		// An upstream that names a time at which it will serve this exit again
		// has told us something no amount of retrying will change. Recording it
		// here — before the rotation decision, and against the exit that
		// actually carried the request — is what turns "rotate and find out" into
		// "do not send that one next time".
		if until, cooled := e.coolExit(exitID, resp.Header, class); cooled {
			e.log.Info("exit is out of quota until the upstream's stated time",
				"account", ent.ID, "exit", exitID, "status", resp.StatusCode, "class", class,
				"until", until.Format(time.RFC3339),
				"after", time.Until(until).Round(time.Second).String())
		}
		// A region refusal is recorded against the exit AND the model, because
		// that pair is what the upstream actually refuses. Measured: the same
		// exit answered 200 for another model in the same second, so recording
		// it against the exit alone would take a working IP out of rotation for
		// every model because one of them is unavailable there.
		if class == model.ClassRegion && exitID != "" && askedModel != "" {
			if e.pool.BlockRegion(exitID, askedModel) {
				e.log.Info("exit's country is refused for this model; remembering the pair",
					"account", ent.ID, "exit", exitID, "model", askedModel,
					"status", resp.StatusCode, "class", class,
					"recheck_after", e.pool.RegionBlockTTL().String())
			}
		}

		if class == model.ClassTierGate {
			// A model that is free but missing from the configured list is
			// discovered here, on its first refusal. The retry is free: the
			// attempt counter is rewound so that a tier gate does not eat the
			// rotation budget that transport failures need.
			if !gateRetried {
				gateRetried = true
				if p := e.free.Prepare(target.Path, body, true); p != nil {
					e.free.Learn(p.Model)
					if viaChatToResponses {
						p.ReplyAsChat = true
					}
					plan = p
					e.log.Info("upstream refused this client for a free model; retrying inside the OpenCode envelope",
						"account", ent.ID, "model", p.Model, "template", p.Template, "stream_folded", !p.Stream)
					cancel()
					attempt--
					continue
				}
			}
			e.log.Warn("upstream refused this free-tier request after the OpenCode envelope",
				"account", ent.ID, "exit", exitID, "status", resp.StatusCode,
				"enveloped", plan != nil, "hint", "see docs/FREE-TIER.md")
			// Park on the free-tier axis (probe-proof) so rotation skips known-bad
			// free proxies instead of rediscovering the same FreeTierError.
			if plan != nil && exitID != "" {
				e.pool.BlockFreeTier(exitID, 15*time.Minute, "enveloped FreeTierError")
			}
		}
		if attempt < maxAttempts && e.shouldRotate(resp.StatusCode, class) {
			e.log.Info("rotating egress after upstream error",
				"account", ent.ID, "exit", exitID, "status", resp.StatusCode, "class", class)
			// The chooser owns what "switch" means here: another IP for the same
			// account, or another account entirely.
			nextCand, ok := ch.next(ent, class, exitID, askedModel, sessionID)
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
// freeAttemptHeaderTimeout caps how long an enveloped free-tier attempt may
// take to produce response headers. The envelope always streams upstream, so
// a live slot answers in well under a second and a stuck one is dead weight;
// without this a single hang ran into the 3600s request ctx (measured 773s).
// Paid/non-stream traffic is untouched: those responses legitimately delay
// headers until generation finishes.
const freeAttemptHeaderTimeout = 30 * time.Second

func (e *Engine) clientFor(proxy *proxypool.Proxy, free bool) *http.Client {
	t := e.direct
	if proxy != nil {
		t = proxy.Transport()
	}
	if free {
		// Clone (never mutate the shared per-exit transport) so the cap dies
		// with this request.
		tc := t.Clone()
		tc.ResponseHeaderTimeout = freeAttemptHeaderTimeout
		t = tc
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
	// header is a deliberate decision and still wins. Inbound session /
	// conversation headers have already been copied above; ApplyHeaders reuses
	// them so the upstream keeps prompt-cache identity across turns.
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
//
// For ReplyAsChat+stream clients, SSE comment keepalives are flushed while the
// upstream Responses stream is still being folded. Without them, muse xhigh
// turns stay silent for minutes and the client (X-Stainless-Timeout / OpenCode)
// aborts with HTTP 499 long before any chat chunk arrives.
func (e *Engine) writeFolded(w http.ResponseWriter, resp *http.Response, plan *GatePlan) {
	wantChatStream := plan != nil && plan.ReplyAsChat && plan.Stream
	var (
		flusher http.Flusher
		hdrSent bool
	)
	if wantChatStream {
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Del("Content-Length")
		w.Header().Del("Transfer-Encoding")
		w.Header().Del("Content-Encoding")
		w.WriteHeader(http.StatusOK)
		hdrSent = true
		if flusher != nil {
			_, _ = w.Write([]byte(": connected\n\n"))
			flusher.Flush()
		}
	}

	raw, err := readAllKeepalive(resp.Body, e.pol.MaxBodyBytes, 15*time.Second, func() {
		if !hdrSent || flusher == nil {
			return
		}
		if _, werr := w.Write([]byte(": keepalive\n\n")); werr == nil {
			flusher.Flush()
		}
	})
	resp.Body.Close()
	if err != nil {
		if hdrSent {
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"failed to read the upstream stream\",\"type\":\"api_error\"}}\n\ndata: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		writeJSONError(w, http.StatusBadGateway, "failed to read the upstream stream: "+err.Error())
		return
	}
	s := surfaceChat
	if plan != nil {
		s = plan.Surface
	}
	out, foldErr := foldStream(raw)
	if s == surfaceResponses {
		out, foldErr = foldResponsesStream(raw)
	}
	if foldErr != "" {
		// A fold that could not produce an answer is an upstream failure, not a
		// reply. Reporting it only in the body would leave the 200 intact, and
		// anything relaying this proxy forwards the status verbatim — the client
		// would believe it succeeded and then find no choices to read.
		if hdrSent {
			payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": foldErr, "type": "api_error"}})
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(payload)
			_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		writeJSONError(w, http.StatusBadGateway, foldErr)
		return
	}
	if plan != nil && plan.ReplyAsChat {
		chatJSON, convErr := responsesToChatCompletion(out)
		if convErr != nil {
			if hdrSent {
				payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "failed to convert responses reply to chat: " + convErr.Error(), "type": "api_error"}})
				_, _ = w.Write([]byte("data: "))
				_, _ = w.Write(payload)
				_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			writeJSONError(w, http.StatusBadGateway, "failed to convert responses reply to chat: "+convErr.Error())
			return
		}
		if plan.Stream {
			sse, sseErr := chatCompletionToSSE(chatJSON)
			if sseErr != nil {
				if hdrSent {
					payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": "failed to stream converted chat reply: " + sseErr.Error(), "type": "api_error"}})
					_, _ = w.Write([]byte("data: "))
					_, _ = w.Write(payload)
					_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
				writeJSONError(w, http.StatusBadGateway, "failed to stream converted chat reply: "+sseErr.Error())
				return
			}
			_, _ = w.Write(sse)
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		out = chatJSON
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.Header().Del("Transfer-Encoding")
	w.Header().Del("Content-Encoding")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// readAllKeepalive copies r up to limit bytes while invoking onTick on a
// wall-clock ticker. Used to keep reverse proxies / SSE clients alive while a
// Responses stream is still being folded into one chat reply — muse xhigh can
// sit silent for minutes before the first chat chunk would otherwise appear.
func readAllKeepalive(r io.Reader, limit int64, every time.Duration, onTick func()) ([]byte, error) {
	if limit <= 0 {
		limit = 32 << 20
	}
	done := make(chan struct{})
	defer close(done)
	if onTick != nil {
		if every <= 0 {
			every = 15 * time.Second
		}
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					onTick()
				}
			}
		}()
	}
	return io.ReadAll(io.LimitReader(r, limit))
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
		// A region refusal must be recognised before the generic 403 branch for
		// the same reason the tier gate is: the operator would otherwise see
		// "auth", go looking for a bad key, and never learn that another exit
		// would have served the request.
		if bytes.Contains(body, []byte("RegionError")) || bytes.Contains(body, []byte("not available in your country")) {
			return model.ClassRegion
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

// isTransientTransport reports a one-shot transport blip that is worth
// retrying on the same exit. Certificate poison and SOCKS "general failure"
// are not transient: the exit is the problem and affinity should move now.
func isTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if proxypool.PoisonTLS(msg) {
		return false
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "socks5: general failure") {
		return false
	}
	if strings.Contains(lower, "socks5: host unreachable") {
		return false
	}
	switch {
	case strings.Contains(lower, "unexpected eof"):
		return true
	case strings.Contains(lower, "connection reset"):
		return true
	case strings.Contains(lower, "broken pipe"):
		return true
	case strings.Contains(lower, "use of closed network connection"):
		return true
	case strings.Contains(lower, "i/o timeout"):
		return true
	default:
		return false
	}
}

// quotaDeadline reads the upstream's own statement of when it will serve this
// exit again, in preference to any guess the proxy could make.
//
// This is the whole reason a ledger is worth having. An exit that is out of
// quota looks exactly like a healthy one from the outside: it connects, it
// answers, it is fast. The only thing distinguishing it is a header on the
// refusal, and Retry-After names a fixed instant rather than a fresh countdown
// per request — measured across a whole pool, the readings counted down
// together toward one reset. So the deadline is stored absolute, and every exit
// that reports it agrees about when the allowance returns.
//
// Both Retry-After encodings are accepted. The delta form is what this upstream
// sends; the HTTP-date form is what the RFC prefers and what other upstreams
// send, and silently ignoring it would leave those upstreams rotating blind.
//
// A missing or unparseable header is not an error: it just means this refusal
// carried no timing information, and reactive rotation — which existed before
// the ledger and still works — remains the fallback.
func quotaDeadline(h http.Header, now time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return time.Time{}, false
	}
	// Delta-seconds. The RFC allows only a non-negative integer here, so a
	// float is not a duration to be generous about — it is a different header
	// that happened to land in this slot, and parsing it as 1.5s would cool an
	// exit for a duration nobody asked for.
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return time.Time{}, false
		}
		return now.Add(time.Duration(secs) * time.Second), true
	}
	// HTTP-date, in any of the three formats the RFC requires parsers to accept.
	for _, layout := range []string{http.TimeFormat, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// onSpentExit reports whether an account is currently parked on an exit that
// cannot serve a request for this model, for either reason.
//
// Both reasons are checked because both are invisible to health: a quota-spent
// exit answers promptly, and a region-refused exit answers promptly from a
// country that is merely the wrong one. Choosing by strategy alone therefore
// lands on either with the same disregard.
//
// It reads the binding rather than resolving one, so it is a question and not a
// decision. It runs once per account in the pool while picking one, and
// resolving there would bind as a side effect and answer about the wrong exit.
func (e *Engine) onSpentExit(accountID, mdl string) bool {
	if e.pool == nil {
		return false
	}
	exitID := e.reg.BoundExit(accountID)
	if exitID == "" {
		return false
	}
	if _, _, cool := e.pool.QuotaCooled(exitID); cool {
		return true
	}
	return e.pool.RegionBlocked(exitID, mdl)
}

// coolExit records an upstream's refusal against the exit that carried it.
//
// The exit is passed in rather than resolved here, and that is deliberate. This
// runs after a response, by which point re-resolving could return a different
// exit than the one that was actually used — model-aware resolution in
// particular will skip an exit refused for this model, so resolving here would
// cool a bystander instead of the culprit.
//
// A 429 is not evidence that the account, the key or the model is at fault —
// measured, the same account on the same key succeeds from a different exit
// within the same second.
func (e *Engine) coolExit(exitID string, h http.Header, class model.ErrorClass) (time.Time, bool) {
	if class != model.ClassRateLimit || e.pool == nil || exitID == "" {
		return time.Time{}, false
	}
	until, ok := quotaDeadline(h, time.Now())
	if !ok {
		return time.Time{}, false
	}
	if !e.pool.CoolForQuota(exitID, until, string(class)) {
		return time.Time{}, false
	}
	return until, true
}

// avoidCooledExit moves an account off an exit that is out of quota, before the
// request is sent rather than after it is refused.
//
// This is what a ledger buys over pure reaction. Without it the pool learns the
// same fact once per account per request: send, get refused, rotate. With it,
// the refusal is paid for once and every later request starts on an exit that
// can actually serve.
//
// It declines to move when there is nothing warmer to move to. Rotating into
// another cooled exit would churn the binding, spend a rotation, and change
// nothing about the outcome.
func (e *Engine) avoidSpentExit(accountID, mdl string) {
	if e.pool == nil {
		return
	}
	px := e.reg.ExitForModel(accountID, mdl)
	if px == nil {
		return
	}
	until, quotaReason, cooled := e.pool.QuotaCooled(px.ID)
	if !cooled && !e.pool.RegionBlocked(px.ID, mdl) {
		return
	}
	next := e.pool.PickFor(mdl, px.ID)
	if next == nil {
		return
	}
	if cooled {
		if _, _, nextCooled := e.pool.QuotaCooled(next.ID); nextCooled {
			// Everything available is out of quota too. Staying put is the
			// honest move: the request is going to be refused wherever it goes,
			// and the exit we are on at least knows when its allowance returns.
			e.log.Warn("every candidate exit is out of quota; the request will most likely be refused",
				"account", accountID, "exit", px.ID,
				"until", until.Format(time.RFC3339), "reason", quotaReason)
			return
		}
		e.log.Info("moving off an exit whose quota is spent, before sending",
			"account", accountID, "from", px.ID, "to", next.ID,
			"until", until.Format(time.RFC3339), "reason", quotaReason)
	} else {
		if e.pool.RegionBlocked(next.ID, mdl) {
			// Nothing warmer exists. Unlike the quota case there is no stated
			// end to wait for, so the honest report is that the model is
			// unavailable from everywhere this pool currently reaches.
			e.log.Warn("every candidate exit is refused for this model; the request will most likely be refused",
				"account", accountID, "exit", px.ID, "model", mdl)
			return
		}
		e.log.Info("moving off an exit whose country is refused for this model, before sending",
			"account", accountID, "from", px.ID, "to", next.ID, "model", mdl)
	}
	e.reg.RotateFor(accountID, px.ID, mdl)
}

// pickAccount chooses an account for a gateway request, preferring one that is
// not parked on an exit whose quota is spent.
//
// The reverse ingress cannot do this — the caller named the account, so the
// only lever is that account's exit. The gateway can, and it is the difference
// between a pool that degrades and one that keeps working: with four usable
// exits out of twenty-four, choosing by strategy alone means most requests land
// on a spent exit and are refused before the rotation loop even starts.
//
// When every account in the pool is out of quota it returns the strategy's own
// choice rather than nothing. The request is going to be refused wherever it
// goes; failing it outright would be strictly worse, and the refusal is what
// tells the caller when the pool comes back.
func (e *Engine) pickAccount(pool, strategy string, exclude map[string]bool, mdl string) *registry.Entry {
	ent := e.reg.PickInPool(pool, strategy, exclude)
	if ent == nil || e.pool == nil || !e.onSpentExit(ent.ID, mdl) {
		return ent
	}
	skip := make(map[string]bool, len(exclude))
	for k, v := range exclude {
		skip[k] = v
	}
	warm := false
	for _, m := range e.reg.PoolMembers(pool) {
		if skip[m.ID] {
			continue
		}
		if e.onSpentExit(m.ID, mdl) {
			skip[m.ID] = true
			continue
		}
		warm = true
	}
	if !warm {
		return ent
	}
	if better := e.reg.PickInPool(pool, strategy, skip); better != nil {
		return better
	}
	return ent
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// inboundSessionID extracts the CPA-style explicit conversation key from the
// request. Headers win; prompt_cache_key in the JSON body is the fallback so a
// body-only sticky seed still pins the exit. Empty means "no session affinity".
func inboundSessionID(h http.Header, body []byte) string {
	for _, name := range inboundSessionHeaderNames {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	if len(body) == 0 {
		return ""
	}
	// Cheap scan: avoid a full decode when the key is absent.
	const needle = `"prompt_cache_key"`
	i := bytes.Index(body, []byte(needle))
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	// skip : and whitespace / quotes
	j := 0
	for j < len(rest) && (rest[j] == ':' || rest[j] == ' ' || rest[j] == '\t' || rest[j] == '\n' || rest[j] == '\r') {
		j++
	}
	if j >= len(rest) || rest[j] != '"' {
		return ""
	}
	j++
	start := j
	for j < len(rest) && rest[j] != '"' {
		if rest[j] == '\\' && j+1 < len(rest) {
			j += 2
			continue
		}
		j++
	}
	if j > start {
		return string(rest[start:j])
	}
	return ""
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
