package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

// discardLogger keeps the package quiet while its handlers are under test.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type stubFree struct{ learned []string }

func (s stubFree) Learned() []string { return s.learned }

func testServer(t *testing.T, cfg config.Config, free FreeTierInfo) (*Server, *registry.Registry) {
	t.Helper()
	pool, err := proxypool.NewPool(nil, config.Health{}, discardLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	reg := registry.New("", pool, discardLogger())
	cfg.Server.AdminToken = "sekret"
	return New(reg, pool, cfg, free, discardLogger()), reg
}

func authed(s *Server, method, target, body string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("X-Admin-Token", "sekret")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

// A partial update must not wipe the fields the caller did not send: an empty
// upstream would route the account into a 500, an empty proxy into the wrong
// exits.
func TestUpsertMergesPartialUpdates(t *testing.T) {
	cfg := config.Default()
	s, reg := testServer(t, cfg, nil)
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Name: "n", Upstream: "zen", APIKey: "sk-0123456789abcdef",
		Proxy: "auto", Weight: 3, Enabled: true, Headers: map[string]string{"X-A": "b"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	rec := authed(s, http.MethodPost, "/admin/api/accounts", `{"id":"zen-01","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ent, ok := reg.Get("zen-01")
	if !ok {
		t.Fatal("account vanished")
	}
	if ent.Account.Enabled {
		t.Error("enabled was not flipped")
	}
	if ent.Account.Upstream != "zen" || ent.Account.Proxy != "auto" || ent.Account.Name != "n" || ent.Account.Weight != 3 {
		t.Errorf("untouched fields were disturbed: %+v", ent.Account)
	}
	if ent.Account.Headers["X-A"] != "b" {
		t.Errorf("headers were dropped: %v", ent.Account.Headers)
	}
	if ent.Account.APIKey != "sk-0123456789abcdef" {
		t.Error("the stored key must survive a key-less update")
	}
}

// …and an explicit field still wins over the stored one.
func TestUpsertExplicitFieldsWin(t *testing.T) {
	cfg := config.Default()
	s, reg := testServer(t, cfg, nil)
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Upstream: "zen", APIKey: "sk-0123456789abcdef", Proxy: "auto", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	rec := authed(s, http.MethodPost, "/admin/api/accounts",
		`{"id":"zen-01","upstream":"zen","proxy":"direct","weight":7,"headers":{"X-A":"b"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ent, _ := reg.Get("zen-01")
	if ent.Account.Proxy != "direct" || ent.Account.Weight != 7 || ent.Account.Headers["X-A"] != "b" {
		t.Errorf("explicit fields did not win: %+v", ent.Account)
	}
	if !ent.Account.Enabled {
		t.Error("enabled must be preserved when the caller does not send it")
	}
}

// 禁用/启用走的是同一条合并路径：一次点按不能把别的东西清掉。
func TestToggleRoundTripThroughTheAPI(t *testing.T) {
	cfg := config.Default()
	s, reg := testServer(t, cfg, nil)
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Name: "n", Upstream: "zen", APIKey: "sk-0123456789abcdef",
		Proxy: "slot-7", Weight: 2, Enabled: true, Headers: map[string]string{"X-A": "b"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	for _, want := range []bool{false, true} {
		body := `{"id":"zen-01","enabled":` + map[bool]string{true: "true", false: "false"}[want] + `}`
		if rec := authed(s, http.MethodPost, "/admin/api/accounts", body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		ent, _ := reg.Get("zen-01")
		if ent.Account.Enabled != want {
			t.Fatalf("enabled = %v, want %v", ent.Account.Enabled, want)
		}
		if ent.Account.Proxy != "slot-7" || ent.Account.Weight != 2 || ent.Account.Name != "n" || ent.Account.Headers["X-A"] != "b" {
			t.Fatalf("a toggle disturbed other fields: %+v", ent.Account)
		}
	}
}

// The dashboard contract: the page and every field it renders must exist, and
// the JSON it reads must carry those fields — the panel is static HTML and a
// typo in a selector or a key name fails silently in the browser.
func TestDashboardServesAndReadsItsOwnAPI(t *testing.T) {
	cfg := config.Default()
	s, reg := testServer(t, cfg, stubFree{learned: []string{"new-free"}})
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Name: "n", Upstream: "zen", APIKey: "sk-0123456789abcdef",
		Proxy: "auto", Weight: 2, Enabled: true, Headers: map[string]string{"X-A": "b"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	page := authed(s, http.MethodGet, "/admin/", "")
	if page.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d", page.Code)
	}
	html := page.Body.String()
	for _, want := range []string{
		`id="free"`, `id="free_lists"`, `id="free_hint"`, `id="foot"`, `id="cfg"`,
		`id="f_upstream"`, `id="f_weight"`, `id="f_headers"`, `id="stat"`,
		`id="exits"`, `id="accounts"`, `id="gate"`, `id="msg"`,
		`renderStat(`, `renderFree(`, `renderConfig(`, `renderExits(`, `renderAccounts(`,
		`onclick="fillAccount(`, `onclick="toggleAccount(`, `onclick="toggleDetail(`,
		`onclick="rotate(`, `onclick="del(`, `onclick="probe()`, `onclick="prune()`,
		`onclick="retireExit(`, `onclick="reviveExit(`,
		`onsubmit="saveToken()`, `onclick="logout()`, `onclick="load()`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}

	status := decode(t, authed(s, http.MethodGet, "/admin/api/status", ""))
	for _, key := range []string{"upstreams", "listen", "allowed_hosts", "state_file", "free_tier", "policy", "health", "exits_retired"} {
		if _, ok := status[key]; !ok {
			t.Errorf("status is missing %q", key)
		}
	}
	for obj, keys := range map[string][]string{
		"policy": {"max_attempts", "rotate_on_status", "rotate_on_transport", "max_body_bytes", "stream_idle_timeout", "upstream_timeout"},
		"health": {"interval", "timeout", "failure_threshold", "quarantine", "max_backoff", "max_latency", "retire_after"},
	} {
		sub, _ := status[obj].(map[string]any)
		if sub == nil {
			t.Fatalf("status.%s is not an object", obj)
		}
		for _, key := range keys {
			if _, ok := sub[key]; !ok {
				t.Errorf("status.%s is missing %q", obj, key)
			}
		}
	}
	ft, _ := status["free_tier"].(map[string]any)
	if ft == nil {
		t.Fatal("status.free_tier is not an object")
	}
	for _, key := range []string{"enabled", "models", "learned"} {
		if _, ok := ft[key]; !ok {
			t.Errorf("status.free_tier is missing %q", key)
		}
	}

	accounts := decode(t, authed(s, http.MethodGet, "/admin/api/accounts", ""))
	list, _ := accounts["accounts"].([]any)
	if len(list) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	view, _ := list[0].(map[string]any)
	for _, key := range []string{
		"id", "upstream", "enabled", "proxy", "weight", "has_key", "key_hint",
		"headers", "stats",
	} {
		if _, ok := view[key]; !ok {
			t.Errorf("account view is missing %q", key)
		}
	}
	if got := view["headers"]; got == nil {
		t.Error("account view drops the headers the UI edits back")
	}
}
