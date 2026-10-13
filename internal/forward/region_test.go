package forward

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// regionUpstream refuses one model on geography a fixed number of times.
//
// It answers by model name rather than by caller, which is not what a real
// upstream does — real geography belongs to the exit. The difference is what
// makes these tests able to say anything at all: the fixture refuses on demand,
// so a rotation's effect is observable, instead of the test trying to reproduce
// a country. The refusal count is what lets one test watch a discovery turn into
// a success on the next attempt.
type regionUpstream struct {
	refuse atomic.Int64
	hits   atomic.Int64
}

const regionRefusal = `{"error":{"type":"RegionError","message":"This model is not available in your country."}}`

func (u *regionUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits.Add(1)
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if bytes.Contains(body, []byte("m-blocked")) && u.refuse.Load() > 0 {
		u.refuse.Add(-1)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, regionRefusal)
		return
	}
	io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
}

// regionHarness is the stock fixture with two working exits instead of one
// working and one dead.
//
// The stock harness cannot express the claim: swapping a refused exit for a
// working one requires a working one to swap to, and with only one, every
// rotation lands on the dead exit and the test proves nothing but that failure
// propagates.
type regionHarness struct {
	engine *Engine
	reg    *registry.Registry
	pool   *proxypool.Pool
	a, b   *mockExit
	up     *regionUpstream
}

func newRegionHarness(t *testing.T) *regionHarness {
	t.Helper()
	up := &regionUpstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	a, b := newMockExit(t), newMockExit(t)
	log := testLogger()

	pool, err := proxypool.NewPool([]proxypool.Spec{
		{ID: "exit-a", URL: a.srv.URL},
		{ID: "exit-b", URL: b.srv.URL},
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
	// A second account, because the gateway's remedy is to change ACCOUNT rather
	// than exit, and that cannot be tested with one.
	if err := reg.Upsert(model.Account{
		ID: "zen-02", Upstream: "zen", APIKey: "sk-account-key-2", Proxy: "auto", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert zen-02: %v", err)
	}
	// Pin the starting exits. An unbound account resolves through PickFor, which
	// already skips region-blocked exits, so the test would not know which exit
	// it began on and could not tell a pre-send move from a lucky first pick.
	if err := reg.Bind("zen-01", "exit-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := reg.Bind("zen-02", "exit-b"); err != nil {
		t.Fatalf("Bind zen-02: %v", err)
	}

	eng := New(reg, pool, Options{
		Upstreams: map[string]model.Upstream{"zen": {BaseURL: srv.URL + "/zen/v1"}},
		Policy: config.Policy{
			MaxAttempts:       3,
			RotateOnStatus:    []int{403, 407, 429, 500, 502, 503, 504},
			RotateOnTransport: true,
			MaxBodyBytes:      1 << 20,
			StreamIdleTimeout: 2 * time.Second,
			UpstreamTimeout:   30 * time.Second,
		},
		Log:      log,
		FreeTier: config.FreeTier{Enabled: true},
	})

	return &regionHarness{engine: eng, reg: reg, pool: pool, a: a, b: b, up: up}
}

func (h *regionHarness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/zen-01/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-client-token")
	h.engine.ServeReverse(rec, req, "zen-01", "/v1/chat/completions")
	return rec
}

func (h *regionHarness) bound() string { return h.reg.BoundExit("zen-01") }

// A region refusal must be recorded against the exit AND the model, because that
// pair is what the upstream actually refuses. Measured on the live pool: one
// exit answered RegionError for one free model and 200 for another in the same
// second.
func TestARegionRefusalIsRememberedPerExitAndModel(t *testing.T) {
	h := newRegionHarness(t)
	h.up.refuse.Store(1)
	h.post(t, `{"model":"m-blocked","messages":[{"role":"user","content":"hi"}]}`)

	if !h.pool.RegionBlocked("exit-a", "m-blocked") {
		t.Fatal("the exit that carried the refusal was not recorded for that model")
	}
	if h.pool.RegionBlocked("exit-a", "m-other") {
		t.Fatal("the refusal leaked onto a different model on the same exit")
	}
	if h.pool.RegionBlocked("exit-b", "m-blocked") {
		t.Fatal("the refusal leaked onto a different exit")
	}
}

// It must not enter the quota ledger. They are different facts with different
// lifetimes: a quota cooldown ends on a clock the upstream names, and a region
// block ends when a country's policy changes, which nothing announces. Treating
// them as one would either invent a deadline or never expire.
func TestARegionRefusalIsNotAQuotaCooldown(t *testing.T) {
	h := newRegionHarness(t)
	h.up.refuse.Store(1)
	h.post(t, `{"model":"m-blocked","messages":[{"role":"user","content":"hi"}]}`)

	if _, reason, cool := h.pool.QuotaCooled("exit-a"); cool {
		t.Fatalf("a region refusal entered the quota ledger (reason %q)", reason)
	}
}

// The gateway's remedy is the other lever: change ACCOUNT rather than exit. An
// account parked on an exit refused for this model has to be skipped whenever a
// warm one exists, or the request is refused before the rotation loop starts.
func TestPickAccountAvoidsAnAccountOnAnExitRefusedForThisModel(t *testing.T) {
	h := newRegionHarness(t)
	h.pool.BlockRegion("exit-a", "m-blocked")

	for i := 0; i < 20; i++ {
		ent := h.engine.pickAccount("zen", "round-robin", nil, "m-blocked")
		if ent == nil {
			t.Fatal("pickAccount gave up while a warm account existed")
		}
		if ent.ID == "zen-01" {
			t.Fatalf("pickAccount chose the account parked on an exit refused for this model (call %d)", i)
		}
	}

	// For a different model the same account is ordinary again: it was set aside
	// for one model, not taken out of service.
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if ent := h.engine.pickAccount("zen", "round-robin", nil, "m-other"); ent != nil {
			seen[ent.ID] = true
		}
	}
	if !seen["zen-01"] {
		t.Fatal("the account was withheld for a model its exit serves fine")
	}
}

// With no alternative it must still answer. Failing outright would be strictly
// worse: a refusal tells the caller when the pool comes back, and this silence
// would not.
func TestPickAccountStillAnswersWhenEveryAccountIsRefused(t *testing.T) {
	h := newRegionHarness(t)
	h.pool.BlockRegion("exit-a", "m-blocked")
	h.pool.BlockRegion("exit-b", "m-blocked")

	ent := h.engine.pickAccount("zen", "round-robin", nil, "m-blocked")
	if ent == nil {
		t.Fatal("pickAccount gave up instead of returning the least-bad account")
	}
}

// The whole point, in its strongest form: an exit already known to be refused
// for this model must not be contacted at all.
func TestAKnownRefusalIsAvoidedBeforeSending(t *testing.T) {
	h := newRegionHarness(t)
	h.up.refuse.Store(0) // the upstream would serve anything; only the ledger can stop it
	h.pool.BlockRegion("exit-a", "m-blocked")

	rec := h.post(t, `{"model":"m-blocked","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := h.a.hits.Load(); got != 0 {
		t.Fatalf("the exit refused for this model was contacted %d time(s); it should not be tried at all", got)
	}
	if got := h.b.hits.Load(); got != 1 {
		t.Fatalf("the working exit was contacted %d time(s), want exactly 1", got)
	}
	if got := h.bound(); got != "exit-b" {
		t.Fatalf("account is bound to %q, want exit-b", got)
	}
}

// And when the refusal is discovered rather than known, the rotation has to land
// somewhere the model works — which is what carrying the model through RotateFor
// buys. Without it the replacement is chosen on health alone and could be
// another exit refused for the same model.
func TestRegionRotationLandsSomewhereTheModelWorks(t *testing.T) {
	h := newRegionHarness(t)
	h.up.refuse.Store(1) // refused once, then served

	rec := h.post(t, `{"model":"m-blocked","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the second exit serves it: %s", rec.Code, rec.Body.String())
	}
	if !h.pool.RegionBlocked("exit-a", "m-blocked") {
		t.Fatal("the discovery was not recorded")
	}
	if got := h.bound(); got == "exit-a" {
		t.Fatal("the account stayed on the exit that refused it")
	}
}

// An empty model name must not become a wildcard. There is no evidence that
// "refused for all models" exists, and accepting it would silently be a
// per-exit block through a different door.
func TestABlockWithoutAModelNameIsRefused(t *testing.T) {
	h := newRegionHarness(t)
	if h.pool.BlockRegion("exit-a", "") {
		t.Fatal("an empty model name was accepted as a region block")
	}
	if h.pool.RegionBlocked("exit-a", "") {
		t.Fatal("an empty model name reported as blocked")
	}
}

// The expensive direction of the mistake: a block scoped to an exit instead of
// to the pair would take a working IP out of rotation for every model it serves
// fine.
func TestAnExitRefusedForOneModelStillServesOthers(t *testing.T) {
	h := newRegionHarness(t)
	h.pool.BlockRegion("exit-a", "m-blocked")

	// For the refused model, the healthy other exit wins.
	if px := h.pool.PickFor("m-blocked", ""); px == nil || px.ID != "exit-b" {
		t.Fatalf("for the refused model PickFor offered %v, want exit-b", px)
	}
	// For any other model, the same exit is ordinary again.
	if px := h.pool.PickFor("m-other", "exit-b"); px == nil || px.ID != "exit-a" {
		t.Fatalf("for a different model the blocked exit was withheld (got %v)", px)
	}
}

// When every remaining exit is region-blocked for the model, fail-fast rather
// than burning an attempt on a known 403. The 6h TTL is the rediscovery path;
// paying one guaranteed refusal per request was the expensive mistake.
func TestARefusedExitIsNotUsedWhenNothingElseRemains(t *testing.T) {
	h := newRegionHarness(t)
	h.pool.BlockRegion("exit-a", "m-blocked")

	if px := h.pool.PickFor("m-blocked", "exit-b"); px != nil {
		t.Fatalf("PickFor handed out region-blocked %q; want nil (fail-fast)", px.ID)
	}
}

// A caller that cannot see the model must not have exits withheld from it on a
// guess. The forward ingress never learns the model, and neither can a health
// probe.
func TestPickWithoutAModelIgnoresRegionBlocks(t *testing.T) {
	h := newRegionHarness(t)
	h.pool.BlockRegion("exit-a", "m-blocked")

	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		px := h.pool.PickFor("", "exit-b")
		if px == nil {
			t.Fatal("Pick with no model refused to hand out anything")
		}
		seen[px.ID] = true
	}
	if !seen["exit-a"] {
		t.Fatal("an exit was withheld from a caller that could not judge the model")
	}
}

// Classifying it as ClassAuth, as it was, is what made this invisible: ClassAuth
// is not IP-attributable, so nothing rotated and the caller was simply told the
// model did not exist for them.
func TestARegionRefusalIsClassifiedAndRotatable(t *testing.T) {
	if got := classifyStatus(http.StatusForbidden, http.Header{}, []byte(regionRefusal)); got != model.ClassRegion {
		t.Fatalf("classifyStatus = %q, want %q", got, model.ClassRegion)
	}
	if !model.ClassRegion.IPAttributable() {
		t.Fatal("a region refusal must be attributable to the exit")
	}
	if !model.ClassRegion.Retryable() {
		t.Fatal("a region refusal must be retryable elsewhere")
	}
	// Auth stays non-attributable; tier_gate is rotatable after the envelope
	// (see TestTierGateRotatesAfterEnvelope).
	if !model.ClassTierGate.IPAttributable() {
		t.Fatal("the tier gate must be IP-attributable so enveloped FreeTierError can rotate")
	}
	if model.ClassAuth.IPAttributable() {
		t.Fatal("a plain 403 became IP-attributable")
	}
	// A Cloudflare challenge is a 403 too, and it is a server-side wall that a
	// different IP may well get past — but it must not be mistaken for geography.
	blocked := http.Header{}
	blocked.Set("Cf-Mitigated", "challenge")
	if got := classifyStatus(http.StatusForbidden, blocked, []byte(`{"x":1}`)); got != model.ClassServer {
		t.Fatalf("a Cloudflare challenge classified as %q, want %q", got, model.ClassServer)
	}
}
