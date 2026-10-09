// Package proxypool maintains the pool of egress exits.
//
// One pool member is one way out to the public internet: a Resin sticky slot,
// a static SOCKS5/HTTP proxy, or a direct connection. The pool owns liveness
// bookkeeping (probe, quarantine, backoff) and is the only component that
// knows how to physically dial through an exit.
package proxypool

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
)

// Proxy is one configured exit. It is safe to share: the underlying
// http.Transport pools connections per host.
type Proxy struct {
	ID       string
	Raw      string // never log this: it may carry credentials
	Scheme   string
	Addr     string
	Username string
	Password string

	url       *url.URL
	transport *http.Transport
}

// New parses a proxy URL. Supported schemes: http, https, socks5, socks5h.
func New(id, raw string) (*Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", id, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q (want http, https, socks5 or socks5h)", id, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: missing host", id)
	}
	p := &Proxy{
		ID:     id,
		Raw:    raw,
		Scheme: u.Scheme,
		Addr:   u.Host,
		url:    u,
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	p.transport = p.newTransport()
	return p, nil
}

func (p *Proxy) newTransport() *http.Transport {
	t := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		DisableCompression:    true,
		// Upstream responses can be long-lived SSE streams; never let the
		// transport close a stream body underneath us.
		ResponseHeaderTimeout: 0,
	}
	if p.Scheme == "http" || p.Scheme == "https" {
		// net/http handles CONNECT and Proxy-Authorization for us.
		t.Proxy = http.ProxyURL(p.url)
	} else {
		// SOCKS5 needs a custom dialer; there is no stdlib support.
		remote := p.Scheme == "socks5h"
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return p.dialSOCKS5(ctx, network, addr, remote)
		}
		t.ForceAttemptHTTP2 = true
	}
	return t
}

// Transport returns the shared transport for this exit.
func (p *Proxy) Transport() *http.Transport { return p.transport }

// Client returns an HTTP client bound to this exit.
func (p *Proxy) Client(timeout, idle time.Duration) *http.Client {
	return &http.Client{
		Transport: p.transport,
		Timeout:   timeout,
	}
}

// Redacted renders the exit for logs and metrics without credentials.
func (p *Proxy) Redacted() string {
	if p.Username == "" {
		return p.Scheme + "://" + p.Addr
	}
	return fmt.Sprintf("%s://%s:***@%s", p.Scheme, p.Username, p.Addr)
}

// dialSOCKS5 performs an RFC 1928 CONNECT with optional RFC 1929 auth.
func (p *Proxy) dialSOCKS5(ctx context.Context, network, addr string, remoteDNS bool) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s: %w", p.Addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}

	// Greeting. Offer no-auth plus user/pass when credentials exist.
	greeting := []byte{0x05, 0x01, 0x00}
	if p.Username != "" {
		greeting = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(greeting); err != nil {
		return fail(fmt.Errorf("socks5 greeting: %w", err))
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(fmt.Errorf("socks5 greeting reply: %w", err))
	}
	if reply[0] != 0x05 {
		return fail(fmt.Errorf("socks5: bad version %d", reply[0]))
	}
	switch reply[1] {
	case 0x00:
		// no auth
	case 0x02:
		if p.Username == "" {
			return fail(errors.New("socks5: server requires auth but no credentials configured"))
		}
		req := []byte{0x01, byte(len(p.Username))}
		req = append(req, p.Username...)
		req = append(req, byte(len(p.Password)))
		req = append(req, p.Password...)
		if _, err := conn.Write(req); err != nil {
			return fail(fmt.Errorf("socks5 auth: %w", err))
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			return fail(fmt.Errorf("socks5 auth reply: %w", err))
		}
		if ar[1] != 0x00 {
			return fail(errors.New("socks5: authentication rejected"))
		}
	default:
		return fail(fmt.Errorf("socks5: no acceptable auth method (server offered 0x%02x)", reply[1]))
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fail(fmt.Errorf("socks5: bad target %q: %w", addr, err))
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fail(fmt.Errorf("socks5: bad port %q", portStr))
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil && !remoteDNS {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fail(errors.New("socks5: target hostname too long"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req); err != nil {
		return fail(fmt.Errorf("socks5 connect: %w", err))
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fail(fmt.Errorf("socks5 connect reply: %w", err))
	}
	if head[1] != 0x00 {
		return fail(socksErr(head[1]))
	}
	// Drain the bound address so the stream starts at the payload.
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fail(fmt.Errorf("socks5 bound addr: %w", err))
		}
		skip = int(l[0])
	default:
		return fail(fmt.Errorf("socks5: unknown address type 0x%02x", head[3]))
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fail(fmt.Errorf("socks5 bound addr: %w", err))
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksErr(code byte) error {
	switch code {
	case 0x01:
		return errors.New("socks5: general failure")
	case 0x02:
		return errors.New("socks5: connection not allowed")
	case 0x03:
		return errors.New("socks5: network unreachable")
	case 0x04:
		return errors.New("socks5: host unreachable")
	case 0x05:
		return errors.New("socks5: connection refused")
	case 0x06:
		return errors.New("socks5: TTL expired")
	case 0x07:
		return errors.New("socks5: command not supported")
	case 0x08:
		return errors.New("socks5: address type not supported")
	default:
		return fmt.Errorf("socks5: connect failed (0x%02x)", code)
	}
}

// DialContext opens a raw TCP tunnel to addr through this exit. Used by the
// reachability probe and by the forward-proxy (CONNECT) ingress.
func (p *Proxy) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.Scheme == "socks5" || p.Scheme == "socks5h" {
		return p.dialSOCKS5(ctx, network, addr, p.Scheme == "socks5h")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return nil, fmt.Errorf("http proxy dial %s: %w", p.Addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}
	var sb strings.Builder
	sb.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
	if p.Username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
		sb.WriteString("Proxy-Authorization: Basic " + token + "\r\n")
	}
	sb.WriteString("Proxy-Connection: keep-alive\r\n\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("http proxy CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http proxy CONNECT reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		resp.Body.Close()
		if resp.StatusCode == http.StatusProxyAuthRequired {
			return nil, errors.New("http proxy: authentication required (407)")
		}
		return nil, fmt.Errorf("http proxy CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn preserves bytes the proxy already pushed into our reader.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// member is a Proxy plus its liveness state.
type member struct {
	*Proxy

	mu              sync.Mutex
	healthy         bool
	exitIP          string
	latencyMS       int64
	failures        int64
	softFails       int64
	consecFails     int
	quarantineUntil time.Time
	lastErr         string
	lastCheck       time.Time
	boundAccounts   map[string]struct{}

	// retired is terminal: it survives quarantine expiry, successful probes
	// and restarts, and only an explicit Revive clears it.
	retired       bool
	retiredReason string
	retiredAt     time.Time

	// quotaUntil is a different axis from quarantineUntil, and conflating the
	// two would be a bug in both directions.
	//
	// Quarantine means "this exit is broken" — transport, TLS, 407 — and a
	// successful probe is evidence it is fixed, so it clears. Quota means the
	// opposite: the exit is working perfectly and the upstream has said its
	// allowance is spent until a stated moment. A probe through this exit to
	// some unrelated host would succeed and must NOT be allowed to clear it,
	// and retrying before the deadline only burns a request to be told the same
	// thing again.
	//
	// So it is kept separately, it is only ever cleared by the clock, and Pick
	// demotes it without ever discarding it: an exit whose quota comes back is
	// the most valuable thing in the pool, and dropping it would throw away the
	// only member that is about to work.
	quotaUntil  time.Time
	quotaReason string
	quotaHits   int
}

// Pool owns every configured exit.
type Pool struct {
	mu      sync.RWMutex
	members []*member
	byID    map[string]*member
	health  config.Health
	log     *slog.Logger

	rr atomic.Uint64
}

// Spec is one exit to build the pool from.
type Spec struct {
	ID  string
	URL string
}

// NewPool builds the pool and marks everything healthy-but-unprobed, so the
// daemon serves traffic immediately instead of waiting for the first probe
// round.
func NewPool(specs []Spec, hc config.Health, log *slog.Logger) (*Pool, error) {
	p := &Pool{byID: make(map[string]*member, len(specs)), health: hc, log: log}
	for _, s := range specs {
		px, err := New(s.ID, s.URL)
		if err != nil {
			return nil, err
		}
		if _, dup := p.byID[s.ID]; dup {
			return nil, fmt.Errorf("duplicate proxy id %q", s.ID)
		}
		m := &member{
			Proxy:         px,
			healthy:       true,
			boundAccounts: make(map[string]struct{}),
		}
		p.members = append(p.members, m)
		p.byID[s.ID] = m
	}
	return p, nil
}

// SpecsFromConfig expands the Resin block plus the static list into specs.
func SpecsFromConfig(pc config.Proxies) []Spec {
	var specs []Spec
	if pc.Resin.Enabled {
		for _, name := range pc.Resin.Accounts {
			specs = append(specs, Spec{ID: "resin-" + name, URL: pc.Resin.ProxyURL(name)})
		}
	}
	for i, raw := range pc.Static {
		specs = append(specs, Spec{ID: fmt.Sprintf("static-%02d", i+1), URL: raw})
	}
	return specs
}

// Len returns the number of exits.
func (p *Pool) Len() int { return len(p.members) }

// IDs lists every exit id.
func (p *Pool) IDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.members))
	for _, m := range p.members {
		out = append(out, m.ID)
	}
	return out
}

// Get looks one exit up by id.
func (p *Pool) Get(id string) (*Proxy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	m, ok := p.byID[id]
	if !ok {
		return nil, false
	}
	return m.Proxy, true
}

// Pick chooses an exit for an account, skipping exclude (the exit that just
// failed). Healthy exits win; quarantined ones are tried only as a last
// resort, so a fully broken pool still limps instead of failing outright.
//
// Quota-cooled exits sit between those two tiers. They are usually in perfect
// health — an upstream that has spent an IP's allowance answers promptly and
// politely — so health alone would rank them first and hand out the one exit
// that is guaranteed to be refused. They are demoted, and among themselves the
// one whose allowance returns soonest goes first, because that is the order in
// which the pool actually regains capacity.
func (p *Pool) Pick(exclude string) *Proxy {
	if len(p.members) == 0 {
		return nil
	}
	now := time.Now()

	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthy, degraded, cooled, quarantined []*member
	// Retired exits are tracked separately precisely so that no later branch
	// can accidentally hand one out.
	var bestRetired *member
	bestRetiredFails := int64(-1)
	for _, m := range p.members {
		if m.ID == exclude {
			continue
		}
		m.mu.Lock()
		q := now.Before(m.quarantineUntil)
		_, _, cool := p.quotaCooledLocked(m, now)
		h := m.healthy
		ret := m.retired
		fails := m.failures
		// A measured-slow exit is demoted, not discarded: with a pool made of
		// unreliable nodes, "slow but answering" still beats "nothing".
		// Unmeasured exits (latency 0) always count as fast, so a freshly
		// added member is not starved before its first probe.
		slow := p.health.MaxLatency > 0 && m.latencyMS > 0 &&
			time.Duration(m.latencyMS)*time.Millisecond > p.health.MaxLatency
		m.mu.Unlock()
		if ret {
			if bestRetiredFails < 0 || fails < bestRetiredFails {
				bestRetired, bestRetiredFails = m, fails
			}
			continue
		}
		switch {
		case q:
			// Broken outranks out-of-quota: a cooled exit that is also
			// quarantined is the worse of the two, and quarantine is the
			// signal that lasts until proven otherwise.
			quarantined = append(quarantined, m)
		case cool:
			cooled = append(cooled, m)
		case h && !slow:
			healthy = append(healthy, m)
		default:
			degraded = append(degraded, m)
		}
	}
	// Round-robin inside a tier spreads load across equally good exits instead
	// of hammering whichever one happens to measure fastest right now.
	pick := func(list []*member) *member {
		if len(list) == 0 {
			return nil
		}
		n := p.rr.Add(1)
		return list[int(n)%len(list)]
	}
	if m := pick(healthy); m != nil {
		return m.Proxy
	}
	if m := pick(degraded); m != nil {
		return m.Proxy
	}
	if len(cooled) > 0 {
		// Not round-robin: when every exit is out of quota they share one reset
		// instant, so spreading load across them buys nothing, while going
		// soonest-first means the pool starts using the first allowance back
		// rather than discovering it by luck.
		soonest := cooled[0]
		best := time.Time{}
		soonest.mu.Lock()
		best = soonest.quotaUntil
		soonest.mu.Unlock()
		for _, m := range cooled[1:] {
			m.mu.Lock()
			u := m.quotaUntil
			m.mu.Unlock()
			if u.Before(best) {
				soonest, best = m, u
			}
		}
		return soonest.Proxy
	}
	if m := pick(quarantined); m != nil {
		return m.Proxy
	}
	// Everything was excluded (single-exit pool): fall back to any exit that
	// has not been retired.
	for _, m := range p.members {
		m.mu.Lock()
		ret := m.retired
		m.mu.Unlock()
		if !ret {
			return m.Proxy
		}
	}
	// Every exit in the pool is retired. Returning nil here would 502 the
	// gateway, so limp along on the least-bad retired exit — and say so out
	// loud, because an operator wants to be told about this state, not have it
	// hidden from them.
	if bestRetired != nil {
		p.log.Warn("every exit is retired; falling back to the least-bad one — revive or add exits to repair the pool",
			"exit", bestRetired.ID, "lifetime_failures", bestRetiredFails,
			"fix", "POST /admin/api/proxies/"+bestRetired.ID+"/revive")
		return bestRetired.Proxy
	}
	return nil
}

// MarkSuccess records a good round trip and clears the backoff.
//
// It deliberately does NOT clear retirement. A retired exit that answers a
// probe is still one an operator chose to take out of service; bringing it
// back on its own would silently undo that decision. Use Revive for that.
func (p *Pool) MarkSuccess(id string, latency time.Duration) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthy = true
	m.consecFails = 0
	m.quarantineUntil = time.Time{}
	m.lastErr = ""
	if latency > 0 {
		m.latencyMS = latency.Milliseconds()
	}
	m.lastCheck = time.Now()
}

// MarkFailure records a bad round trip and quarantines the exit once the
// configured threshold is crossed, with exponential backoff per exit.
func (p *Pool) MarkFailure(id, reason string) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures++
	m.consecFails++
	m.lastErr = reason
	m.lastCheck = time.Now()
	if m.consecFails >= p.health.FailureThreshold {
		m.healthy = false
		backoff := p.health.Quarantine
		for i := 1; i < m.consecFails-p.health.FailureThreshold+1 && backoff < p.health.MaxBackoff; i++ {
			backoff *= 2
		}
		if backoff > p.health.MaxBackoff {
			backoff = p.health.MaxBackoff
		}
		m.quarantineUntil = time.Now().Add(backoff)
		p.log.Warn("egress quarantined",
			"exit", id, "failures", m.failures, "consecutive", m.consecFails,
			"for", backoff.String(), "reason", reason)
	}
	// Quarantine is a nap; retirement is the door. An exit that has failed
	// RetireAfter times running, with no success in between to reset the
	// counter, is not going to come back — and re-probing it every 30 minutes
	// forever is exactly the noise this is meant to stop.
	if p.health.RetireAfter > 0 && !m.retired && m.consecFails >= p.health.RetireAfter {
		m.retired = true
		m.retiredAt = time.Now()
		m.retiredReason = fmt.Sprintf("%d consecutive failures with no success in between; last: %s",
			m.consecFails, reason)
		p.log.Warn("egress retired for good",
			"exit", id, "consecutive", m.consecFails, "retire_after", p.health.RetireAfter,
			"reason", reason, "undo", "POST /admin/api/proxies/"+id+"/revive")
	}
}

// MarkSoftFailure records an observation that says something about the
// request rather than about the exit.
//
// A 429 or a 5xx means the exit carried the request and brought back a
// reply: the IP is reachable and working. Blaming the exit for it would let an
// account-level quota drain the whole IP pool, so this only updates the
// dashboard and leaves health untouched.
func (p *Pool) MarkSoftFailure(id, reason string) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	m.softFails++
	m.lastErr = reason
	m.mu.Unlock()
}

// CoolForQuota records that an upstream has refused this exit until a stated
// moment, which is what an HTTP 429 with Retry-After is actually saying.
//
// It deliberately takes an absolute deadline rather than a duration: the
// upstream's Retry-After counts down to one fixed reset, so every exit that
// reports it agrees on the same instant. Storing a duration per exit would let
// each one drift by however long it took to be discovered, and the whole point
// of reading the header is to stop guessing when the allowance returns.
//
// A deadline in the past is ignored rather than recorded. It means the exit was
// already usable again by the time the failure was classified, and cooling it
// would take a working exit out of rotation on the strength of stale news.
func (p *Pool) CoolForQuota(id string, until time.Time, reason string) bool {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok || until.IsZero() || !time.Now().Before(until) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// An exit may be reported by several requests in flight at once. Keeping
	// the later deadline never shortens a cooldown that a sibling request
	// already established.
	if until.After(m.quotaUntil) {
		m.quotaUntil = until
		m.quotaReason = reason
		m.quotaHits++
	}
	return true
}

// QuotaCooled reports whether an exit is inside a quota cooldown, and until
// when. An expired cooldown reads as not cooled: the clock is the only thing
// that ends one, so there is nothing to clean up and no sweep to forget to run.
func (p *Pool) QuotaCooled(id string) (time.Time, string, bool) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return time.Time{}, "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return p.quotaCooledLocked(m, time.Now())
}

// quotaCooledLocked reads one member's cooldown with its lock already held.
func (p *Pool) quotaCooledLocked(m *member, now time.Time) (time.Time, string, bool) {
	if m.quotaUntil.IsZero() || !now.Before(m.quotaUntil) {
		return time.Time{}, "", false
	}
	return m.quotaUntil, m.quotaReason, true
}

// ClearQuotaCool ends a cooldown early. Nothing in the request path calls it —
// cooldowns end on their own — but an operator who has just topped up an
// upstream, or who knows the header was wrong, needs a way to say so.
func (p *Pool) ClearQuotaCool(id string) bool {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	had := !m.quotaUntil.IsZero() && time.Now().Before(m.quotaUntil)
	m.quotaUntil = time.Time{}
	m.quotaReason = ""
	return had
}

// QuotaCooledIDs lists every exit currently inside a cooldown, soonest to
// return first. The order is the useful part: it is the order in which the pool
// will get capacity back.
func (p *Pool) QuotaCooledIDs() []QuotaCool {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []QuotaCool
	for _, m := range p.members {
		m.mu.Lock()
		until, reason, held := p.quotaCooledLocked(m, now)
		id := m.ID
		m.mu.Unlock()
		if held {
			out = append(out, QuotaCool{
				ExitID: id,
				Until:  until,
				Reason: reason,
				After:  until.Sub(now).Round(time.Second).String(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// QuotaCool is one exit's outstanding quota cooldown.
type QuotaCool struct {
	ExitID string    `json:"exit_id"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
	// After is how long is left, filled in for the operator's benefit: reading
	// an absolute instant tells you nothing about whether to wait or go fix
	// something else.
	After string `json:"after,omitempty"`
}

// Bind records that an account currently egresses through this exit.
func (p *Pool) Bind(exitID, accountID string) {
	p.mu.RLock()
	m, ok := p.byID[exitID]
	p.mu.RUnlock()
	if !ok || accountID == "" {
		return
	}
	m.mu.Lock()
	m.boundAccounts[accountID] = struct{}{}
	m.mu.Unlock()
}

// Unbind removes an account from an exit's binding set.
func (p *Pool) Unbind(exitID, accountID string) {
	p.mu.RLock()
	m, ok := p.byID[exitID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	delete(m.boundAccounts, accountID)
	m.mu.Unlock()
}

// Health snapshots the pool for the admin API.
func (p *Pool) Health() []model.ProxyHealth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]model.ProxyHealth, 0, len(p.members))
	for _, m := range p.members {
		m.mu.Lock()
		h := model.ProxyHealth{
			ID:          m.ID,
			Addr:        m.Addr,
			Scheme:      m.Scheme,
			Healthy:     m.healthy,
			ExitIP:      m.exitIP,
			LatencyMS:   m.latencyMS,
			Failures:    m.failures,
			SoftFails:   m.softFails,
			ConsecFails: m.consecFails,
			LastError:   m.lastErr,
		}
		if !m.quarantineUntil.IsZero() && time.Now().Before(m.quarantineUntil) {
			t := m.quarantineUntil
			h.QuarantinedTill = &t
		}
		if until, reason, cool := p.quotaCooledLocked(m, time.Now()); cool {
			t := until
			h.QuotaUntil = &t
			h.QuotaReason = reason
			h.QuotaHits = m.quotaHits
		}
		if !m.lastCheck.IsZero() {
			t := m.lastCheck
			h.LastCheck = &t
		}
		for a := range m.boundAccounts {
			h.BoundAccounts = append(h.BoundAccounts, a)
		}
		h.Retired = m.retired
		h.RetiredReason = m.retiredReason
		if !m.retiredAt.IsZero() {
			t := m.retiredAt
			h.RetiredAt = &t
		}
		h.Prunable = p.pruneWorthyLocked(m)
		m.mu.Unlock()
		out = append(out, h)
	}
	return out
}

// pruneWorthyLocked reports whether an exit has earned permanent removal.
//
// It reads only the one member whose lock the caller already holds, so it can
// never introduce a lock-ordering problem between members.
//
// "Prune-worthy" is the operator's definition, not the scheduler's: an exit
// that is quarantined, has been marked unhealthy, or is over the latency
// ceiling is taking up room without earning it.
func (p *Pool) pruneWorthyLocked(m *member) bool {
	if m.retired {
		return false
	}
	if !m.healthy {
		return true
	}
	if !m.quarantineUntil.IsZero() && time.Now().Before(m.quarantineUntil) {
		return true
	}
	if p.health.MaxLatency > 0 && m.latencyMS > 0 &&
		time.Duration(m.latencyMS)*time.Millisecond > p.health.MaxLatency {
		return true
	}
	return false
}

// Retire takes an exit out of rotation until it is explicitly revived.
func (p *Pool) Retire(id, reason string) error {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown exit %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retired {
		return nil
	}
	if reason == "" {
		reason = "retired manually"
	}
	m.retired = true
	m.retiredAt = time.Now()
	m.retiredReason = reason
	p.log.Warn("egress retired by hand", "exit", id, "reason", reason)
	return nil
}

// Revive puts a retired exit back into rotation and clears its failure streak
// so it gets a fair chance rather than being retired again on the next blip.
func (p *Pool) Revive(id string) error {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown exit %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retired = false
	m.retiredReason = ""
	m.retiredAt = time.Time{}
	m.consecFails = 0
	m.quarantineUntil = time.Time{}
	m.healthy = true
	m.lastErr = ""
	p.log.Info("egress revived", "exit", id)
	return nil
}

// Prune retires every exit that is currently unusable — quarantined, marked
// unhealthy, or over the latency ceiling — and reports exactly what it did.
//
// It does not spare the last exit: an operator who asks to clear the garbage
// should be told the pool is now empty, not have one piece of garbage quietly
// kept on their behalf. Traffic keeps flowing anyway through Pick's
// least-bad-retired fallback.
func (p *Pool) Prune(reason string) model.PruneReport {
	rep := model.PruneReport{}
	if reason == "" {
		reason = "pruned as unusable"
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	usable := 0
	for _, m := range p.members {
		m.mu.Lock()
		switch {
		case m.retired:
			rep.AlreadyRetired = append(rep.AlreadyRetired, m.ID)
		case p.pruneWorthyLocked(m):
			m.retired = true
			m.retiredAt = now
			m.retiredReason = reason
			rep.Retired = append(rep.Retired, m.ID)
		default:
			usable++
		}
		m.mu.Unlock()
	}
	rep.UsableAfter = usable
	rep.PoolEmpty = usable == 0
	if len(rep.Retired) > 0 {
		p.log.Warn("pruned unusable exits", "retired", len(rep.Retired),
			"still_usable", usable, "pool_empty", rep.PoolEmpty)
	}
	return rep
}

// HealthyCount reports how many exits are currently usable.
func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	now := time.Now()
	for _, m := range p.members {
		m.mu.Lock()
		if m.healthy && !m.retired && !now.Before(m.quarantineUntil) {
			n++
		}
		m.mu.Unlock()
	}
	return n
}

// RetiredCount reports how many exits have been taken out of rotation for good.
func (p *Pool) RetiredCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, m := range p.members {
		m.mu.Lock()
		if m.retired {
			n++
		}
		m.mu.Unlock()
	}
	return n
}

// Healthy reports whether an exit is usable right now. The second result is
// false when the id is unknown, which callers must distinguish from "known but
// sick". A retired exit is never usable, however well its last probe went.
func (p *Pool) Healthy(id string) (bool, bool) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.retired && m.healthy && !time.Now().Before(m.quarantineUntil), true
}

// Run probes every exit on the configured interval until ctx is cancelled.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	p.ProbeAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every exit that is still in rotation, concurrently.
// Retired exits are skipped: they are out of the pool, so spending probe
// budget on them would be paying rent on an empty room.
func (p *Pool) ProbeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range p.members {
		m.mu.Lock()
		ret := m.retired
		m.mu.Unlock()
		if ret {
			continue
		}
		wg.Add(1)
		go func(m *member) {
			defer wg.Done()
			p.probe(ctx, m)
		}(m)
	}
	wg.Wait()
}

// ProbeOne probes a single exit by id.
func (p *Pool) ProbeOne(ctx context.Context, id string) error {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown exit %q", id)
	}
	p.probe(ctx, m)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.healthy {
		return nil
	}
	return errors.New(m.lastErr)
}

func (p *Pool) probe(ctx context.Context, m *member) {
	timeout := p.health.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	// Reachability: a TCP tunnel to the upstream confirms the exit works for
	// the traffic we actually care about, without needing credentials.
	if p.health.Reachability != "" {
		conn, err := m.DialContext(pctx, "tcp", p.health.Reachability)
		if err != nil {
			p.MarkFailure(m.ID, "unreachable: "+err.Error())
			return
		}
		conn.Close()
	}
	latency := time.Since(start)

	exitIP := ""
	if p.health.ProbeURL != "" {
		if ip, err := p.fetchExitIP(pctx, m); err != nil {
			// A failed IP probe is informative, not fatal: the tunnels that
			// matter already proved themselves above.
			m.mu.Lock()
			m.lastErr = "exit-ip probe: " + err.Error()
			m.mu.Unlock()
		} else {
			exitIP = ip
		}
	}

	m.mu.Lock()
	m.exitIP = exitIP
	m.latencyMS = latency.Milliseconds()
	m.healthy = true
	m.consecFails = 0
	m.quarantineUntil = time.Time{}
	if exitIP != "" {
		m.lastErr = ""
	}
	m.lastCheck = time.Now()
	m.mu.Unlock()
}

func (p *Pool) fetchExitIP(ctx context.Context, m *member) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.health.ProbeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json, text/plain")
	client := m.Client(p.health.Timeout, 0)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	return parseExitIP(body), nil
}

// parseExitIP tolerates both {"ip":"..."} and a bare address.
func parseExitIP(body []byte) string {
	var obj struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(body, &obj); err == nil {
		if s := strings.TrimSpace(obj.IP); net.ParseIP(s) != nil {
			return s
		}
	}
	// Plain-text echo services (icanhazip, ifconfig.me) return the address and
	// nothing else. Anything that is not an IP is not an exit IP: storing it
	// would put an arbitrary response body in the dashboard, in every consumer
	// of this field, and in the operator's mental model of their pool.
	if s := strings.TrimSpace(string(body)); net.ParseIP(s) != nil {
		return s
	}
	return ""
}
