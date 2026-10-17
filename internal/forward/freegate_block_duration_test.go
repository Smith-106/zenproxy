package forward

import (
	"testing"
	"time"

	"zenproxy/internal/config"
)

// The free-tier park is the one knob that decides how fast the pool comes back
// after a refusal wave: ClearFreeTier only fires on a real upstream 2xx, and a
// parked exit is skipped by rotation, so nothing else can end the park early.
// Measured 2026-10-17 with the old hardcoded 15m: one wave parked enough exits
// that the pool answered 403 for minutes after the gate had reopened.
func TestFreeTierBlockDurationFollowsConfig(t *testing.T) {
	if got := freeTierBlockDuration(config.FreeTier{BlockDuration: 2 * time.Minute}); got != 2*time.Minute {
		t.Errorf("configured duration ignored: got %v, want 2m", got)
	}

	// A config file that predates the key, or sets it to something meaningless,
	// must keep the historical behaviour instead of parking forever.
	for _, cfg := range []config.FreeTier{{}, {BlockDuration: -time.Second}} {
		if got := freeTierBlockDuration(cfg); got != defaultFreeTierBlock {
			t.Errorf("fallback = %v, want %v", got, defaultFreeTierBlock)
		}
	}
	if defaultFreeTierBlock != 15*time.Minute {
		t.Errorf("the fallback must stay at the historical 15m, got %v", defaultFreeTierBlock)
	}
}

// The engine must carry the resolved value, not the raw config field: this is
// the field the refusal path passes to pool.BlockFreeTier.
func TestEngineCarriesFreeTierBlock(t *testing.T) {
	e := New(nil, nil, Options{FreeTier: config.FreeTier{BlockDuration: 90 * time.Second}})
	if e == nil {
		t.Fatal("New returned nil")
	}
	if e.freeTierBlock != 90*time.Second {
		t.Errorf("engine.freeTierBlock = %v, want 90s", e.freeTierBlock)
	}

	empty := New(nil, nil, Options{})
	if empty.freeTierBlock != 15*time.Minute {
		t.Errorf("engine.freeTierBlock = %v, want the 15m default", empty.freeTierBlock)
	}
}
