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

	if got := e.stormFallbackExit(0, false); got != nil {
		t.Fatalf("engaged before any failure: %+v", got)
	}
	if got := e.stormFallbackExit(1, false); got != nil {
		t.Fatalf("engaged below the threshold (first strike may be a blip): %+v", got)
	}
	p := e.stormFallbackExit(stormFallbackThreshold, false)
	if p == nil || p.ID != "direct" {
		t.Fatalf("expected the direct exit once the route is proven broken, got %+v", p)
	}

	// Direct itself must be serving to qualify: mark it sick and the gate
	// closes — fallback never replaces one broken route with another.
	pool.MarkFailure("direct", "dial: refused")
	if got := e.stormFallbackExit(stormFallbackThreshold, false); got != nil {
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
	if got := e2.stormFallbackExit(stormFallbackThreshold, false); got != nil {
		t.Fatalf("engaged without a direct exit: %+v", got)
	}
}

// A request the free-tier gate will judge must not fall back to an exit the
// gate does not admit. That fallback cannot fix a transport storm — the gate
// refuses the exit before any generation happens — and it costs the request its
// remaining attempts. Measured: direct was the most-refused exit in the pool
// while sitting outside the admission set, and every such refusal ended in a
// failed turn.
func TestStormFallbackSkipsUnadmittedDirectForGatedRequests(t *testing.T) {
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

	// Plain traffic keeps the old behaviour.
	if got := e.stormFallbackExit(stormFallbackThreshold, false); got == nil || got.ID != "direct" {
		t.Fatalf("plain traffic lost its fallback: %+v", got)
	}
	// Gated traffic does not, while direct is unadmitted.
	if got := e.stormFallbackExit(stormFallbackThreshold, true); got != nil {
		t.Fatalf("gated request fell back to an unadmitted exit: %+v", got)
	}
	// Once the gate has actually admitted direct, the fallback is legitimate.
	pool.MarkGatePass("direct")
	if got := e.stormFallbackExit(stormFallbackThreshold, true); got == nil || got.ID != "direct" {
		t.Fatalf("admitted direct must be usable as fallback: %+v", got)
	}
}
