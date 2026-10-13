package forward

import (
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/proxypool"
)

// The storm fallback must engage only when a request has PROVEN the proxy
// layer is flapping pool-wide: two straight transport/proxy-auth failures,
// a direct pseudo-exit present in the pool, and direct itself serving. Any
// other state keeps normal rotation.
func TestStormFallbackExitGate(t *testing.T) {
	log := testLogger()
	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "dead", URL: "http://" + deadAddr(t)},
		{ID: "direct", URL: "direct://"},
	}, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	e := &Engine{pool: pool, log: log}

	if got := e.stormFallbackExit(0); got != nil {
		t.Fatalf("engaged before any failure: %+v", got)
	}
	if got := e.stormFallbackExit(1); got != nil {
		t.Fatalf("engaged below the threshold (first strike may be a blip): %+v", got)
	}
	p := e.stormFallbackExit(stormFallbackThreshold)
	if p == nil || p.ID != "direct" {
		t.Fatalf("expected the direct exit once the route is proven broken, got %+v", p)
	}

	// Direct itself must be serving to qualify: mark it sick and the gate
	// closes — fallback never replaces one broken route with another.
	pool.MarkFailure("direct", "dial: refused")
	if got := e.stormFallbackExit(stormFallbackThreshold); got != nil {
		t.Fatalf("engaged while direct is quarantined: %+v", got)
	}

	// No direct member at all: the feature simply does not exist.
	pool2, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "dead", URL: "http://" + deadAddr(t)},
	}, config.Health{
		Timeout:          time.Second,
		FailureThreshold: 1,
		Quarantine:       time.Minute,
		MaxBackoff:       time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("NewPool 2: %v", err)
	}
	e2 := &Engine{pool: pool2, log: log}
	if got := e2.stormFallbackExit(stormFallbackThreshold); got != nil {
		t.Fatalf("engaged without a direct exit: %+v", got)
	}
}
