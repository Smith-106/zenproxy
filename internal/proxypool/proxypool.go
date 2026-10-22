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
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
)

// PoisonRetireAfter is how many TLS/certificate poison strikes it takes to
// retire an exit for good. Quarantine already happens on the first such
// failure (see MarkFailure); retirement is the door that stops the probe loop
// from re-admitting a MITM slot every quarantine window.
//
// These strikes live on member.poisonConsec and are deliberately NOT cleared
// by a successful reachability probe: intermittent MITM slots often pass TLS
// to the probe host between real-request failures, which used to reset
// consecFails and keep the slot immortal. Only a real MarkSuccess (forwarded
// traffic) clears the poison streak. Ordinary transport noise still uses
// health.RetireAfter + consecFails.
const PoisonRetireAfter = 2

// tunnelKeepAlive arms TCP keepalive on every dialed conn — proxy tunnels,
// SOCKS slots and direct dials alike. The custom dialers used zero-value
// net.Dialers, which silently DISABLE keepalive (only net/http's built-in
// default dialer turns it on), so idle tunnels had no way to keep NAT and
// provider-edge bindings alive. The measured victim class: muse xhigh streams
// think silently for minutes, the edge reaps the quiet tunnel, and the stream
// dies mid-flight ("upstream stream ended without a completed response").
// 30s matches net/http's own default; the probes cost nothing at this rate
// and the kernel reaps a genuinely dead TCP peer within ~9 intervals.
const tunnelKeepAlive = 30 * time.Second

// PoisonTLS reports whether a failure reason means the exit cannot be trusted
// to carry the upstream's TLS stream. These are not blips: a slot that
// tunnels TCP but fails x509 verification will keep failing every real
// request, and a TCP-only reachability probe used to mark it healthy again.
//
// The malformed-HTTP-response match covers the provider-side MITM corruption
// measured on the 3129 family: the tunnel lands on a proxy edge that relays
// raw HTTP/2 frame bytes ("\x00\x00\x12\x04…", a SETTINGS frame) where the
// HTTP/1.1 response belongs. Like a bad certificate it is not transient
// noise — the edge keeps leaking frames until its operators fix the layer —
// and every strike burns a full attempt budget, so it earns the same fast
// treatment: one strike quarantines, two consecutive retire.
func PoisonTLS(reason string) bool {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "x509:"):
		return true
	case strings.Contains(lower, "certificate is not valid"):
		return true
	case strings.Contains(lower, "certificate signed by unknown authority"):
		return true
	case strings.Contains(lower, "tls: failed to verify"):
		return true
	case strings.Contains(lower, "tls: bad certificate"):
		return true
	case strings.Contains(lower, "malformed http response"):
		return true
	default:
		return false
	}
}

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

// New parses a proxy URL. Supported schemes: http, https, socks5, socks5h,
// and direct — a pseudo-exit that dials the target from the local host with
// no proxy at all, so the daemon keeps an upstream path open even when every
// rented egress at once is unreachable (measured: the shared 3129 edge POPs
// timed out at TCP while the server's own network reached the target fine).
func New(id, raw string) (*Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy %q: %w", id, err)
	}
	if u.Scheme == "direct" {
		p := &Proxy{
			ID:     id,
			Raw:    raw,
			Scheme: u.Scheme,
			url:    u,
		}
		p.transport = p.newTransport()
		return p, nil
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q (want http, https, socks5, socks5h or direct)", id, u.Scheme)
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
	if p.Scheme == "direct" {
		// No proxy at all: plain dial from the local host, capped like the
		// SOCKS path so a dead local NIC turns into rotation fuel instead of
		// a stuck request. No ForceAttemptHTTP2: the measured-good direct
		// request used HTTP/1.1, keep parity.
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			d := net.Dialer{KeepAlive: tunnelKeepAlive}
			return d.DialContext(dialCtx, network, addr)
		}
		return t
	}
	if p.Scheme == "http" || p.Scheme == "https" {
		// net/http handles CONNECT and Proxy-Authorization for us.
		t.Proxy = http.ProxyURL(p.url)
		// Without a DialContext a dead POP relies on nobody: the default
		// dialer has no timeout and the CONNECT dial hung on OS patience.
		// Cap it like the SOCKS path — live POPs dial in well under a
		// second, a dead one becomes fast rotation fuel instead of a
		// minutes-long slot holder. ForceAttemptHTTP2 keeps the TLS-over-
		// CONNECT path on h2: a custom DialContext alone would silently
		// downgrade every proxied upstream stream to HTTP/1.1.
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			d := net.Dialer{KeepAlive: tunnelKeepAlive}
			return d.DialContext(dialCtx, network, addr)
		}
		t.ForceAttemptHTTP2 = true
	} else {
		// SOCKS5 needs a custom dialer; there is no stdlib support.
		remote := p.Scheme == "socks5h"
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			// A dead socks slot used to hang the whole forward attempt for the
			// remaining request ctx (minutes). Dial+handshake through a live
			// resin slot measures 0.5-3s, so cap the tunnel setup and turn
			// dead slots into fast rotation fuel.
			dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return p.dialSOCKS5(dialCtx, network, addr, remote)
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
	if p.Scheme == "direct" {
		return "direct://local-egress"
	}
	if p.Username == "" {
		return p.Scheme + "://" + p.Addr
	}
	return fmt.Sprintf("%s://%s:***@%s", p.Scheme, p.Username, p.Addr)
}

// dialSOCKS5 performs an RFC 1928 CONNECT with optional RFC 1929 auth.
func (p *Proxy) dialSOCKS5(ctx context.Context, network, addr string, remoteDNS bool) (net.Conn, error) {
	d := net.Dialer{KeepAlive: tunnelKeepAlive}
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
	if p.Scheme == "direct" {
		// The local host IS the egress: dial the target directly.
		d := net.Dialer{KeepAlive: tunnelKeepAlive}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("direct dial %s: %w", addr, err)
		}
		return conn, nil
	}
	if p.Scheme == "socks5" || p.Scheme == "socks5h" {
		return p.dialSOCKS5(ctx, network, addr, p.Scheme == "socks5h")
	}
	d := net.Dialer{KeepAlive: tunnelKeepAlive}
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

	mu          sync.Mutex
	healthy     bool
	exitIP      string
	exitIPNote  string
	latencyMS   int64
	failures    int64
	softFails   int64
	consecFails int
	// poisonConsec counts TLS/certificate poison strikes. Unlike consecFails,
	// probe success does not reset it — see PoisonRetireAfter.
	poisonConsec    int
	quarantineUntil time.Time
	// quarantineClass is the panel word for why this nap started. It is the
	// only form that survives a restart; the raw dial error does not.
	quarantineClass string
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

	// freeTierUntil is a third axis: the exit answered an already-enveloped
	// FreeTierError. Like quotaUntil it must NOT be cleared by a successful
	// probe (probes never speak the free-tier gate), and like quarantineUntil
	// it expires on a clock so a later residential-looking IP reuse can be
	// tried again. Measured 2026-10: free-proxy slots flip between 403 and 200
	// for the same envelope, so parking them briefly is what makes rotation
	// converge instead of rediscovering the same refusals every minute.
	freeTierUntil  time.Time
	freeTierReason string

	// gatePassUntil is the ONLY positive evidence the free-tier gate ever gives:
	// an enveloped request for a free model came back 2xx through this exit.
	//
	// It cannot be inferred from anything else the pool already tracks. Health
	// is blind to it — probes go to ipify and opencode.ai:443 and never speak
	// the gate — and "not currently blocked" is not the same fact: every one of
	// the 1,613 members is unblocked most of the time, and almost all of them
	// answer FreeTierError the moment an enveloped request touches them.
	//
	// Measured 2026-10-17 over three hours of live traffic (10,192 enveloped
	// upstream attempts, 220 admissions): EVERY admission came from six exits —
	// resin-slot-199 (126 admissions / 1 refusal), static-161 (104/0),
	// static-1040 (39/0), static-897 (15/0), resin-slot-386 (13/4),
	// static-44 (8/0) — while 804 exits were refused five or more times and
	// were never admitted once. The admission rate was flat across load
	// (124 attempts → 2, 330 → 10), so this is a flagged population with a few
	// clean IPs inside it, not congestion. Sampling it uniformly is why the
	// pool's overall pass rate sat at 2.2%; PickForAccount now puts passers
	// first (see gatePassers in PickForAccount), which is the difference
	// between a lottery and a route.
	gatePassUntil time.Time
	gatePasses    int

	// gateScore ranks admitted exits against each other, because "the gate
	// admits it" is not the same as "it carries a conversation". Measured on the
	// six admitted exits 2026-10-17, admissions versus mid-stream drops were
	// static-161 131/32, static-1040 69/70, static-44 53/94, static-897 38/45 —
	// a threefold spread in what a client actually experiences
	// (the upstream stream ended without a completed response), while the tier
	// treated all four as interchangeable and rotated evenly. So the better
	// tunnel was handed the same share as the worse one, and a third of the
	// attempts had to be re-rolled.
	//
	// It is a bounded, self-healing rank rather than a ratio: +gateScorePass on
	// every admission, -gateScoreDrop on every drop, clamped both ways. A ratio
	// of lifetime counters would punish an exit forever for an outage early in
	// its life (static-1040 opened 39 admissions against 70 drops and stayed
	// there); a bounded score walks back out of the floor after a few clean
	// admissions, which is what the next request should believe. Drops decay
	// faster than admissions accumulate precisely because one drop is stronger
	// evidence than one admission: the gate can admit an exit that then kills
	// every stream, and a client pays for each of those.
	gateScore int

	// gateDrops counts mid-stream drops on this exit, for the health panel only;
	// gateScore is what selection reads.
	gateDrops int

	// regionBlocked records, per model name, that this exit's country is refused
	// for that model — and it is keyed by model because that is what the
	// upstream actually does. Measured on one pool: an exit in RU answered 403
	// RegionError for one free model and 200 for another in the same second. So
	// a region block is NOT a property of the exit, and cooling the whole exit
	// for it would take a perfectly working IP out of rotation for every other
	// model because one of them is unavailable there.
	regionBlocked map[string]time.Time
}

// regionBlockTTL is how long a region refusal is trusted.
//
// Unlike a quota cooldown there is no clock to read: the upstream never says
// when a country reopens, so something has to decide when to stop believing it.
// The two mistakes are not symmetric in cost. Believe it too long and an exit
// that could serve the model again stays out of rotation; believe it too short
// and every request for that model pays one wasted attempt to rediscover a fact
// already known. Measured, these blocks are stable across hours — the same two
// exits refused the same model every time they were tried — so the longer end is
// the cheaper error, and the retry cost is one attempt per expiry rather than
// one per request.
const regionBlockTTL = 6 * time.Hour

// Pool owns every configured exit.
type Pool struct {
	mu      sync.RWMutex
	members []*member
	byID    map[string]*member
	health  config.Health
	log     *slog.Logger

	// statePath persists the members' liveness slice across restarts
	// (see persist.go). Empty disables persistence.
	statePath string
	dirty     atomic.Bool
	// stateFiles caches a parsed exit-state file while its mtime and size
	// stay the same. One status call otherwise parses the same files several
	// times: the peer marks, the replica list, and the split.
	stateFileMu sync.Mutex
	stateFiles  map[string]cachedExitFile

	rr atomic.Uint64

	// peerRetired is the other replica's retired set, refreshed from its state
	// file. Picks read it without touching the disk. The map is replaced whole
	// and never mutated.
	peerRetired atomic.Value // map[string]struct{}

	// outageMu guards the global-failure circuit breaker (see
	// noteFailureOutcome). It is deliberately a leaf lock — never held while
	// taking p.mu or a member lock — so MarkFailure's hold order
	// p.mu.RLock -> member.mu -> outageMu cannot deadlock.
	outageMu    sync.Mutex
	globalFails int
	outage      bool
}

// defaultOutageThreshold is how many consecutive pool-wide failure outcomes
// trip the retirement breaker when config leaves OutageThreshold at zero.
// Twelve-attempt rotation means roughly four fully-failed requests; measured
// against the incident that motivated the breaker (3518 of 3544 exits
// retired overnight by one provider-wide glitch), 40 catches the mass
// failure while staying well above single-exit noise, which any success
// elsewhere in the pool keeps reset.
const defaultOutageThreshold = 40

// outageLimit resolves the configured breaker sensitivity. Zero means use
// the default; negative disables the breaker entirely.
func (p *Pool) outageLimit() int {
	switch t := p.health.OutageThreshold; {
	case t < 0:
		return 0
	case t == 0:
		return defaultOutageThreshold
	default:
		return t
	}
}

// defaultRateLimitCooldown is the window lent to a 429 that arrived with no
// Retry-After of its own. Short enough to shed the burst waves measured on
// muse (its per-key throttles arrive in roughly minute-scale storms); long
// enough that the struck exit is not immediately re-picked inside the same
// wave and fed the full prompt again.
const defaultRateLimitCooldown = 90 * time.Second

// rateLimitCooldown resolves the configured headerless-429 cooldown. Zero
// means use the default; negative disables the lending entirely (a headerless
// 429 then only rotates, exactly as before this existed).
func (p *Pool) RateLimitCooldown() time.Duration {
	switch t := p.health.RateLimitCooldown; {
	case t < 0:
		return 0
	case t == 0:
		return defaultRateLimitCooldown
	default:
		return t
	}
}

// SetRateLimitCooldownDisabled is a test seam: it forces the
// headerless-429 lending off without going through config parsing.
func (p *Pool) SetRateLimitCooldownDisabled() {
	p.mu.Lock()
	p.health.RateLimitCooldown = -time.Second
	p.mu.Unlock()
}

// noteFailureOutcome feeds the breaker one more failure. It reports whether
// the breaker is open, in which case callers must not retire.
func (p *Pool) noteFailureOutcome() bool {
	p.outageMu.Lock()
	defer p.outageMu.Unlock()
	p.globalFails++
	limit := p.outageLimit()
	if limit > 0 && !p.outage && p.globalFails >= limit {
		p.outage = true
		p.log.Warn("outage circuit breaker OPEN — retirement suspended pool-wide",
			"consecutive_failures", p.globalFails, "threshold", limit,
			"hint", "failures look correlated (provider-wide); quarantine still applies, streaks keep accruing, breaker lifts on the next success")
	}
	return p.outage
}

// noteSuccessOutcome records one pool-wide success — a request that got
// headers, a successful probe, even a soft 429, all of which prove some
// tunnel carried traffic — clearing the breaker and the failure run.
func (p *Pool) noteSuccessOutcome() {
	p.outageMu.Lock()
	was := p.outage
	p.globalFails = 0
	p.outage = false
	p.outageMu.Unlock()
	if was {
		p.log.Info("outage circuit breaker lifted — retirement resumes individually",
			"hint", "exits that stayed dead retire again on their next failure; streaks accrued during the outage are preserved")
	}
}

// OutageActive reports whether the retirement breaker is currently open.
func (p *Pool) OutageActive() bool {
	p.outageMu.Lock()
	defer p.outageMu.Unlock()
	return p.outage
}

// OutageState returns the breaker snapshot for consoles: tripped, current
// consecutive-failure count, and the effective threshold (0 when disabled).
func (p *Pool) OutageState() (tripped bool, globalFails, threshold int) {
	p.outageMu.Lock()
	defer p.outageMu.Unlock()
	return p.outage, p.globalFails, p.outageLimit()
}

// Spec is one exit to build the pool from.
type Spec struct {
	ID  string
	URL string
}

// NewPool builds the pool and marks everything healthy-but-unprobed, so the
// daemon serves traffic immediately instead of waiting for the first probe
// round.
//
// An optional trailing statePath turns on exit-health persistence (persist.go):
// the pool replays quarantines, verdicts and failure streaks recorded by the
// previous process at startup, and flushes its own changes debounced while it
// runs. Omit it for stateless operation (tests).
func NewPool(specs []Spec, hc config.Health, log *slog.Logger, statePath ...string) (*Pool, error) {
	p := &Pool{byID: make(map[string]*member, len(specs)), health: hc, log: log}
	if len(statePath) > 0 {
		p.statePath = statePath[0]
	}
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
	p.peerRetired.Store(map[string]struct{}{})
	p.loadState()
	p.refreshPeerRetired()
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
	if pc.DirectEgress {
		specs = append(specs, Spec{ID: "direct", URL: "direct://"})
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

// Pick chooses an exit without regard to the model being asked for. Callers
// that know the model should use PickFor; this is the wrapper for the ones that
// cannot, such as the forward ingress and the health panel.
func (p *Pool) Pick(exclude string) *Proxy { return p.PickFor("", exclude) }

// PickFor chooses an exit for a request for one model, skipping exclude (the
// exit that just failed). Healthy exits win; quarantined ones are tried only as
// a last resort, so a fully broken pool still limps instead of failing outright.
//
// Two kinds of "known bad right now" are demoted below healthy exits, and they
// are separate tiers because they are separate facts:
//
//   - quota-cooled means the upstream named a time at which it will serve this
//     exit again. There is a clock, so it is the closest of the three to usable.
//   - region-blocked means the upstream refuses this exit's country for this
//     model. There is no clock, only a policy that changes on its own schedule.
//   - quarantined means the exit is suspected broken, which is the weakest
//     claim of the three: the others are stated by the upstream, this one is
//     inferred from failures.
//
// The ordering among those last tiers only matters when nothing healthy is left,
// at which point the request is likely to fail wherever it goes. Ranking them by
// how strong the evidence is at least makes the choice explainable.
//
// A model of "" means the caller cannot judge geography, and no region tier is
// applied — guessing there would take exits out of rotation on no evidence.
// PickFor chooses an exit without knowing which account is asking, which means
// it cannot tell a shared exit from a private one. It is the wrapper for callers
// that genuinely have no account context, and it keeps the old behaviour.
func (p *Pool) PickFor(model, exclude string) *Proxy {
	return p.PickForAccount("", model, exclude)
}

// PickForAccount chooses an exit for one account's request for one model,
// skipping exclude (the exit that just failed).
//
// It differs from a plain PickFor only in that it knows who is asking, and can
// therefore prefer an exit that no other account is already using. That matters
// because the pool is the redundancy: two accounts sharing an exit share its
// per-IP quota and its region refusals, so a failure that rotation was supposed
// to absorb instead lands on both of them at once. Collisions arise naturally,
// since an account only re-resolves its exit when the old one goes bad, and the
// replacement used to be chosen without regard to anyone else.
func (p *Pool) PickForAccount(accountID, model, exclude string) *Proxy {
	return p.PickForAccountGated(accountID, model, exclude, false)
}

// PickForAccountGated is PickForAccount for a request the free-tier gate will
// judge, and gated changes exactly one thing: an exit the gate has admitted
// stays a candidate even while quarantined, ranked right behind the admitted
// exits that are not quarantined.
//
// The two verdicts measure different links and on this pool they routinely
// disagree. Quarantine comes from a health probe to ipify/opencode.ai:443;
// admission comes from a real enveloped request that came back 2xx through the
// exit. Measured 2026-10-17: four of the six admitted exits were quarantined at
// the same moment by probe failures (consec_fails 3-4) while the gate was still
// serving them, and with those four gone the pool fell back to non-admitted
// exits and refused every request in that window. The evidence is not
// symmetric — a probe says "this hop cannot reach a probe host", admission says
// "this hop carried the exact request we care about".
//
// It is a candidate, not a promotion: a genuinely dead exit keeps failing real
// requests and its quarantine is renewed. Plain traffic never sees this tier, so
// paid models keep their old aversion to quarantined exits.
func (p *Pool) PickForAccountGated(accountID, model, exclude string, gated bool) *Proxy {
	if len(p.members) == 0 {
		return nil
	}
	now := time.Now()
	model = strings.TrimSpace(model)

	p.mu.RLock()
	defer p.mu.RUnlock()

	var healthyFree, healthyTaken, degradedFree, degradedTaken []*member
	var cooled, region, quarantined []*member
	// gatePassers are exits the free-tier gate has admitted recently. They are
	// kept in their own tier ahead of every other healthy exit because on this
	// pool they are the only exits that work for free models at all: the gate
	// refuses by exit population, not by load (see member.gatePassUntil for the
	// measurement). Deliberately NOT subject to the account-sharing tiebreak
	// below — sharing a working exit costs a little per-IP allowance, while
	// stepping off it to a "free" exit costs the whole request.
	//
	// Each admitted exit appears gateWeight(score) times, so a tunnel that
	// actually carries streams is drawn more often than one that drops a third
	// of them. Repetition is the weighting mechanism because pick() scatters a
	// hash across the whole list — list order carries no meaning there, only
	// multiplicity does.
	var gatePassers, gatePassersImpaired, gatePassersLapsed, gatePassersHistory, gatePassersParked []*member
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
		ft := p.freeTierBlockedLocked(m, now)
		reg := p.regionBlockedLocked(m, model, now)
		h := m.healthy
		ret := m.retired
		fails := m.failures
		gatePass := !m.gatePassUntil.IsZero() && now.Before(m.gatePassUntil)
		gateScoreVal := m.gateScore
		gatePassesVal := m.gatePasses
		consec := m.consecFails
		lastErr := m.lastErr
		// proven is the pool's own verdict that this exit has carried free-tier
		// traffic, independent of the 30-minute admission TTL and of probe health:
		// without it a proven exit becomes invisible to gated requests the moment
		// its admission lapses or a probe fails, which is how the whole admitted
		// set disappeared once (four exits retired, two lapsed, nothing left).
		proven := gateScoreVal >= gateGoodScore
		// A probe failure is blind to the gate, so it must not hide an exit the
		// gate is serving. A real request failure is the opposite: this exit
		// just failed to carry traffic. Leaving it in a front tier is how four
		// dead proxies absorbed 281 attempts and zero admissions in one window
		// (2026-10-17 21:09) — each dial timed out, quarantine put them in the
		// impaired tier, and the next gated pick took them straight back.
		requestSick := consec > 0 && !probeQuarantineReason(lastErr)
		// A measured-slow exit is demoted, not discarded: with a pool made of
		// unreliable nodes, "slow but answering" still beats "nothing".
		// Unmeasured exits (latency 0) always count as fast, so a freshly
		// added member is not starved before its first probe.
		slow := p.health.MaxLatency > 0 && m.latencyMS > 0 &&
			time.Duration(m.latencyMS)*time.Millisecond > p.health.MaxLatency
		// The other replica already retired this exit for a dial failure. It
		// can still answer here, so it stays usable, but it waits behind exits
		// both replicas still accept. An active free-tier admission keeps its
		// front seat: those exits are scarce, and none of the disagreed set
		// was admitted when this was measured.
		peerDead := p.peerHasRetired(m.ID)
		// taken means some OTHER account is parked here. An empty accountID
		// means the caller cannot say who is asking (the health panel, the
		// forward ingress), and then nothing is taken: without an identity to
		// compare against, every exit looks equally free and this reduces to
		// the previous behaviour exactly.
		taken := false
		if accountID != "" {
			others := len(m.boundAccounts)
			if _, self := m.boundAccounts[accountID]; self {
				others--
			}
			taken = others > 0
		}
		m.mu.Unlock()
		if ret {
			if bestRetiredFails < 0 || fails < bestRetiredFails {
				bestRetired, bestRetiredFails = m, fails
			}
			continue
		}
		switch {
		case q:
			// Broken outranks the rest: a cooled exit that is also quarantined
			// is the worse of the two, and quarantine is the signal that lasts
			// until proven otherwise. The one exception is an admitted exit on a
			// gated request — see PickForAccountGated.
			if gated && (gatePass || proven) && !ft && !requestSick && probeQuarantineReason(lastErr) {
				gatePassersImpaired = append(gatePassersImpaired, m)
			}
			quarantined = append(quarantined, m)
		case cool, ft:
			// Quota-cooled and free-tier-blocked are both "working exit, wrong
			// moment": demote together so Pick still prefers unblocked IPs.
			//
			// For a gated request they stay candidates behind everything else
			// that is gated, because the park and the gate measure different
			// things: the park says "this exit was refused minutes ago", while
			// the alternative fallback says "this exit has never been admitted
			// once in 10,000 attempts". Measured 2026-10-17 after the admission
			// set was seeded: 129 of 134 refusals in one window came from
			// never-admitted exits that the pool only reached because every
			// admitted exit was parked — the pool was choosing a certain refusal
			// over a stale admission. The park still holds the exit out of every
			// non-gated decision, and a refusal still re-arms the park, so a
			// genuinely dead passer cannot be hammered: the red-window fast-fail
			// bounds each request.
			if gated && (gatePass || proven) && !requestSick {
				gatePassersParked = append(gatePassersParked, m)
			}
			cooled = append(cooled, m)
		case reg:
			region = append(region, m)
		case h && !slow:
			if gatePass && !requestSick {
				for i := gateWeight(gateScoreVal); i > 0; i-- {
					gatePassers = append(gatePassers, m)
				}
			} else if !peerDead && gated && proven && !ft && !cool && !requestSick {
				// Admission lapsed, reputation did not. The admission TTL is 30
				// minutes, which is short compared with how long an IP stays
				// clean — measured 2026-10-17, two of the six admitted exits
				// (resin-slot-199 with 126 admissions, resin-slot-386 with 13)
				// simply aged out of the tier and were then only reachable
				// whenever no admitted exit was left, so they never got the one
				// attempt that would re-admit them. Retrying a lapsed-but-proven
				// exit costs one attempt and has a far better hit rate than the
				// 0.4% lottery over never-admitted exits.
				gatePassersLapsed = append(gatePassersLapsed, m)
			} else if !peerDead && gated && gatePassesVal >= gateGoodScore && !ft && !cool && !requestSick {
				// Score can be driven to the floor by a burst of drops or
				// refusals and then the exit disappears into the 1,600-wide
				// lottery. The pass count is the part that does not decay:
				// an exit that has carried eight or more free requests is
				// still a better first try than one that never has, once it
				// is no longer request-sick. Measured 2026-10-17 21:18, after
				// the known passers had all been scored to -8, thirty minutes
				// of traffic produced zero admissions.
				gatePassersHistory = append(gatePassersHistory, m)
			}
			if peerDead && !(gatePass && !requestSick) {
				if taken {
					degradedTaken = append(degradedTaken, m)
				} else {
					degradedFree = append(degradedFree, m)
				}
				break
			}
			if taken {
				healthyTaken = append(healthyTaken, m)
			} else {
				healthyFree = append(healthyFree, m)
			}
		default:
			// Degraded is the same disagreement as quarantine, one step earlier:
			// a few probe failures on a tunnel the gate is still admitting. Keep
			// them as gated candidates behind the healthy admissions instead of
			// dropping to an exit the gate has never admitted.
			if gated && (gatePass || proven) && !ft && !cool && !reg && !requestSick && probeQuarantineReason(lastErr) {
				gatePassersImpaired = append(gatePassersImpaired, m)
			}
			if taken {
				degradedTaken = append(degradedTaken, m)
			} else {
				degradedFree = append(degradedFree, m)
			}
		}
	}
	// Scatter successive picks across the WHOLE member list instead of walking
	// it from the front. The member order is config order (resin block first,
	// then every static, then direct), and account bindings are long-lived, so
	// the pool only makes a handful of picks per day: a front-walking counter
	// (n % len) kept landing inside the resin prefix forever and left any large
	// static tail unreached for days — measured 2026-10-14, 3544 static exits
	// never served a request while their IDs aged out into retirement. The
	// Knuth multiplicative map (golden-ratio hash) sends consecutive counters
	// to well-scattered indices over the full list, stays deterministic for
	// tests, and needs no seed; a list-length change just re-maps.
	pick := func(list []*member) *member {
		if len(list) == 0 {
			return nil
		}
		n := p.rr.Add(1)
		return list[int((n*0x9E3779B1)%uint64(len(list)))]
	}
	// Within a tier, an exit no other account is using comes first. Sharing an
	// exit means sharing its per-IP allowance and its region refusals, which is
	// exactly the redundancy the pool exists to provide, so splitting a tier this
	// way costs nothing and keeps accounts apart. It is deliberately a tiebreak
	// and not a demotion: a healthy exit that is busy is still better than a
	// degraded one, so the tier order itself is untouched.
	// Gate passers first: an exit the free-tier gate has admitted is the only
	// kind that can serve a free model right now, so it outranks even a
	// healthy-but-unproven exit that no other account is using. When there are
	// no passers (cold start, or every passer parked by a refusal) this branch
	// is empty and selection falls through to exactly the previous behaviour.
	if m := pick(gatePassers); m != nil {
		return m.Proxy
	}
	// Admitted but impaired (probe-quarantined or degraded) comes next for gated
	// requests only: a probe failure is weaker evidence than a real 2xx through
	// the same exit, and the alternative — a healthy exit the gate has never
	// admitted — is measurably a refusal. Empty for plain traffic.
	if m := pick(gatePassersImpaired); m != nil {
		return m.Proxy
	}
	// Then exits whose admission lapsed while their track record did not. Still
	// gated-only: for plain traffic a lapsed admission is no information at all.
	if m := pick(gatePassersLapsed); m != nil {
		return m.Proxy
	}
	// Then exits whose score was walked down to the floor but which have
	// actually carried free traffic. The score is allowed to go negative; the
	// pass count is not. Still behind a currently-good exit, still skipped
	// while request-sick, and still gated-only.
	if m := pick(gatePassersHistory); m != nil {
		return m.Proxy
	}
	// Then admitted-but-parked: refused or quota-cooled within the block window.
	// Still ahead of an exit the gate has never admitted, for the reason in the
	// cool/ft branch — a stale admission beats a certain refusal.
	if m := pick(gatePassersParked); m != nil {
		return m.Proxy
	}
	if m := pick(healthyFree); m != nil {
		return m.Proxy
	}
	if m := pick(healthyTaken); m != nil {
		return m.Proxy
	}
	if m := pick(degradedFree); m != nil {
		return m.Proxy
	}
	if m := pick(degradedTaken); m != nil {
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
	// Region-blocked exits are never handed out. Measured, a known RegionError
	// pair burns an attempt for a guaranteed 403; when every remaining exit is
	// refused for this model the honest answer is "nothing usable", not "pick
	// one and watch it fail". The region slice is kept above only so the
	// ranking still classifies them instead of letting them look healthy.
	_ = region
	if m := pick(quarantined); m != nil {
		return m.Proxy
	}
	// Single-exit limp: the sole member was passed as `exclude`, so the tiers
	// above saw an empty pool. Hand it back unless it is region-blocked for
	// this model. Multi-exit pools must not resurrect an excluded exit or a
	// region-blocked peer through this door — that undoes fail-fast.
	if exclude != "" && len(p.members) == 1 {
		m := p.members[0]
		if m.ID == exclude {
			m.mu.Lock()
			ret := m.retired
			reg := p.regionBlockedLocked(m, model, now)
			m.mu.Unlock()
			if !ret && !reg {
				return m.Proxy
			}
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
	m.poisonConsec = 0 // real traffic proved the cert path; wipe the poison streak
	m.quarantineUntil = time.Time{}
	m.quarantineClass = ""
	m.lastErr = ""
	if latency > 0 {
		m.latencyMS = latency.Milliseconds()
	}
	m.lastCheck = time.Now()
	p.markDirty()
	p.noteSuccessOutcome()
}

// MarkFailure records a bad round trip and quarantines the exit once the
// configured threshold is crossed, with exponential backoff per exit.
//
// TLS/certificate poison is special-cased: one strike quarantines, and a short
// consecutive streak retires. A MITM resin slot that passes a TCP tunnel but
// fails x509 on the real HTTPS request used to need FailureThreshold hits to
// nap and RetireAfter hits to leave — while probes kept clearing the streak.
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

	// Feed the global breaker before deciding anything per-exit: during a
	// provider-wide outage every exit looks individually dead, and acting on
	// that perception is exactly the mass-retire false positive the breaker
	// exists to prevent (3518 of 3544 exits, one bad night).
	outage := p.noteFailureOutcome()

	threshold := p.health.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}
	poison := PoisonTLS(reason)
	if poison {
		threshold = 1
		m.poisonConsec++
	}
	if m.consecFails >= threshold {
		m.healthy = false
		backoff := p.health.Quarantine
		for i := 1; i < m.consecFails-threshold+1 && backoff < p.health.MaxBackoff; i++ {
			backoff *= 2
		}
		if backoff > p.health.MaxBackoff {
			backoff = p.health.MaxBackoff
		}
		m.quarantineUntil = time.Now().Add(backoff)
		m.quarantineClass = classifyQuarantine(reason)
		p.log.Warn("egress quarantined",
			"exit", id, "failures", m.failures, "consecutive", m.consecFails,
			"poison_consec", m.poisonConsec,
			"for", backoff.String(), "reason", reason, "poison_tls", poison,
			"outage", outage)
	}
	// Quarantine is a nap; retirement is the door. Ordinary exits use
	// consecFails vs RetireAfter. Poison TLS uses poisonConsec vs
	// PoisonRetireAfter so a lucky probe between MITM blips cannot wipe the
	// streak (probe success clears consecFails but not poisonConsec).
	// While the outage breaker is open the door is locked: no retirement on
	// any path. Streaks keep accruing, so whatever stays dead after the
	// breaker lifts retires on its very next failure; whatever recovered
	// heals via probe success instead.
	if !m.retired && !outage {
		if poison && PoisonRetireAfter > 0 && m.poisonConsec >= PoisonRetireAfter {
			m.retired = true
			m.retiredAt = time.Now()
			m.retiredReason = fmt.Sprintf("%d TLS poison strikes (probe success does not reset); last: %s",
				m.poisonConsec, reason)
			p.log.Warn("egress retired for good",
				"exit", id, "poison_consec", m.poisonConsec, "retire_after", PoisonRetireAfter,
				"reason", reason, "poison_tls", true,
				"undo", "POST /admin/api/proxies/"+id+"/revive")
		} else if !poison {
			retireAfter := p.health.RetireAfter
			// An exit the gate has admitted hundreds of times is not noise, and
			// its streak is not evidence about the IP: on this pool it is usually
			// evidence about the proxy provider's connection slots, because the
			// free-tier route concentrates traffic on a handful of exits.
			// Measured 2026-10-17: all four working exits retired within two
			// minutes of each other, twenty consecutive "proxyconnect ... i/o
			// timeout" / "Not Enough Connections" dial failures apiece, while they
			// had carried hundreds of successful free-tier requests that same
			// hour. Retirement is permanent, so it turned our own load into a
			// permanent loss of the only route that worked; a nap is the correct
			// verdict for a proven exit, and the quarantine above already applies
			// it. Retirement stays available for exits that have never carried a
			// free request (the 1,600-strong flagged population) and for TLS
			// poison, which is a data-safety matter rather than a capacity one.
			// The exemption is scoped to the admission window rather than to a
			// lifetime score, so it cannot become free credit: an exit that stops
			// being admitted for thirty minutes is retired like any other, while
			// one the gate is serving right now is napping, not dead.
			proven := !m.gatePassUntil.IsZero() && time.Now().Before(m.gatePassUntil)
			if retireAfter > 0 && m.consecFails >= retireAfter && !proven {
				m.retired = true
				m.retiredAt = time.Now()
				m.retiredReason = fmt.Sprintf("%d consecutive failures with no success in between; last: %s",
					m.consecFails, reason)
				p.log.Warn("egress retired for good",
					"exit", id, "consecutive", m.consecFails, "retire_after", retireAfter,
					"reason", reason, "poison_tls", false,
					"undo", "POST /admin/api/proxies/"+id+"/revive")
			} else if retireAfter > 0 && m.consecFails == retireAfter && !proven {
				// Exactly at the threshold but held by the breaker: say so
				// once per streak, so consoles can tell deferred executions
				// from healthy exits during an outage.
				p.log.Info("retirement deferred by outage breaker",
					"exit", id, "consecutive", m.consecFails, "retire_after", retireAfter,
					"reason", reason)
			} else if retireAfter > 0 && m.consecFails == retireAfter && proven {

				p.log.Warn("retirement withheld: this exit has carried free-tier traffic",
					"exit", id, "consecutive", m.consecFails, "retire_after", retireAfter,
					"gate_score", m.gateScore, "gate_passes", m.gatePasses,
					"reason", reason, "action", "quarantined instead")
			}
		}
	}
	p.markDirty()
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
	// A 429 or 5xx still proves the tunnel carried a round trip, so it counts
	// as pool-wide evidence of life for the outage breaker — an account-level
	// quota storm must not be able to hold the breaker open and freeze the
	// pool into quarantine-only mode.
	p.noteSuccessOutcome()
	p.markDirty()
}

// Quarantine parks an exit for d without counting toward retirement.
// Prefer BlockFreeTier for FreeTierError — probes clear quarantineUntil.
func (p *Pool) Quarantine(id string, d time.Duration, reason string) bool {
	if id == "" {
		return false
	}
	if d <= 0 {
		d = p.health.Quarantine
		if d <= 0 {
			d = 5 * time.Minute
		}
	}
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	until := time.Now().Add(d)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retired {
		return false
	}
	if until.After(m.quarantineUntil) {
		m.quarantineUntil = until
		m.quarantineClass = classifyQuarantine(reason)
		m.lastErr = reason
		m.lastCheck = time.Now()
		p.log.Info("egress quarantined",
			"exit", id, "for", d.String(), "reason", reason)
		p.markDirty()
		return true
	}
	return false
}

// BlockFreeTier parks an exit against free-tier traffic until a deadline.
//
// Unlike Quarantine / MarkFailure, a successful health probe does NOT clear
// this — probes never exercise the OpenCode free-tier gate, so "ipify works"
// is not evidence the exit will pass FreeTierError. The clock is the only
// clearer, same as quotaUntil.
func (p *Pool) BlockFreeTier(id string, d time.Duration, reason string) bool {
	if id == "" {
		return false
	}
	if d <= 0 {
		d = 30 * time.Minute
	}
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	until := time.Now().Add(d)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retired {
		return false
	}
	if until.After(m.freeTierUntil) {
		m.freeTierUntil = until
		m.freeTierReason = reason
		m.lastErr = reason
		m.lastCheck = time.Now()
		// A refusal is fresher than the 30-minute admission, so the exit leaves
		// the front tier immediately. The score also steps down, which is what
		// stops a once-good exit from being retried forever after the gate has
		// turned on it: resin-slot-199 was admitted 126 times hours earlier,
		// still scored 16, and was then refused on every retry. Three refusals
		// walk a capped score back under the "proven" line; a later admission
		// walks it forward again. Exits the gate has never admitted are not
		// scored — there is nothing to revoke.
		if m.gatePasses > 0 || !m.gatePassUntil.IsZero() || m.gateScore != 0 {
			m.gatePassUntil = time.Time{}
			m.gateScore -= gateScoreDrop
			if m.gateScore < gateScoreMin {
				m.gateScore = gateScoreMin
			}
		}
		p.log.Info("egress blocked for free-tier refusal",
			"exit", id, "for", d.String(), "until", until.Format(time.RFC3339),
			"score", m.gateScore, "reason", reason)
		p.markDirty()
		return true
	}
	return false
}

func (p *Pool) freeTierBlockedLocked(m *member, now time.Time) bool {
	return !m.freeTierUntil.IsZero() && now.Before(m.freeTierUntil)
}

// FreeTierBlocked reports whether an exit is parked against free-tier traffic.
func (p *Pool) FreeTierBlocked(id string) bool {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return p.freeTierBlockedLocked(m, time.Now())
}

// ClearFreeTier drops a free-tier block after a real upstream 2xx. Do not call
// this from MarkSuccess: that helper currently runs for every HTTP response,
// including FreeTier 403s, and would race the block just being recorded.
func (p *Pool) ClearFreeTier(id string) {
	if id == "" {
		return
	}
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	m.freeTierUntil = time.Time{}
	m.freeTierReason = ""
	m.mu.Unlock()
	p.markDirty()
}

// gatePassTTL is how long one admission keeps an exit at the front of the queue.
//
// The admission set is small and sticky, but not eternal: a clean IP can be
// flagged later (resin-slot-386 was admitted 13 times and refused 4 in the same
// three hours), and a flagged one can come back. Thirty minutes covers a
// working session — long enough that the pool is not re-discovering the same
// six exits every rotation — while still re-measuring within an episode. A
// refusal shortens it immediately anyway: BlockFreeTier parks the exit for
// free_tier.block_duration, and a parked exit is not picked at all.
const gatePassTTL = 30 * time.Minute

// gateScore bookkeeping: one admission is worth less than one drop, because the
// two are not symmetric evidence. An admission says "the gate let this exit in
// and the request completed"; a drop says "the gate let it in and the stream
// died under the client". The pool can recover from a missed admission (the next
// pick costs one attempt) but a drop costs the client a visible stall.
const (
	gateScorePass = 2
	gateScoreDrop = 3
	gateScoreMax  = 16
	gateScoreMin  = -8
	// gateGoodScore is the "this exit has carried requests" line: four net
	// admissions. It sets both the top weighting bucket and the bar for retrying
	// an exit whose admission TTL lapsed.
	gateGoodScore = 8
)

// gateWeight is how many times an admitted exit's ID appears in the pass-tier
// candidate list, i.e. its share of picks among the admitted exits.
//
// Three shares, not a continuous weight: with four to six admitted exits a finer
// gradient buys nothing measurable, while three buckets are easy to reason about
// in an incident ("is this exit good, neutral, or bad right now"). A fresh
// admission with no history is neutral rather than excluded — otherwise a newly
// discovered exit could never be measured.
func gateWeight(score int) int {
	switch {
	case score >= gateGoodScore:
		return 3
	case score >= 0:
		return 2
	default:
		return 1
	}
}

// probeQuarantineReason reports a failure recorded by the health probe. The
// probe never speaks the free-tier gate, so it is not evidence that an exit
// the gate has admitted cannot carry a request. A real forward failure
// (proxyconnect, a folded stream, an upstream Post error) is that evidence.
func probeQuarantineReason(reason string) bool {
	lower := strings.ToLower(strings.TrimSpace(reason))
	return strings.HasPrefix(lower, "unreachable:") || strings.HasPrefix(lower, "tls:")
}

// RequestSick reports that this exit's latest failure was a real request, not
// a probe, and has not been cleared by a success since. Gated picks leave such
// an exit out of every front tier until that happens.
func (p *Pool) RequestSick(id string) bool {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.consecFails > 0 && !probeQuarantineReason(m.lastErr)
}

// MarkGateDrop records that an admitted exit failed to carry the request
// through: the upstream stream ended without a completed response, or a
// transport failure hit this exit on a gated request.
//
// This is the counterweight to MarkGatePass, and it is the one that keeps the
// pass tier from becoming self-defeating: without it every admitted exit looks
// equally good forever, and the pool keeps rotating the client onto the tunnel
// that kills one stream in three.
func (p *Pool) MarkGateDrop(id, reason string) {
	if id == "" {
		return
	}
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	m.mu.Lock()
	if m.retired {
		m.mu.Unlock()
		return
	}
	// Only exits the gate has ever admitted are ranked: a drop on an exit outside
	// the admission set says nothing about a route the pool would take, and
	// letting those drift to the score floor would make the panel's column
	// meaningless for 1,600 exits that never carry a free request.
	if m.gatePassUntil.IsZero() {
		m.mu.Unlock()
		return
	}
	m.gateDrops++
	m.gateScore -= gateScoreDrop
	if m.gateScore < gateScoreMin {
		m.gateScore = gateScoreMin
	}
	score, drops := m.gateScore, m.gateDrops
	m.mu.Unlock()
	p.markDirty()
	p.log.Debug("free-tier admitted exit dropped the request", "exit", id,
		"reason", reason, "score", score, "drops", drops)
}

// MarkGatePass records that the free-tier gate admitted this exit for an
// enveloped request. It is the pool's only positive gate signal — see
// member.gatePassUntil for the measurement that makes it worth having.
func (p *Pool) MarkGatePass(id string) {
	if id == "" {
		return
	}
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return
	}
	now := time.Now()
	m.mu.Lock()
	if m.retired {
		m.mu.Unlock()
		return
	}
	m.gatePasses++
	m.gateScore += gateScorePass
	if m.gateScore > gateScoreMax {
		m.gateScore = gateScoreMax
	}
	m.gatePassUntil = now.Add(gatePassTTL)
	passes, score := m.gatePasses, m.gateScore
	m.mu.Unlock()
	p.markDirty()
	// Logged at Debug: this fires on every successful free-tier request, and at
	// Info it would out-shout the refusals that need attention.
	p.log.Debug("free-tier gate admitted this exit", "exit", id,
		"passes", passes, "score", score, "until", now.Add(gatePassTTL).Format(time.RFC3339))
}

// GatePassing reports whether id is currently a gate passer, and its lifetime
// admission count. Used by tests and the health panel; the pick path reads the
// field directly under the member lock.
func (p *Pool) GatePassing(id string) (bool, int) {
	p.mu.RLock()
	m, ok := p.byID[id]
	p.mu.RUnlock()
	if !ok {
		return false, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.gatePassUntil.IsZero() && time.Now().Before(m.gatePassUntil), m.gatePasses
}

// GatePassers lists the exits currently admitted by the free-tier gate, most
// recently admitted first would be nicer but the pool keeps no ordering — the
// panel only needs to show WHICH exits the gate is serving, so it stays a set.
func (p *Pool) GatePassers() []string {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, 8)
	for _, m := range p.members {
		m.mu.Lock()
		passing := !m.gatePassUntil.IsZero() && now.Before(m.gatePassUntil)
		m.mu.Unlock()
		if passing {
			out = append(out, m.ID)
		}
	}
	return out
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
		p.markDirty()
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

// HasUncooledExit reports whether any non-retired, non-quarantined, healthy
// exit is currently outside a quota cooldown and not region-blocked for model.
// When this is false, every send will burn a 429 rotation; callers should fail
// fast with Retry-After instead of looping maxAttempts.
func (p *Pool) HasUncooledExit(model string) bool {
	now := time.Now()
	model = strings.TrimSpace(model)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, m := range p.members {
		m.mu.Lock()
		ret := m.retired
		q := now.Before(m.quarantineUntil)
		_, _, cool := p.quotaCooledLocked(m, now)
		ft := p.freeTierBlockedLocked(m, now)
		reg := p.regionBlockedLocked(m, model, now)
		m.mu.Unlock()
		// Degraded (unhealthy/slow) still counts: PickFor would use them before
		// falling back to cooled exits. Only retired/quarantined/cooled/
		// free-tier/region blocks are true dead ends.
		if ret || q || cool || ft || reg {
			continue
		}
		return true
	}
	return false
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

// BlockRegion records that an exit's country is refused for one model.
//
// Scoped to the model on purpose. Measured: slot-10 answered 403 RegionError for
// one free model and 200 for another, in the same second, through the same exit.
// A per-exit region flag would therefore be wrong in the expensive direction —
// it would retire a working IP for models it serves fine.
//
// An empty model name is refused rather than stored as a wildcard. There is no
// such thing as "blocked for all models" in the evidence, and inventing one
// would silently become a per-exit block through a different door.
func (p *Pool) BlockRegion(exitID, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	p.mu.RLock()
	m, ok := p.byID[exitID]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.regionBlocked == nil {
		m.regionBlocked = map[string]time.Time{}
	}
	m.regionBlocked[model] = time.Now()
	return true
}

// RegionBlocked reports whether an exit is currently refused for a model.
//
// An empty model means "cannot judge" and always answers false: the forward
// ingress never sees the model, and guessing there would take exits out of
// rotation on no evidence at all.
func (p *Pool) RegionBlocked(exitID, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	p.mu.RLock()
	m, ok := p.byID[exitID]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return p.regionBlockedLocked(m, model, time.Now())
}

// regionBlockedLocked reads one member's region blocks with its lock held.
func (p *Pool) regionBlockedLocked(m *member, model string, now time.Time) bool {
	if model == "" || m.regionBlocked == nil {
		return false
	}
	at, ok := m.regionBlocked[model]
	if !ok {
		return false
	}
	// Expiry is evaluated on read rather than swept, so there is no cleanup pass
	// to forget to run — the same reason quota cooldowns work this way.
	if now.Sub(at) >= regionBlockTTL {
		return false
	}
	return true
}

// regionBlockedModelsLocked lists the models still blocked on one member.
func (p *Pool) regionBlockedModelsLocked(m *member, now time.Time) []string {
	if m.regionBlocked == nil {
		return nil
	}
	var out []string
	for model, at := range m.regionBlocked {
		if now.Sub(at) < regionBlockTTL {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

// ClearRegionBlocks forgets every region block on one exit and reports how many
// were live. Nothing in the request path calls it; it exists because a country
// reopening is not something the proxy can observe, only an operator can.
func (p *Pool) ClearRegionBlocks(exitID string) int {
	p.mu.RLock()
	m, ok := p.byID[exitID]
	p.mu.RUnlock()
	if !ok {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(p.regionBlockedModelsLocked(m, time.Now()))
	m.regionBlocked = nil
	return n
}

// RegionBlockTTL exposes how long a region refusal is trusted, so callers can
// report it without restating the constant and drifting from it.
func (p *Pool) RegionBlockTTL() time.Duration { return regionBlockTTL }

// RegionBlock is one exit-and-model pair the upstream refuses on geography.
type RegionBlock struct {
	ExitID     string    `json:"exit_id"`
	Model      string    `json:"model"`
	BlockedAt  time.Time `json:"blocked_at"`
	RetryAfter string    `json:"recheck_after,omitempty"`
}

// RegionBlocks lists every live region block, so an operator can answer "is this
// model unavailable everywhere, or only from some exits" without probing by
// hand.
func (p *Pool) RegionBlocks() []RegionBlock {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []RegionBlock
	for _, m := range p.members {
		m.mu.Lock()
		id := m.ID
		models := p.regionBlockedModelsLocked(m, now)
		at := make(map[string]time.Time, len(models))
		for _, model := range models {
			at[model] = m.regionBlocked[model]
		}
		m.mu.Unlock()
		for _, model := range models {
			left := regionBlockTTL - now.Sub(at[model])
			out = append(out, RegionBlock{
				ExitID:     id,
				Model:      model,
				BlockedAt:  at[model],
				RetryAfter: left.Round(time.Second).String(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ExitID < out[j].ExitID
	})
	return out
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

// PanelPoolStats is the summary card. It is collected in the same walk as
// Health so the status call does not traverse the pool once per number.
type PanelPoolStats struct {
	Healthy     int
	Watching    int
	Clear       int
	Unmeasured  int
	Retired     int
	Slow        int
	Gate        int
	Quarantine  QuarantineBreakdown
	Fingerprint string
}

// Health snapshots the pool for the admin API.
func (p *Pool) Health() []model.ProxyHealth {
	rows, _ := p.collect()
	return rows
}

// PanelSnapshot returns the exit rows and the summary from one walk.
func (p *Pool) PanelSnapshot() ([]model.ProxyHealth, PanelPoolStats) {
	rows, tally := p.collect()
	st := PanelPoolStats{
		Healthy:    tally.healthy,
		Watching:   tally.watching,
		Clear:      tally.clear,
		Unmeasured: tally.unmeasured,
		Retired:    tally.retired,
		Slow:       tally.slow,
		Gate:       tally.gate,
		Quarantine: tally.q,
	}
	st.Fingerprint = hashHealth(rows)
	return rows, st
}

type panelTally struct {
	healthy, watching, clear, unmeasured, retired, slow, gate int
	q                                                         QuarantineBreakdown
}

func (p *Pool) collect() ([]model.ProxyHealth, panelTally) {
	peer, peerOK := p.peerExitRetired()
	var tally panelTally
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
			ExitIPNote:  m.exitIPNote,
			LatencyMS:   m.latencyMS,
			Failures:    m.failures,
			SoftFails:   m.softFails,
			ConsecFails: m.consecFails,
			LastError:   model.PanelReason(m.lastErr),
			Slow: p.health.MaxLatency > 0 && m.healthy && !m.retired &&
				!time.Now().Before(m.quarantineUntil) && m.latencyMS > 0 &&
				time.Duration(m.latencyMS)*time.Millisecond > p.health.MaxLatency,
			// Only the live ones: an expired admission is history, and the
			// panel exists to answer "who can serve free models NOW".
			GatePasses: m.gatePasses,
			GateScore:  m.gateScore,
			GateDrops:  m.gateDrops,
		}
		if !m.gatePassUntil.IsZero() && time.Now().Before(m.gatePassUntil) {
			t := m.gatePassUntil
			h.GatePassUntil = &t
		}
		if !m.quarantineUntil.IsZero() && time.Now().Before(m.quarantineUntil) {
			t := m.quarantineUntil
			h.QuarantinedTill = &t
			h.QuarantineClass = m.quarantineClass
		}
		if until, reason, cool := p.quotaCooledLocked(m, time.Now()); cool {
			t := until
			h.QuotaUntil = &t
			h.QuotaReason = reason
			h.QuotaHits = m.quotaHits
		}
		// Listed per exit as model names, because that is the half an operator
		// cannot guess: the exit is healthy and answering, and only the pairing
		// explains the 403.
		h.RegionBlockedFor = p.regionBlockedModelsLocked(m, time.Now())
		if !m.lastCheck.IsZero() {
			t := m.lastCheck
			h.LastCheck = &t
		}
		for a := range m.boundAccounts {
			h.BoundAccounts = append(h.BoundAccounts, a)
		}
		sort.Strings(h.BoundAccounts)
		now := time.Now()
		switch {
		case m.retired:
			tally.retired++
		case now.Before(m.quarantineUntil):
			tally.q.Total++
			class := m.quarantineClass
			if class == "" {
				class = classifyQuarantine(m.lastErr)
			}
			switch class {
			case "timeout":
				tally.q.Timeout++
			case "unreachable":
				tally.q.Unreachable++
			case "tls":
				tally.q.TLS++
			default:
				tally.q.Other++
			}
		case m.healthy:
			tally.healthy++
			watch := m.consecFails > 0
			if watch {
				tally.watching++
			}
			if h.Slow {
				tally.slow++
			}
			if !watch && !h.Slow {
				if m.latencyMS > 0 {
					tally.clear++
				} else {
					tally.unmeasured++
				}
			}
		}
		if h.GatePassUntil != nil {
			tally.gate++
		}
		h.Retired = m.retired
		h.RetiredReason = model.PanelRetireReason(m.retiredReason)
		if !m.retiredAt.IsZero() {
			t := m.retiredAt
			h.RetiredAt = &t
		}
		h.Prunable = p.pruneWorthyLocked(m)
		if peerOK {
			note, listed := peer[m.ID]
			if !m.retired && note.Retired {
				h.PeerRetired = true
				h.PeerRetiredClass = note.Class
			}
			if m.retired && listed && !note.Retired {
				h.PeerKeeps = true
			}
			peerCool := listed && note.Quota
			if h.QuotaUntil == nil && peerCool {
				h.PeerQuota = true
			}
			if h.QuotaUntil != nil && !peerCool {
				h.PeerQuotaHere = true
			}
		}
		m.mu.Unlock()
		out = append(out, h)
	}
	return out, tally
}

// ExitTableFingerprint is a short hash of the exit table the panel draws.
// LastCheck is left out: a probe that did not change a visible cell must not
// make the browser download the table again. Bound account names are sorted
// in Health, so a map walk cannot change the hash by itself.
func (p *Pool) ExitTableFingerprint() string {
	if p == nil {
		return ""
	}
	return hashHealth(p.Health())
}

func hashHealth(rows []model.ProxyHealth) string {
	for i := range rows {
		rows[i].LastCheck = nil
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
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
	p.markDirty()
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
	m.poisonConsec = 0
	m.quarantineUntil = time.Time{}
	m.quarantineClass = ""
	m.healthy = true
	m.lastErr = ""
	p.log.Info("egress revived", "exit", id)
	p.markDirty()
	return nil
}

// Prune retires every exit that is currently unusable — quarantined, marked
// unhealthy, or over the latency ceiling — and reports exactly what it did.
//
// It does not spare the last exit: an operator who asks to clear the garbage
// should be told the pool is now empty, not have one piece of garbage quietly
// kept on their behalf. Traffic keeps flowing anyway through Pick's
// least-bad-retired fallback.
func (p *Pool) Prune(reason string, force bool) model.PruneReport {
	rep := model.PruneReport{}
	if !force && p.OutageActive() {
		// Pruning during a breaker-open outage would execute the whole
		// quarantined pool for a correlated failure — the exact massacre the
		// breaker exists to prevent. Refuse unless the operator insists.
		rep.Refused = "outage circuit breaker is open; retirement is suspended pool-wide (pass force=1 to override)"
		p.log.Warn("prune refused while outage breaker is open", "hint", "pass force=1 to override")
		return rep
	}
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
		p.markDirty()
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

// WatchCount reports usable exits whose last checks already failed, but not
// enough times to start a quarantine. The summary card must not call these
// simply healthy: the row labels them 观察.
func (p *Pool) WatchCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, m := range p.members {
		m.mu.Lock()
		watch := m.healthy && !m.retired && !now.Before(m.quarantineUntil) && m.consecFails > 0
		m.mu.Unlock()
		if watch {
			n++
		}
	}
	return n
}

// QuarantineBreakdown counts exits parked right now, by the kind of failure
// that parked them. The strings are categories, never the raw dial error.
type QuarantineBreakdown struct {
	Total       int `json:"total"`
	Timeout     int `json:"timeout"`
	Unreachable int `json:"unreachable"`
	TLS         int `json:"tls"`
	Other       int `json:"other"`
}

// QuarantineBreakdown reports the exits that are in a nap at this moment.
func (p *Pool) QuarantineBreakdown() QuarantineBreakdown {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var out QuarantineBreakdown
	for _, m := range p.members {
		m.mu.Lock()
		parked := !m.retired && now.Before(m.quarantineUntil)
		class := m.quarantineClass
		if class == "" {
			class = classifyQuarantine(m.lastErr)
		}
		m.mu.Unlock()
		if !parked {
			continue
		}
		out.Total++
		switch class {
		case "timeout":
			out.Timeout++
		case "unreachable":
			out.Unreachable++
		case "tls":
			out.TLS++
		default:
			out.Other++
		}
	}
	return out
}

// SlowCount reports healthy exits whose last measurement is over the latency
// ceiling. They stay in rotation, behind the faster ones. A ceiling of zero
// means the operator did not ask for this distinction.
func (p *Pool) SlowCount() int {
	if p.health.MaxLatency <= 0 {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, m := range p.members {
		m.mu.Lock()
		slow := m.healthy && !m.retired && !now.Before(m.quarantineUntil) &&
			m.latencyMS > 0 && time.Duration(m.latencyMS)*time.Millisecond > p.health.MaxLatency
		m.mu.Unlock()
		if slow {
			n++
		}
	}
	return n
}

// classifyQuarantine folds a dial error into one of four panel words.
// Order matches a live count on 2026-10-21: certificate and "tls:" first,
// then timeouts, then connect failures. Anything else stays "other" so a new
// failure shape shows up instead of being filed under a neighbour.
func classifyQuarantine(reason string) string {
	low := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case strings.Contains(low, "certificate") || strings.Contains(low, "x509") || strings.Contains(low, "tls:"):
		return "tls"
	case strings.Contains(low, "timeout") || strings.Contains(low, "deadline"):
		return "timeout"
	case strings.Contains(low, "proxyconnect") || strings.Contains(low, "connection refused") ||
		strings.Contains(low, "no such host") || strings.HasPrefix(low, "unreachable") ||
		strings.Contains(low, "socks"):
		return "unreachable"
	default:
		return "other"
	}
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
// It also runs the debounced exit-state flusher (persist.go) on the same
// lifecycle.
func (p *Pool) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	saveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.saveLoop(saveCtx, 30*time.Second)
	p.ProbeAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel() // final flush of any dirty state
			return
		case <-t.C:
			p.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every exit that is still in rotation, under a bounded
// concurrency. Retired exits are skipped: they are out of the pool, so
// spending probe budget on them would be paying rent on an empty room.
//
// The bound matters at pool sizes in the thousands. Unbounded, every tick
// opened one CONNECT+TLS per exit at the same instant — measured against a
// provider that fronts all its exits from a handful of POPs, 3544 concurrent
// probes every 2 minutes tripped the provider's connection quota
// ("CONNECT reply: 429 Not Enough Connections"), which then also starved live
// traffic and quarantined healthy exits in a cascade. The cap (and a longer
// interval, set together) turns the probe round into a gentle trickle.
//
// The cap is a per-round gate, not a global throttle: one round finishes
// before the ticker fires the next only because the interval is expected to
// exceed round time at these sizes.
func (p *Pool) ProbeAll(ctx context.Context) {
	var wg sync.WaitGroup
	cap := p.health.ProbeConcurrency
	if cap <= 0 {
		cap = 64
	}
	sem := make(chan struct{}, cap)
	for _, m := range p.members {
		m.mu.Lock()
		ret := m.retired
		m.mu.Unlock()
		if ret {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Add(1)
		go func(m *member) {
			defer wg.Done()
			defer func() { <-sem }()
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
	// Reachability: a TCP tunnel is necessary but not sufficient. Measured,
	// MITM resin slots complete CONNECT/SOCKS to opencode.ai:443 and still
	// fail the real HTTPS request with x509 name mismatches. Doing the TLS
	// handshake here (with certificate verification) is what stops those
	// slots from being marked healthy by a probe that never spoke TLS.
	if p.health.Reachability != "" {
		conn, err := m.DialContext(pctx, "tcp", p.health.Reachability)
		if err != nil {
			p.MarkFailure(m.ID, "unreachable: "+err.Error())
			return
		}
		host, _, splitErr := net.SplitHostPort(p.health.Reachability)
		if splitErr != nil || host == "" {
			host = p.health.Reachability
		}
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(pctx); err != nil {
			_ = conn.Close()
			msg := err.Error()
			// Handshake errors from crypto/tls already start with "tls:".
			if !strings.HasPrefix(strings.ToLower(msg), "tls:") {
				msg = "tls: " + msg
			}
			p.MarkFailure(m.ID, msg)
			return
		}
		_ = tlsConn.Close()
	}
	latency := time.Since(start)

	exitIP := ""
	if p.health.ProbeURL != "" {
		if ip, err := p.fetchExitIP(pctx, m); err != nil {
			// A failed IP probe is informative, not fatal: the tunnels that
			// matter already proved themselves above. Keep it off lastErr,
			// which the panel reads as "this exit is broken".
			m.mu.Lock()
			m.exitIPNote = exitIPNote(err)
			m.lastErr = ""
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
	// Keep poisonConsec: a probe that happens to pass TLS must not wipe the
	// MITM strike count, or intermittent poison slots never reach retirement.
	m.quarantineUntil = time.Time{}
	m.quarantineClass = ""
	if exitIP != "" {
		m.lastErr = ""
		m.exitIPNote = ""
	}
	m.lastCheck = time.Now()
	m.mu.Unlock()
	// A probe that cleared reachability is the strongest life signal the pool
	// sees — it runs even when request traffic is absent — so it feeds the
	// breaker directly: recovery is detected at probe cadence, not request
	// cadence.
	p.noteSuccessOutcome()
}

// exitIPNote reduces an address-lookup failure to a fixed word. The panel
// says the tunnel is fine and the lookup was not; it never sees the raw error.
func exitIPNote(err error) string {
	msg := strings.TrimSpace(err.Error())
	if rest, ok := strings.CutPrefix(msg, "status "); ok {
		if n, conv := strconv.Atoi(rest); conv == nil && n >= 100 && n <= 599 {
			return "status " + rest
		}
		return "status"
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "canceled"):
		return "canceled"
	case strings.Contains(low, "deadline") || strings.Contains(low, "timeout"):
		return "timeout"
	default:
		return "failed"
	}
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
