package proxypool

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// socks5Fixture is a minimal RFC 1928/1929 server. The SOCKS5 client in this
// package is hand-rolled, so it is the riskiest code here and deserves a real
// peer rather than a mock interface.
type socks5Fixture struct {
	addr string
	user string
	pass string
}

func newSocks5Fixture(t *testing.T) *socks5Fixture {
	t.Helper()
	f := &socks5Fixture{user: "u", pass: "p"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f.addr = ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *socks5Fixture) serve(c net.Conn) {
	defer c.Close()

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(hdr[1]))); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 2}); err != nil { // demand user/pass
		return
	}

	ah := make([]byte, 2)
	if _, err := io.ReadFull(c, ah); err != nil {
		return
	}
	user := make([]byte, int(ah[1]))
	if _, err := io.ReadFull(c, user); err != nil {
		return
	}
	pl := make([]byte, 1)
	if _, err := io.ReadFull(c, pl); err != nil {
		return
	}
	pass := make([]byte, int(pl[0]))
	if _, err := io.ReadFull(c, pass); err != nil {
		return
	}
	if string(user) != f.user || string(pass) != f.pass {
		_, _ = c.Write([]byte{1, 1})
		return
	}
	if _, err := c.Write([]byte{1, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		_, _ = c.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(pb))

	if req[1] != 1 { // CONNECT only
		_, _ = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go func() { _, _ = io.Copy(up, c) }()
	_, _ = io.Copy(c, up)
}

// deadAddr returns an address nothing is listening on, so a dial fails fast
// and deterministically (port 1 can be filtered rather than refused on some
// hosts).
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestSOCKS5ReachesUpstreamThroughFixture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("through-the-tunnel"))
	}))
	defer upstream.Close()

	fx := newSocks5Fixture(t)
	px, err := New("s1", "socks5h://u:p@"+fx.addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := &http.Client{Transport: px.Transport(), Timeout: 10 * time.Second}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET through socks5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-the-tunnel" {
		t.Fatalf("body = %q", body)
	}
}

func TestSOCKS5RejectsWrongPassword(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	fx := newSocks5Fixture(t)
	px, err := New("s1", "socks5h://u:wrong@"+fx.addr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client := &http.Client{Transport: px.Transport(), Timeout: 10 * time.Second}
	_, err = client.Get(upstream.URL)
	if err == nil {
		t.Fatal("expected an authentication failure")
	}
	if !strings.Contains(err.Error(), "authentication rejected") {
		t.Fatalf("error = %v, want an authentication rejection", err)
	}
}

func TestNewRejectsUnsupportedScheme(t *testing.T) {
	if _, err := New("x", "ftp://1.2.3.4:21"); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
	if _, err := New("x", "socks5://"); err == nil {
		t.Fatal("expected an error for a missing host")
	}
}

func TestRedactedHidesCredentials(t *testing.T) {
	px, err := New("s1", "socks5h://user:supersecret@10.0.0.1:1080")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := px.Redacted()
	if strings.Contains(got, "supersecret") {
		t.Fatalf("Redacted leaked the password: %s", got)
	}
	if !strings.Contains(got, "user:***@10.0.0.1:1080") {
		t.Fatalf("Redacted = %s", got)
	}
}

func healthyPool(t *testing.T, n int) (*Pool, *config.Health) {
	t.Helper()
	hc := config.Health{
		Timeout:          time.Second,
		FailureThreshold: 2,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}
	specs := make([]Spec, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, Spec{
			ID:  string(rune('a' + i)),
			URL: "http://127.0.0.1:9", // never dialled in these tests
		})
	}
	pool, err := NewPool(specs, hc, testLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool, &hc
}

func TestPickSkipsExcludedExit(t *testing.T) {
	pool, _ := healthyPool(t, 3)
	for i := 0; i < 30; i++ {
		if got := pool.Pick("a"); got == nil || got.ID == "a" {
			t.Fatalf("Pick returned the excluded exit: %v", got)
		}
	}
}

func TestMarkFailureQuarantinesAfterThreshold(t *testing.T) {
	pool, _ := healthyPool(t, 3)

	pool.MarkFailure("b", "boom")
	if ok, known := pool.Healthy("b"); !known || !ok {
		t.Fatal("one failure must not quarantine when the threshold is 2")
	}
	pool.MarkFailure("b", "boom again")
	if ok, _ := pool.Healthy("b"); ok {
		t.Fatal("two failures must quarantine at threshold 2")
	}
	if n := pool.HealthyCount(); n != 2 {
		t.Fatalf("HealthyCount = %d, want 2", n)
	}
	for i := 0; i < 30; i++ {
		if got := pool.Pick(""); got != nil && got.ID == "b" {
			t.Fatal("Pick handed out a quarantined exit while healthy ones existed")
		}
	}
}

func TestMarkSuccessClearsQuarantine(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.MarkFailure("a", "x")
	pool.MarkFailure("a", "x")
	pool.MarkSuccess("a", 12*time.Millisecond)
	if ok, _ := pool.Healthy("a"); !ok {
		t.Fatal("MarkSuccess must clear the quarantine")
	}
	h := pool.Health()
	var found bool
	for _, e := range h {
		if e.ID == "a" {
			found = true
			if e.ConsecFails != 0 {
				t.Fatalf("consecutive failures not reset: %+v", e)
			}
			if !e.Healthy {
				t.Fatalf("exit a should be healthy again: %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("exit a missing from Health()")
	}
}

func TestPoolFallsBackToQuarantinedWhenNothingElseRemains(t *testing.T) {
	pool, _ := healthyPool(t, 1)
	pool.MarkFailure("a", "x")
	pool.MarkFailure("a", "x")
	if ok, _ := pool.Healthy("a"); ok {
		t.Fatal("precondition: exit should be quarantined")
	}
	if got := pool.Pick(""); got == nil {
		t.Fatal("a single-exit pool must still limp rather than fail outright")
	}
}

func TestSpecsFromConfigExpandsResin(t *testing.T) {
	specs := SpecsFromConfig(config.Proxies{
		Resin: config.Resin{
			Enabled:  true,
			Scheme:   "socks5h",
			Host:     "127.0.0.1:2260",
			Platform: "Default",
			Token:    "tok",
			Accounts: []string{"slot-01", "slot-02"},
		},
		Static: []string{"http://1.2.3.4:8080"},
	})
	if len(specs) != 3 {
		t.Fatalf("got %d specs, want 3", len(specs))
	}
	if specs[0].ID != "resin-slot-01" {
		t.Fatalf("specs[0].ID = %q", specs[0].ID)
	}
	// Resin's sticky identity format is Platform.Account:token@host
	if !strings.Contains(specs[0].URL, "Default.slot-01:tok@127.0.0.1:2260") {
		t.Fatalf("resin url = %q", specs[0].URL)
	}
	if specs[2].ID != "static-01" {
		t.Fatalf("specs[2].ID = %q", specs[2].ID)
	}
}

// A pool built from unreliable nodes contains exits that work but take tens
// of seconds. Those must be held in reserve rather than handed out while
// better options exist.
func TestPickDemotesSlowButAliveExits(t *testing.T) {
	specs := []Spec{
		{ID: "a", URL: "http://127.0.0.1:9"},
		{ID: "b", URL: "http://127.0.0.1:9"},
		{ID: "c", URL: "http://127.0.0.1:9"},
	}
	pool, err := NewPool(specs, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 3,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
		MaxLatency:       2 * time.Second,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	pool.MarkSuccess("b", 20*time.Second)
	for i := 0; i < 30; i++ {
		if got := pool.Pick(""); got == nil || got.ID == "b" {
			t.Fatalf("a 20s exit was handed out while faster ones existed: %v", got)
		}
	}

	// Demoted, not discarded: with nothing else left it must still be used.
	pool.MarkSuccess("a", 30*time.Second)
	pool.MarkSuccess("c", 30*time.Second)
	if got := pool.Pick(""); got == nil {
		t.Fatal("a pool of only-slow exits must still produce an exit")
	}
	if ok, _ := pool.Healthy("b"); !ok {
		t.Error("the latency ceiling must not mark an exit unhealthy")
	}
}

// Without a ceiling configured, behaviour is unchanged.
func TestPickIgnoresLatencyWhenNoCeilingIsSet(t *testing.T) {
	pool, _ := healthyPool(t, 2)
	pool.MarkSuccess("a", 30*time.Second)
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		seen[pool.Pick("").ID] = true
	}
	if !seen["a"] {
		t.Fatal("with max_latency unset, a slow exit must still take its turn")
	}
}

func TestParseExitIPAcceptsBothShapes(t *testing.T) {
	if got := parseExitIP([]byte(`{"ip":"203.0.113.7"}`)); got != "203.0.113.7" {
		t.Fatalf("json form: %q", got)
	}
	if got := parseExitIP([]byte("203.0.113.8\n")); got != "203.0.113.8" {
		t.Fatalf("plain form: %q", got)
	}
	if got := parseExitIP([]byte("2001:db8::1")); got != "2001:db8::1" {
		t.Fatalf("ipv6 form: %q", got)
	}
	// A probe endpoint that answers with anything other than an address must
	// not have its body recorded as an "exit IP" and shown to the operator.
	for _, bad := range []string{
		`{"path":"/","auth":null,"bodyBytes":0}`,
		`{"ip":"not-an-ip"}`,
		`<html>gateway error</html>`,
		``,
	} {
		if got := parseExitIP([]byte(bad)); got != "" {
			t.Errorf("parseExitIP(%q) = %q, want empty", bad, got)
		}
	}
}

// retirePool builds a pool whose exits are never dialled, with retirement
// enabled.
func retirePool(t *testing.T, n, retireAfter int) *Pool {
	t.Helper()
	specs := make([]Spec, 0, n)
	for i := 0; i < n; i++ {
		specs = append(specs, Spec{
			ID:  string(rune('a' + i)),
			URL: "http://127.0.0.1:9",
		})
	}
	pool, err := NewPool(specs, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 2,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
		RetireAfter:      retireAfter,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool
}

func healthOf(t *testing.T, pool *Pool, id string) model.ProxyHealth {
	t.Helper()
	for _, h := range pool.Health() {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("exit %q not found in the pool", id)
	return model.ProxyHealth{}
}

func TestMarkFailureRetiresAfterTheConfiguredStreak(t *testing.T) {
	pool := retirePool(t, 2, 4)
	for i := 0; i < 3; i++ {
		pool.MarkFailure("a", "dial: refused")
	}
	if h := healthOf(t, pool, "a"); h.Retired {
		t.Fatalf("retired after only 3 of 4 failures: %+v", h)
	}
	pool.MarkFailure("a", "dial: refused")
	h := healthOf(t, pool, "a")
	if !h.Retired {
		t.Fatalf("not retired after 4 consecutive failures: %+v", h)
	}
	if h.RetiredReason == "" {
		t.Error("a retired exit must record why it was retired")
	}
	if h.Prunable {
		t.Error("an already-retired exit must not also be advertised as prunable")
	}
}

func TestOneSuccessResetsTheRetirementStreak(t *testing.T) {
	pool := retirePool(t, 2, 3)
	// A flapping exit that still answers now and then is not garbage.
	for round := 0; round < 5; round++ {
		pool.MarkFailure("a", "dial: refused")
		pool.MarkFailure("a", "dial: refused")
		pool.MarkSuccess("a", 10*time.Millisecond)
	}
	if h := healthOf(t, pool, "a"); h.Retired {
		t.Fatalf("an exit that keeps succeeding was retired: %+v", h)
	}
}

func TestRetirementIsDisabledByZero(t *testing.T) {
	pool := retirePool(t, 2, 0)
	for i := 0; i < 50; i++ {
		pool.MarkFailure("a", "dial: refused")
	}
	if h := healthOf(t, pool, "a"); h.Retired {
		t.Fatalf("retire_after: 0 must disable retirement entirely: %+v", h)
	}
}

// A retired exit is out of rotation, full stop — quarantine expiry, a
// successful probe and a restart all leave it retired.
func TestRetiredExitIsNeverPickedWhileOthersExist(t *testing.T) {
	pool := retirePool(t, 2, 1)
	if err := pool.Retire("a", "test"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	for i := 0; i < 30; i++ {
		got := pool.Pick("")
		if got == nil {
			t.Fatal("Pick returned nil while one healthy exit remained")
		}
		if got.ID == "a" {
			t.Fatal("a retired exit was handed out")
		}
	}
	if ok, known := pool.Healthy("a"); !known || ok {
		t.Errorf("Healthy(retired) = (%v, %v), want (false, true)", ok, known)
	}
	if n := pool.HealthyCount(); n != 1 {
		t.Errorf("HealthyCount = %d, want 1", n)
	}
	if n := pool.RetiredCount(); n != 1 {
		t.Errorf("RetiredCount = %d, want 1", n)
	}
	// Even a success must not undo the operator's decision.
	pool.MarkSuccess("a", time.Millisecond)
	if ok, _ := pool.Healthy("a"); ok {
		t.Error("a successful probe silently un-retired an exit")
	}
}

// The whole pool retired must degrade, not fail: 502-ing the gateway would be
// a worse outcome than limping on the least-bad exit.
func TestRetiredExitIsUsedOnlyWhenNothingElseRemains(t *testing.T) {
	pool := retirePool(t, 3, 0)
	// Lifetime failure counts: a=2, b=5, c=3. Once everything is retired the
	// fallback must land on "a", the least-bad of them.
	pool.MarkFailure("a", "x")
	pool.MarkFailure("a", "x")
	for i := 0; i < 5; i++ {
		pool.MarkFailure("b", "x")
	}
	for i := 0; i < 3; i++ {
		pool.MarkFailure("c", "x")
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := pool.Retire(id, "test"); err != nil {
			t.Fatalf("Retire(%s): %v", id, err)
		}
	}

	got := pool.Pick("")
	if got == nil {
		t.Fatal("Pick returned nil with a fully retired pool; it must limp, not 502")
	}
	if got.ID != "a" {
		t.Errorf("Pick = %q, want the least-bad retired exit %q", got.ID, "a")
	}
}

func TestPruneRetiresUnusableExitsAndReportsThem(t *testing.T) {
	pool := retirePool(t, 3, 0)
	// "a" is fine; "b" and "c" accumulate failures past the threshold.
	pool.MarkFailure("b", "dial: refused")
	pool.MarkFailure("b", "dial: refused")
	pool.MarkFailure("c", "dial: refused")
	pool.MarkFailure("c", "dial: refused")
	pool.MarkFailure("c", "dial: refused")

	if h := healthOf(t, pool, "b"); !h.Prunable {
		t.Fatalf("an unhealthy exit was not advertised as prunable: %+v", h)
	}
	if h := healthOf(t, pool, "a"); h.Prunable {
		t.Fatalf("a healthy exit was advertised as prunable: %+v", h)
	}

	rep := pool.Prune("test")
	if len(rep.Retired) != 2 || rep.Retired[0] != "b" || rep.Retired[1] != "c" {
		t.Fatalf("Retired = %v, want [b c]", rep.Retired)
	}
	if rep.UsableAfter != 1 || rep.PoolEmpty {
		t.Fatalf("UsableAfter = %d, PoolEmpty = %v, want 1 / false", rep.UsableAfter, rep.PoolEmpty)
	}

	// Idempotent: a second pass retires nothing and reports the same state.
	rep2 := pool.Prune("test")
	if len(rep2.Retired) != 0 || len(rep2.AlreadyRetired) != 2 {
		t.Fatalf("second prune = %+v, want nothing retired and both already retired", rep2)
	}
	for i := 0; i < 10; i++ {
		if got := pool.Pick(""); got == nil || got.ID != "a" {
			t.Fatalf("Pick = %v, want the one surviving exit", got)
		}
	}
}

// Pruning the last exit is allowed, but the operator must be told the pool is
// now empty rather than discover it from the traffic.
func TestPruneReportsAnEmptiedPool(t *testing.T) {
	pool := retirePool(t, 1, 0)
	pool.MarkFailure("a", "dial: refused")
	pool.MarkFailure("a", "dial: refused")
	rep := pool.Prune("test")
	if !rep.PoolEmpty || rep.UsableAfter != 0 {
		t.Fatalf("Prune = %+v, want PoolEmpty with zero usable", rep)
	}
	if got := pool.Pick(""); got == nil {
		t.Fatal("Pick returned nil after the pool emptied; it must limp")
	}
}

func TestReviveRestoresARetiredExit(t *testing.T) {
	pool := retirePool(t, 2, 2)
	pool.MarkFailure("a", "dial: refused")
	pool.MarkFailure("a", "dial: refused")
	if h := healthOf(t, pool, "a"); !h.Retired {
		t.Fatalf("precondition failed, exit not retired: %+v", h)
	}
	if err := pool.Revive("a"); err != nil {
		t.Fatalf("Revive: %v", err)
	}
	h := healthOf(t, pool, "a")
	if h.Retired || h.RetiredReason != "" || h.ConsecFails != 0 {
		t.Fatalf("Revive left stale state: %+v", h)
	}
	// Revive grants a clean slate, so a single blip does not immediately
	// re-retire it out from under the operator.
	pool.MarkFailure("a", "dial: refused")
	if h := healthOf(t, pool, "a"); h.Retired {
		t.Fatal("a revived exit was retired again on its first failure")
	}
	if err := pool.Revive("nope"); err == nil {
		t.Error("Revive on an unknown exit must fail")
	}
	if err := pool.Retire("nope", "x"); err == nil {
		t.Error("Retire on an unknown exit must fail")
	}
}
