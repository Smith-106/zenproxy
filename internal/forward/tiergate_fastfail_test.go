package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// The tier gate's red verdict spans every exit type at once (measured
// 2026-10-14: direct AND resin slots refused simultaneously for fifteen
// minutes while a sibling replica passed) and every refusal re-arms it.
// Burning the whole attempt budget across a dozen exits therefore cannot dodge
// the flag — it flag-poisons the entire pool (each refused exit gets a 15m
// free-tier block) and stretches a 403 into the multi-minute hammer the caller
// times out on. The loop must cap free-tier refusals: same-exit flap catch,
// one rotation, then hand the caller the upstream's own answer verbatim.
func TestTierGateRedWindowFastFails(t *testing.T) {
	var (
		mu   sync.Mutex
		hits int
	)

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits++
		mu.Unlock()
		_ = raw // enveloped or not, this window refuses everything
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	}))
	t.Cleanup(up.Close)

	exitA := newMockExit(t)
	exitB := newMockExit(t)
	log := testLogger()

	pol := config.Policy{
		MaxAttempts:       12,
		RotateOnStatus:    []int{403, 407, 429, 500, 502, 503, 504},
		RotateOnTransport: true,
		MaxBodyBytes:      1 << 20,
		StreamIdleTimeout: 2 * time.Second,
		UpstreamTimeout:   30 * time.Second,
	}
	opts := Options{
		Upstreams: map[string]model.Upstream{"zen": {BaseURL: up.URL + "/zen/v1"}},
		Policy:    pol,
		Log:       log,
		// The model is pre-configured, so the very first attempt already sits
		// inside the envelope; the mock refuses it anyway (red-window style).
		FreeTier: config.FreeTier{Enabled: true, Models: []string{"muse-spark-1.3-contributor-free"}},
	}

	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "live-a", URL: exitA.srv.URL},
		{ID: "live-b", URL: exitB.srv.URL},
	}, config.Health{
		Timeout: time.Second, FailureThreshold: 1, Quarantine: time.Minute, MaxBackoff: time.Minute,
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
		account, rest := splitAccount(r.URL.Path)
		eng.ServeReverse(w, r, account, rest)
	}))
	t.Cleanup(proxySrv.Close)

	rec := httptest.NewRecorder()
	body := `{"model":"muse-spark-1.3-contributor-free","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/zen-01/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	eng.ServeReverse(rec, req, "zen-01", "/v1/chat/completions")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("client status = %d, want 403 (the upstream refusal is relayed)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FreeTierError") {
		t.Errorf("client body = %.80q, want the upstream FreeTierError verbatim", rec.Body.String())
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 3 {
		t.Errorf("upstream attempts = %d, want 3 (same-exit flap catch + one rotation + fast-fail)", got)
	}
}