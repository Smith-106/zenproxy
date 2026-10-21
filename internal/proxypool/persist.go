package proxypool

// Exit-health persistence.
//
// A restart used to amnesty the whole pool: every quarantine, free-tier
// block, quota cooldown and retire verdict lived only in process memory, so
// a `docker restart` handed every long-dead slot a fresh conscience. The
// result was measurable: each restart let requests re-burn their attempt
// budget on the same known-dead slots, and the failure counters show roughly
// three failed upstream calls per request against the same recurring dead
// batch.
//
// The fix is a small sidecar file beside the registry's state.json. It
// records, per exit, the facts that should outlive a process death:
//
//   - the verdict flags (retired, quarantine, free-tier block, quota cool,
//     per-model region blocks) as wall-clock deadlines, so a restart neither
//     extends nor shortens them — expired ones are simply not restored;
//   - the failure streaks (failures, soft, consecutive, TLS-poison), so an
//     exit that was one strike away from retirement still is.
//
// What deliberately does NOT survive: the healthy/unprobed flag. A fresh
// process starts optimistic and lets the first probe round judge again —
// the probe path is still the thing that clears a quarantine, exactly as
// before. Retirement is the only state a probe cannot clear, and it
// therefore survives everything except an explicit Revive.
//
// Writes are debounced (dirty flag + 30s ticker inside Run) and atomic
// (tmp file + rename). Load errors degrade to "start clean" with a warning.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const exitsStateVersion = 1

// exitRecord is the persisted slice of one member's liveness state.
type exitRecord struct {
	Retired         bool      `json:"retired,omitempty"`
	RetiredReason   string    `json:"retired_reason,omitempty"`
	RetiredAt       time.Time `json:"retired_at,omitempty"`
	QuarantineUntil time.Time `json:"quarantine_until,omitempty"`
	FreeTierUntil   time.Time `json:"free_tier_until,omitempty"`
	FreeTierReason  string    `json:"free_tier_reason,omitempty"`
	QuotaUntil      time.Time `json:"quota_until,omitempty"`
	QuotaReason     string    `json:"quota_reason,omitempty"`
	// GatePassUntil is the free-tier admission deadline and GatePasses its
	// lifetime count. Persisted because the whole point of the signal is that
	// it is scarce: rediscovering the six clean exits in a 1,613-member pool
	// costs the first ~2% -passing minutes after every restart.
	GatePassUntil time.Time `json:"gate_pass_until,omitempty"`
	GatePasses    int       `json:"gate_passes,omitempty"`
	// GateScore ranks admitted exits against each other (see member.gateScore)
	// and GateDrops counts the streams they failed to carry. Both persist for the
	// same reason as the admission itself: a restart should not reset the pool's
	// opinion of which admitted tunnel is the reliable one, or it will re-learn
	// that by handing clients a few more dead streams.
	GateScore    int                  `json:"gate_score,omitempty"`
	GateDrops    int                  `json:"gate_drops,omitempty"`
	RegionBlocks map[string]time.Time `json:"region_blocks,omitempty"`
	Failures     int64                `json:"failures,omitempty"`
	SoftFails    int64                `json:"soft_fails,omitempty"`
	ConsecFails  int                  `json:"consec_fails,omitempty"`
	PoisonConsec int                  `json:"poison_consec,omitempty"`
}

// MarshalJSON omits zero timestamps. encoding/json's omitempty does not treat
// time.Time{} as empty, so every live exit was saved with retired_at,
// quarantine_until and the rest set to year 1. A reader then cannot tell a
// retired exit from one that has never been retired.
func (r exitRecord) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if r.Retired {
		m["retired"] = true
	}
	if r.RetiredReason != "" {
		m["retired_reason"] = r.RetiredReason
	}
	putTime(m, "retired_at", r.RetiredAt)
	putTime(m, "quarantine_until", r.QuarantineUntil)
	putTime(m, "free_tier_until", r.FreeTierUntil)
	if r.FreeTierReason != "" {
		m["free_tier_reason"] = r.FreeTierReason
	}
	putTime(m, "quota_until", r.QuotaUntil)
	if r.QuotaReason != "" {
		m["quota_reason"] = r.QuotaReason
	}
	putTime(m, "gate_pass_until", r.GatePassUntil)
	if r.GatePasses != 0 {
		m["gate_passes"] = r.GatePasses
	}
	if r.GateScore != 0 {
		m["gate_score"] = r.GateScore
	}
	if r.GateDrops != 0 {
		m["gate_drops"] = r.GateDrops
	}
	if len(r.RegionBlocks) > 0 {
		blocks := make(map[string]time.Time, len(r.RegionBlocks))
		for model, until := range r.RegionBlocks {
			if !until.IsZero() {
				blocks[model] = until.UTC()
			}
		}
		if len(blocks) > 0 {
			m["region_blocks"] = blocks
		}
	}
	if r.Failures != 0 {
		m["failures"] = r.Failures
	}
	if r.SoftFails != 0 {
		m["soft_fails"] = r.SoftFails
	}
	if r.ConsecFails != 0 {
		m["consec_fails"] = r.ConsecFails
	}
	if r.PoisonConsec != 0 {
		m["poison_consec"] = r.PoisonConsec
	}
	return json.Marshal(m)
}

func putTime(m map[string]any, key string, t time.Time) {
	if t.IsZero() {
		return
	}
	m[key] = t.UTC()
}

// exitsState is the on-disk document.
type exitsState struct {
	Version int                   `json:"version"`
	SavedAt time.Time             `json:"saved_at"`
	Exits   map[string]exitRecord `json:"exits"`
}

// markDirty schedules a debounced flush. No-op when persistence is off.
func (p *Pool) markDirty() {
	if p.statePath != "" {
		p.dirty.Store(true)
	}
}

// SaveState snapshots every member's persisted slice and writes it atomically.
func (p *Pool) SaveState() {
	if p.statePath == "" {
		return
	}
	st := exitsState{
		Version: exitsStateVersion,
		SavedAt: time.Now().UTC(),
		Exits:   make(map[string]exitRecord, len(p.members)),
	}
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, m := range p.members {
		m.mu.Lock()
		rec := exitRecord{
			Retired:         m.retired,
			RetiredReason:   m.retiredReason,
			RetiredAt:       m.retiredAt,
			QuarantineUntil: m.quarantineUntil,
			FreeTierUntil:   m.freeTierUntil,
			FreeTierReason:  m.freeTierReason,
			QuotaUntil:      m.quotaUntil,
			QuotaReason:     m.quotaReason,
			GatePassUntil:   m.gatePassUntil,
			GatePasses:      m.gatePasses,
			GateScore:       m.gateScore,
			GateDrops:       m.gateDrops,
			Failures:        m.failures,
			SoftFails:       m.softFails,
			ConsecFails:     m.consecFails,
			PoisonConsec:    m.poisonConsec,
		}
		if len(m.regionBlocked) > 0 {
			blocks := make(map[string]time.Time, len(m.regionBlocked))
			for mdl, until := range m.regionBlocked {
				if now.Before(until) {
					blocks[mdl] = until
				}
			}
			rec.RegionBlocks = blocks
		}
		m.mu.Unlock()
		st.Exits[m.ID] = rec
	}
	b, err := json.Marshal(st)
	if err != nil {
		p.log.Warn("exits state marshal failed", "err", err)
		return
	}
	if dir := filepath.Dir(p.statePath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			p.log.Warn("exits state dir create failed", "err", err)
			return
		}
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		p.log.Warn("exits state write failed", "err", err)
		return
	}
	if err := os.Rename(tmp, p.statePath); err != nil {
		p.log.Warn("exits state rename failed", "err", err)
		return
	}
	p.dirty.Store(false)
}

// loadState replays the persisted slice into the freshly built pool. Deadlines
// that have already passed while the process was down are dropped, not
// restored; counters are restored as they were.
func (p *Pool) loadState() {
	if p.statePath == "" {
		return
	}
	b, err := os.ReadFile(p.statePath)
	if err != nil {
		// First boot of a per-host file: take one snapshot from the legacy
		// shared document, then never write back to it. Both replicas can
		// seed from the same facts without resuming the last-writer race.
		legacy := filepath.Join(filepath.Dir(p.statePath), "exits-state.json")
		if legacy == p.statePath {
			return
		}
		seed, seedErr := os.ReadFile(legacy)
		if seedErr != nil {
			return
		}
		b = seed
		p.log.Info("seeded exit health from the shared file", "from", legacy, "own", p.statePath)
	}
	var st exitsState
	if err := json.Unmarshal(b, &st); err != nil {
		p.log.Warn("exits state unreadable; starting clean", "path", p.statePath, "err", err)
		return
	}
	now := time.Now()
	restored := 0
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, rec := range st.Exits {
		m, ok := p.byID[id]
		if !ok {
			continue // exit no longer configured
		}
		m.mu.Lock()
		m.failures = rec.Failures
		m.softFails = rec.SoftFails
		m.consecFails = rec.ConsecFails
		m.poisonConsec = rec.PoisonConsec
		if rec.QuarantineUntil.After(now) {
			m.quarantineUntil = rec.QuarantineUntil
		}
		if rec.FreeTierUntil.After(now) {
			m.freeTierUntil = rec.FreeTierUntil
			m.freeTierReason = rec.FreeTierReason
		}
		if rec.QuotaUntil.After(now) {
			m.quotaUntil = rec.QuotaUntil
			m.quotaReason = rec.QuotaReason
		}
		// The pass count is restored unconditionally (it is history), while the
		// deadline only survives if it has not already elapsed while the
		// process was down — same rule as every other deadline here.
		m.gatePasses = rec.GatePasses
		m.gateScore = rec.GateScore
		m.gateDrops = rec.GateDrops
		if rec.GatePassUntil.After(now) {
			m.gatePassUntil = rec.GatePassUntil
		}
		if len(rec.RegionBlocks) > 0 {
			blocks := make(map[string]time.Time, len(rec.RegionBlocks))
			for mdl, until := range rec.RegionBlocks {
				if until.After(now) {
					blocks[mdl] = until
				}
			}
			if len(blocks) > 0 {
				m.regionBlocked = blocks
			}
		}
		if rec.Retired {
			m.retired = true
			m.retiredReason = rec.RetiredReason
			m.retiredAt = rec.RetiredAt
			m.healthy = false
		}
		m.mu.Unlock()
		restored++
	}
	if restored > 0 {
		p.log.Info("exit health restored from disk",
			"exits", restored, "path", p.statePath)
		// What was just read is what is on disk; do not schedule a redundant
		// first flush.
		p.dirty.Store(false)
	}
}

// ReplicaState is one exit-state file in this pool's directory. The admin
// panel is pinned to replica A, so without this the operator cannot see that
// replica B has retired a different set.
type ReplicaState struct {
	File    string    `json:"file"`
	SavedAt time.Time `json:"saved_at"`
	Exits   int       `json:"exits"`
	Retired int       `json:"retired"`
	Quota   int       `json:"quota"`
	Self    bool      `json:"self"`
}

// StateSnapshots reads every exit-state file next to this pool's own. Counts
// only: the files also hold failure reasons, which are not needed here and
// sometimes quote an upstream sentence.
func (p *Pool) StateSnapshots() []ReplicaState {
	if p.statePath == "" {
		return nil
	}
	dir := filepath.Dir(p.statePath)
	matches, err := filepath.Glob(filepath.Join(dir, "exits-state*.json"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]ReplicaState, 0, len(matches))
	for _, path := range matches {
		base := filepath.Base(path)
		if strings.Contains(base, ".bak-") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) > 8<<20 {
			continue
		}
		var st exitsState
		if json.Unmarshal(b, &st) != nil {
			continue
		}
		snap := ReplicaState{
			File:    base,
			SavedAt: st.SavedAt,
			Exits:   len(st.Exits),
			Self:    path == p.statePath,
		}
		for _, rec := range st.Exits {
			if rec.Retired {
				snap.Retired++
			}
			if rec.QuotaUntil.After(now) {
				snap.Quota++
			}
		}
		out = append(out, snap)
	}
	return out
}

// saveLoop flushes the persisted slice while the process runs, and once more
// at shutdown so the last verdicts are not lost to the debounce window.
func (p *Pool) saveLoop(ctx context.Context, every time.Duration) {
	if p.statePath == "" {
		return
	}
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if p.dirty.Load() {
				p.SaveState()
			}
			return
		case <-t.C:
			if p.dirty.Load() {
				p.SaveState()
			}
		}
	}
}
