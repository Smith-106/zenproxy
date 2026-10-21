package forward

import (
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/proxypool"
)

func TestQuotaFallbackLeavesASpentPopulation(t *testing.T) {
	log := testLogger()
	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "resin", URL: "http://" + deadAddr(t)},
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

	if got := e.quotaFallbackExit(0, "resin"); got != nil {
		t.Fatalf("switched before any 429: %+v", got)
	}
	if got := e.quotaFallbackExit(1, "resin"); got != nil {
		t.Fatalf("one 429 is not a population: %+v", got)
	}
	got := e.quotaFallbackExit(stormFallbackThreshold, "resin")
	if got == nil || got.ID != "direct" {
		t.Fatalf("two 429s did not leave for direct: %+v", got)
	}

	// An exit already inside a cooldown is a known refusal. Don't spend the
	// attempt that would only re-read it.
	pool.CoolForQuota("resin", time.Now().Add(time.Hour), "429")
	if got := e.quotaFallbackExit(0, "resin"); got == nil || got.ID != "direct" {
		t.Fatalf("a cooled exit was kept: %+v", got)
	}

	pool.CoolForQuota("direct", time.Now().Add(time.Hour), "429")
	if got := e.quotaFallbackExit(stormFallbackThreshold, "resin"); got != nil {
		t.Fatalf("fell back onto a cooled direct: %+v", got)
	}
}
