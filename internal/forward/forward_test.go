package forward

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// splitAccount mirrors cmd/zenproxy's router so the tests exercise the same
// path shape the real binary serves.
func splitAccount(p string) (account, rest string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i:]
	}
	return p, ""
}

// ---------------------------------------------------------------------------
// pure functions
// ---------------------------------------------------------------------------

func TestJoinUpstreamPathCollapsesDuplicateV1(t *testing.T) {
	cases := []struct {
		base, rest, want string
	}{
		// new-api appends /v1/... to whatever Base URL is configured.
		{"https://opencode.ai/zen/v1", "/v1/chat/completions", "https://opencode.ai/zen/v1/chat/completions"},
		// LiteLLM appends /chat/completions with no /v1 of its own.
		{"https://opencode.ai/zen/v1", "/chat/completions", "https://opencode.ai/zen/v1/chat/completions"},
		{"https://opencode.ai/zen/go/v1", "/v1/messages", "https://opencode.ai/zen/go/v1/messages"},
		{"https://opencode.ai/zen/v1", "", "https://opencode.ai/zen/v1"},
		{"https://opencode.ai/zen/v1", "chat/completions", "https://opencode.ai/zen/v1/chat/completions"},
		// A /v1 that is not the tail must not be collapsed.
		{"https://example.com/v1/beta", "/v1/chat/completions", "https://example.com/v1/beta/v1/chat/completions"},
	}
	for _, c := range cases {
		got, err := joinUpstreamPath(c.base, c.rest)
		if err != nil {
			t.Fatalf("joinUpstreamPath(%q,%q): %v", c.base, c.rest, err)
		}
		if got.String() != c.want {
			t.Errorf("joinUpstreamPath(%q,%q) = %q, want %q", c.base, c.rest, got, c.want)
		}
	}
}

func TestClassifyStatusSeparatesTierGateFromAuth(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   model.ErrorClass
	}{
		{403, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`, model.ClassTierGate},
		{403, "Error from provider (Console): OpenCode's free tier can only be used from within OpenCode", model.ClassTierGate},
		{403, `{"error":"invalid api key"}`, model.ClassAuth},
		{401, `{"error":"unauthorized"}`, model.ClassAuth},
		{429, `{"error":"rate limited"}`, model.ClassRateLimit},
		{407, "", model.ClassProxyAuth},
		{503, "", model.ClassServer},
		{520, "", model.ClassServer},
		{400, `{"error":"bad model"}`, model.ClassClient},
		{404, "", model.ClassClient},
	}
	for _, c := range cases {
		got := classifyStatus(c.status, http.Header{}, []byte(c.body))
		if got != c.want {
			t.Errorf("classifyStatus(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

func TestTierGateIsNeverAttributableToTheExit(t *testing.T) {
	if model.ClassTierGate.Retryable() {
		t.Error("a client-identity refusal must not be retried")
	}
	if model.ClassTierGate.IPAttributable() {
		t.Error("a client-identity refusal must not consume exits")
	}
	if !model.ClassTransport.IPAttributable() {
		t.Error("transport failures are exactly what rotation is for")
	}
}

func TestSplitAccount(t *testing.T) {
	cases := []struct{ in, account, rest string }{
		{"/zen-01/v1/chat/completions", "zen-01", "/v1/chat/completions"},
		{"/zen-01", "zen-01", ""},
		{"/", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		a, r := splitAccount(c.in)
		if a != c.account || r != c.rest {
			t.Errorf("splitAccount(%q) = (%q,%q), want (%q,%q)", c.in, a, r, c.account, c.rest)
		}
	}
}

// ---------------------------------------------------------------------------
// end-to-end harness
// ---------------------------------------------------------------------------

// mockExit is a working egress proxy: absolute-form requests are relayed and
// CONNECT is tunnelled.
type mockExit struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newMockExit(t *testing.T) *mockExit {
	t.Helper()
	m := &mockExit{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		if r.Method == http.MethodConnect {
			up, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer up.Close()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			cl, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer cl.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = rw.Flush()
			go func() { _, _ = io.Copy(up, rw) }()
			_, _ = io.Copy(cl, up)
			return
		}
		if !r.URL.IsAbs() {
			http.Error(w, "absolute URI required", http.StatusBadRequest)
			return
		}
		out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Propagate cancellation downstream, as a real proxy does: when the
		// proxy under test gives up on us, the mock upstream must find out,
		// otherwise the test's own servers never unwind.
		out = out.WithContext(r.Context())
		for k, vs := range r.Header {
			if strings.EqualFold(k, "Proxy-Authorization") {
				continue
			}
			for _, v := range vs {
				out.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Copy by hand and flush after every chunk. io.Copy would buffer the
		// whole response, which would hide whether the proxy under test
		// actually streams or merely forwards bytes eventually.
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 16<<10)
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
	}))
	t.Cleanup(m.srv.Close)
	return m
}

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

type harness struct {
	engine  *Engine
	reg     *registry.Registry
	pool    *proxypool.Pool
	upOpts  Options
	exit    *mockExit
	baseURL string           // mock upstream, plain http so the proxy path is exercised
	proxy   *httptest.Server // the engine exposed as a real HTTP proxy
}

func newHarness(t *testing.T, upstream http.Handler) *harness {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	exit := newMockExit(t)
	log := testLogger()

	pol := config.Policy{
		MaxAttempts:       3,
		RotateOnStatus:    []int{407, 429, 500, 502, 503, 504},
		RotateOnTransport: true,
		MaxBodyBytes:      1 << 20,
		StreamIdleTimeout: 2 * time.Second,
		UpstreamTimeout:   30 * time.Second,
	}
	opts := Options{
		Upstreams: map[string]model.Upstream{"zen": {BaseURL: up.URL + "/zen/v1"}},
		Policy:    pol,
		Log:       log,
		// Enabled the way the shipped default has it, with an empty model list
		// so tests exercise the reactive path: nothing is enveloped until the
		// upstream refuses the model once.
		FreeTier: config.FreeTier{Enabled: true},
	}

	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "dead", URL: "http://" + deadAddr(t)},
		{ID: "live", URL: exit.srv.URL},
	}, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	reg := registry.New("", pool, log)
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Upstream: "zen", APIKey: "sk-account-key", Proxy: "auto", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	eng := New(reg, pool, opts)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect || r.URL.IsAbs() {
			eng.ServeForward(w, r)
			return
		}
		account, rest := splitAccount(r.URL.Path)
		if account == "" {
			WriteError(w, http.StatusNotFound, "no account in path")
			return
		}
		eng.ServeReverse(w, r, account, rest)
	}))
	t.Cleanup(proxySrv.Close)

	return &harness{
		engine:  eng,
		reg:     reg,
		pool:    pool,
		upOpts:  opts,
		exit:    exit,
		baseURL: up.URL,
		proxy:   proxySrv,
	}
}

func (h *harness) post(t *testing.T, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zen-01"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-client-token")
	h.engine.ServeReverse(rec, req, "zen-01", path)
	return rec
}

func (h *harness) stats(t *testing.T) model.Stats {
	t.Helper()
	st, ok := h.reg.Stats("zen-01")
	if !ok {
		t.Fatal("no stats for zen-01")
	}
	return st
}

// ---------------------------------------------------------------------------
// reverse mode
// ---------------------------------------------------------------------------

func TestEndToEndRotatesOffDeadExitAndSucceeds(t *testing.T) {
	var seenAuth, seenPath atomic.Value
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		seenPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	// Force the account onto the dead exit so the very first attempt fails.
	if err := h.reg.Bind("zen-01", "dead"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec := h.post(t, "/v1/chat/completions", `{"model":"x","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
	if st := h.stats(t); st.Rotations != 1 {
		t.Errorf("Rotations = %d, want 1 (the dead exit must be skipped)", st.Rotations)
	}
	if st := h.stats(t); st.Success != 1 {
		t.Errorf("Success = %d, want 1", st.Success)
	}
	if got, _ := seenAuth.Load().(string); got != "Bearer sk-account-key" {
		t.Errorf("upstream saw %q; the pool key must replace the client's", got)
	}
	if got, _ := seenPath.Load().(string); got != "/zen/v1/chat/completions" {
		t.Errorf("upstream path = %q; the duplicate /v1 must be collapsed", got)
	}
	if v, _ := h.reg.View("zen-01"); v.BoundExit != "live" {
		t.Errorf("account is still bound to %q after rotation", v.BoundExit)
	}
}

func TestEndToEndTierGateIsPassedThroughWithoutRotating(t *testing.T) {
	body := `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec := h.post(t, "/v1/messages", `{"model":"x"}`)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (the upstream body must reach the client verbatim)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FreeTierError") {
		t.Fatalf("body = %q", rec.Body.String())
	}
	st := h.stats(t)
	if st.Rotations != 0 {
		t.Errorf("Rotations = %d; a client-identity refusal must not burn exits", st.Rotations)
	}
	if st.LastClass != model.ClassTierGate {
		t.Errorf("LastClass = %q, want %q", st.LastClass, model.ClassTierGate)
	}
	if st.Failures != 1 {
		t.Errorf("Failures = %d, want 1", st.Failures)
	}
	if hits := h.exit.hits.Load(); hits != 1 {
		t.Errorf("exit saw %d requests, want exactly 1 (no retry budget spent)", hits)
	}
}

func TestEndToEndRotatesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec := h.post(t, "/v1/chat/completions", `{"model":"x"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if st := h.stats(t); st.Rotations < 1 {
		t.Errorf("Rotations = %d, want at least 1", st.Rotations)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
	// A 429 says the exit carried the request fine; condemning the exit would
	// let an account-level quota drain the IP pool.
	if ok, _ := h.pool.Healthy("live"); !ok {
		t.Error("a 429 must move the binding without quarantining the exit")
	}
}

func TestEndToEndStreamsSSEChunksInOrder(t *testing.T) {
	want := "data: {\"delta\":\"he\"}\n\ndata: {\"delta\":\"llo\"}\n\ndata: [DONE]\n\n"
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"delta\":\"he\"}\n\n",
			"data: {\"delta\":\"llo\"}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			if f != nil {
				f.Flush()
			}
		}
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	rec := h.post(t, "/v1/chat/completions", `{"model":"x","stream":true}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("stream = %q, want %q", got, want)
	}
}

// Streaming has to be incremental, not merely complete: the client must see
// the first chunk while the upstream is still holding the rest. The upstream
// blocks until the test releases it, so a buffering proxy deadlocks and trips
// the handler's own timeout.
func TestEndToEndDeliversFirstChunkBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"delta\":\"first\"}\n\n")
		if f != nil {
			f.Flush()
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("the first chunk never reached the client; the proxy is buffering the stream")
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f != nil {
			f.Flush()
		}
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// A real client against a real listener, because httptest.NewRecorder
	// cannot express "arrived early".
	resp, err := http.Get(h.proxy.URL + "/zen-01/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	head := make([]byte, 64)
	n, err := resp.Body.Read(head)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(string(head[:n]), "first") {
		t.Fatalf("first chunk = %q", head[:n])
	}
	close(release) // only now may the upstream finish

	tail, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the rest: %v", err)
	}
	if !strings.Contains(string(tail), "[DONE]") {
		t.Fatalf("tail = %q", tail)
	}
}

// An upstream that accepts a streaming request and then goes silent must not
// hold the connection open until the hard upstream timeout.
func TestEndToEndAbortsAStreamThatGoesSilent(t *testing.T) {
	silent := make(chan struct{})
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"delta\":\"hi\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(silent)
		<-r.Context().Done() // go silent until the caller gives up
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	start := time.Now()
	rec := h.post(t, "/v1/chat/completions", `{"model":"x","stream":true}`)
	elapsed := time.Since(start)

	select {
	case <-silent:
	default:
		t.Fatal("the upstream never got as far as going silent")
	}
	// The harness sets a 2s idle timeout against a 30s upstream timeout, so
	// anything near 30s means the guard did nothing.
	if elapsed > 6*time.Second {
		t.Fatalf("the idle guard never fired: request took %v", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"hi"`) {
		t.Errorf("the bytes delivered before going silent were lost: %q", rec.Body.String())
	}
}

func TestEndToEndUnknownAccountIs404(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/nope/v1/chat/completions", strings.NewReader("{}"))
	h.engine.ServeReverse(rec, req, "nope", "/v1/chat/completions")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// forward-proxy mode
// ---------------------------------------------------------------------------

func TestEndToEndForwardProxySelectsAccountFromProxyCredentials(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via-forward"))
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	proxyURL, err := url.Parse(h.proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	proxyURL.User = url.UserPassword("zen-01", "anything")
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableCompression: true},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Get(h.baseURL + "/zen/v1/models")
	if err != nil {
		t.Fatalf("GET through the forward proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-forward" {
		t.Fatalf("body = %q", body)
	}
}

func TestEndToEndForwardProxyWithoutCredentialsIs407(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, h.baseURL+"/zen/v1/models", nil)
	h.engine.ServeForward(rec, req)
	if rec.Code != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d, want 407", rec.Code)
	}
	if rec.Header().Get("Proxy-Authenticate") == "" {
		t.Error("a 407 must advertise Proxy-Authenticate")
	}
}

func TestEndToEndForwardProxyHonoursAllowlist(t *testing.T) {
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	pinned := New(h.reg, h.pool, Options{
		Upstreams:    h.upOpts.Upstreams,
		Policy:       h.upOpts.Policy,
		AllowedHosts: []string{"opencode.ai"},
		Log:          testLogger(),
	})

	// An upstream hostname is allowed implicitly, so one list governs both
	// ingress shapes.
	if !pinned.hostAllowed("opencode.ai") {
		t.Error("opencode.ai must be allowed implicitly via the upstream map")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/steal", nil)
	req.Header.Set("X-Zen-Account", "zen-01")
	pinned.ServeForward(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a host outside the allowlist", rec.Code)
	}
}

func TestAccountFromProxyAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("Proxy-Authorization", "Basic "+basic("zen-07", "x"))
	if got := AccountFromProxyAuth(req); got != "zen-07" {
		t.Fatalf("basic auth: got %q", got)
	}
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("X-Zen-Account", "zen-08")
	if got := AccountFromProxyAuth(req); got != "zen-08" {
		t.Fatalf("header fallback: got %q", got)
	}
	req.Header.Del("X-Zen-Account")
	if got := AccountFromProxyAuth(req); got != "" {
		t.Fatalf("no credentials: got %q", got)
	}
}
