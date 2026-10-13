package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/proxypool"
)

// A folded stream that died mid-flight (or folded into garbage) is not just a
// client-visible error — the exit that carried it shipped a dead stream. One
// strike is nothing; a slot that repeatedly does this climbs its failure
// streak and leaves rotation like any other sick exit.
func TestWriteFoldedFeedsStreamDeathBackToPoolHealth(t *testing.T) {
	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "a", URL: "http://" + deadAddr(t)},
	}, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, testLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}, pool: pool, log: testLogger()}

	resp := &http.Response{Body: io.NopCloser(strings.NewReader("<html>502 from a gateway</html>"))}
	rec := httptest.NewRecorder()
	e.writeFolded(rec, resp, &GatePlan{Surface: surfaceChat}, "zen-01", "a", "ses_0123456789abcdefabcdefghij")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if ok, found := pool.Healthy("a"); !found || ok {
		t.Fatalf("a dead-stream strike must reach the pool health (healthy=%v found=%v)", ok, found)
	}
}
