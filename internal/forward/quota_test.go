package forward

import (
	"net/http"
	"testing"
	"time"

	"zenproxy/internal/model"
)

// --- reading the upstream's own deadline ------------------------------------

func TestQuotaDeadlineReadsBothRetryAfterEncodings(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header string
		want   time.Time
	}{
		// What this upstream actually sends: a countdown toward one fixed reset.
		{"delta seconds", "3600", now.Add(time.Hour)},
		{"one second", "1", now.Add(time.Second)},
		// The HTTP-date form an RFC-compliant upstream would send instead.
		{"http-date", "Sun, 01 Mar 2026 13:30:00 GMT", time.Date(2026, 3, 1, 13, 30, 0, 0, time.UTC)},
		{"rfc850", "Sunday, 01-Mar-26 13:30:00 GMT", time.Date(2026, 3, 1, 13, 30, 0, 0, time.UTC)},
		{"ansic", "Sun Mar  1 13:30:00 2026", time.Date(2026, 3, 1, 13, 30, 0, 0, time.UTC)},
		// Whitespace around the value is common and must not defeat the parse.
		{"padded seconds", "  3600  ", now.Add(time.Hour)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("Retry-After", c.header)
			got, ok := quotaDeadline(h, now)
			if !ok {
				t.Fatalf("quotaDeadline(%q) reported nothing to read", c.header)
			}
			if !got.Equal(c.want) {
				t.Fatalf("quotaDeadline(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

func TestQuotaDeadlineRefusesWhatItCannotTrust(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	// Every one of these must come back as "no deadline", because the caller's
	// fallback — reactive rotation — is correct and a wrong deadline is not.
	// Cooling an exit until a moment nobody stated would take a working IP out
	// of the pool for an invented duration.
	bad := []struct {
		name   string
		header string
	}{
		{"absent", ""},
		{"blank", "   "},
		{"zero", "0"},
		{"negative", "-60"},
		// The RFC allows only an integer here. A float is some other header that
		// landed in this slot, and reading it as seconds would cool the exit for
		// a duration nobody asked for.
		{"float", "1.5"},
		{"garbage", "soon"},
		{"unit suffix", "3600s"},
		{"nonsense date", "Sun, 32 Mar 2026 13:30:00 GMT"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.header != "" {
				h.Set("Retry-After", c.header)
			}
			if got, ok := quotaDeadline(h, now); ok {
				t.Fatalf("quotaDeadline(%q) invented a deadline of %v", c.header, got)
			}
		})
	}
}

// --- recording it against the right thing -----------------------------------

// upstreamAlways answers every request with one fixed refusal.
func upstreamAlways(status int, header http.Header, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// bindLive pins the account to the exit that can actually carry a request, so a
// test measures the refusal under study rather than which exit round-robin
// happened to hand out.
func bindLive(t *testing.T, h *harness) {
	t.Helper()
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if px := h.reg.ExitFor("zen-01"); px == nil || px.ID != "live" {
		t.Fatalf("the fixture failed to pin the account to the live exit, got %v", px)
	}
}

func TestARefusalWithRetryAfterCoolsTheExitThatCarriedIt(t *testing.T) {
	hd := http.Header{}
	hd.Set("Retry-After", "3600")
	h := newHarness(t, upstreamAlways(http.StatusTooManyRequests, hd,
		`{"error":{"type":"FreeUsageLimitError","message":"limit reached"}}`))
	bindLive(t, h)

	h.post(t, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	until, reason, cool := h.pool.QuotaCooled("live")
	if !cool {
		t.Fatal("an exit that carried a 429 with Retry-After was not cooled")
	}
	// The deadline must be the upstream's, not a duration this proxy invented.
	if left := time.Until(until); left < 50*time.Minute || left > 61*time.Minute {
		t.Fatalf("deadline is %v away, want about an hour", left.Round(time.Second))
	}
	if reason != string(model.ClassRateLimit) {
		t.Errorf("reason = %q, want %q", reason, model.ClassRateLimit)
	}
}

func TestA429WithoutRetryAfterLeavesTheExitAlone(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusTooManyRequests, nil,
		`{"error":{"type":"FreeUsageLimitError","message":"limit reached"}}`))
	bindLive(t, h)

	h.post(t, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	// No header means no deadline. Rotation still happens — that path predates
	// the ledger and must keep working — but nothing may be recorded, because
	// there is nothing to record.
	if _, _, cool := h.pool.QuotaCooled("live"); cool {
		t.Fatal("a 429 with no Retry-After produced a cooldown out of nothing")
	}
}

func TestATierGateNeverCoolsTheExit(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusForbidden, nil,
		`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`))
	bindLive(t, h)

	h.post(t, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	// The whole reason ClassTierGate exists as a separate class: this refusal is
	// about the request's shape, not about the IP. Cooling the exit would take a
	// perfectly good IP out of the pool and, worse, hide the real cause behind a
	// rotation loop that can never fix it.
	if _, _, cool := h.pool.QuotaCooled("live"); cool {
		t.Fatal("a tier gate cooled the exit; the gate is not an IP problem")
	}
}

// --- acting on the ledger before paying for the refusal ---------------------

func TestAvoidCooledExitMovesTheAccountOffIt(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	bindLive(t, h)
	h.pool.CoolForQuota("live", time.Now().Add(time.Hour), "429")

	h.engine.avoidSpentExit("zen-01", "muse-spark")

	got := h.reg.ExitFor("zen-01")
	if got == nil {
		t.Fatal("the account has no exit after avoiding a cooled one")
	}
	if got.ID == "live" {
		t.Fatal("the account stayed on an exit whose quota is spent")
	}
}

func TestAvoidCooledExitLeavesTheAccountAloneWhenEveryExitIsSpent(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	bindLive(t, h)
	h.pool.CoolForQuota("live", time.Now().Add(time.Hour), "429")
	h.pool.CoolForQuota("dead", time.Now().Add(time.Hour), "429")

	h.engine.avoidSpentExit("zen-01", "muse-spark")

	// Rotating into another spent exit would churn the binding, spend a
	// rotation and change nothing. Staying put is the honest move: the request
	// is refused wherever it goes, and this exit at least knows when its
	// allowance returns.
	if got := h.reg.ExitFor("zen-01"); got == nil || got.ID != "live" {
		t.Fatalf("the account moved to %v even though every exit is spent", got)
	}
}

func TestAvoidCooledExitDoesNothingWhenTheExitIsFine(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	bindLive(t, h)
	before, _, _ := h.pool.QuotaCooled("live")

	h.engine.avoidSpentExit("zen-01", "muse-spark")

	if got := h.reg.ExitFor("zen-01"); got == nil || got.ID != "live" {
		t.Fatalf("a healthy exit was rotated away from for no reason: %v", got)
	}
	if until, _, cool := h.pool.QuotaCooled("live"); cool || !until.Equal(before) {
		t.Error("avoiding a cooled exit invented a cooldown")
	}
}

func TestPickAccountPrefersAnExitThatCanActuallyServe(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	if err := h.reg.Upsert(model.Account{
		ID: "zen-02", Upstream: "zen", APIKey: "sk-account-key-2", Pool: "zen", Proxy: "auto", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind zen-01: %v", err)
	}
	if err := h.reg.Bind("zen-02", "dead"); err != nil {
		t.Fatalf("Bind zen-02: %v", err)
	}
	// zen-01 holds the good exit but is out of quota; zen-02's exit is worse but
	// can serve. With four usable exits out of twenty-four measured, choosing by
	// strategy alone means most requests are refused before the retry loop even
	// starts, so the choice has to account for the ledger.
	h.pool.CoolForQuota("live", time.Now().Add(time.Hour), "429")

	for i := 0; i < 20; i++ {
		ent := h.engine.pickAccount("zen", "round-robin", nil, "muse-spark")
		if ent == nil {
			t.Fatal("pickAccount returned nothing while a warm account existed")
		}
		if ent.ID == "zen-01" {
			t.Fatalf("pickAccount chose the account parked on a spent exit (call %d)", i)
		}
	}
}

func TestPickAccountStillAnswersWhenEveryAccountIsSpent(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	h.pool.CoolForQuota("live", time.Now().Add(time.Hour), "429")
	h.pool.CoolForQuota("dead", time.Now().Add(time.Hour), "429")

	// Failing the request outright would be strictly worse than trying: the
	// refusal is what tells the caller when the pool comes back.
	ent := h.engine.pickAccount("zen", "round-robin", nil, "muse-spark")
	if ent == nil {
		t.Fatal("pickAccount gave up instead of returning the least-bad account")
	}
	if ent.ID != "zen-01" {
		t.Fatalf("pickAccount = %q, want the only account in the pool", ent.ID)
	}
}

// --- the admin surface ------------------------------------------------------

func TestProxyHealthCarriesTheCooldownToThePanel(t *testing.T) {
	h := newHarness(t, upstreamAlways(http.StatusOK, nil, `{}`))
	bindLive(t, h)
	h.pool.CoolForQuota("live", time.Now().Add(time.Hour), "429")

	var found bool
	for _, ph := range h.pool.Health() {
		if ph.ID != "live" {
			continue
		}
		found = true
		if ph.QuotaUntil == nil {
			t.Fatal("the panel cannot show what it is not told")
		}
		if ph.QuotaReason != "429" {
			t.Errorf("quota_reason = %q", ph.QuotaReason)
		}
		// The distinction an operator has to be able to draw.
		if !ph.Healthy {
			t.Error("a quota-cooled exit must still report healthy")
		}
		if ph.QuarantinedTill != nil {
			t.Error("a quota cooldown must not be reported as a quarantine")
		}
	}
	if !found {
		t.Fatal("the live exit is missing from the health listing")
	}
	// And the summary a dashboard would poll.
	cooled := h.pool.QuotaCooledIDs()
	if len(cooled) != 1 || cooled[0].ExitID != "live" {
		t.Fatalf("QuotaCooledIDs = %+v, want just the live exit", cooled)
	}
	if cooled[0].After == "" {
		t.Error("After should spare the operator the date arithmetic")
	}
}
