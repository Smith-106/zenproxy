package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// 表单把备注清成空字符串时，旧备注必须真的消失。没带 name 的启用/禁用不能跟着清。
func TestClearingTheNoteRemovesIt(t *testing.T) {
	cfg := config.Default()
	s, reg := testServer(t, cfg, nil)
	if err := reg.Upsert(model.Account{
		ID: "zen-01", Name: "n", Upstream: "zen", APIKey: "sk-0123456789abcdef", Proxy: "auto", Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	rec := authed(s, http.MethodPost, "/admin/api/accounts", `{"id":"zen-01","name":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ent, _ := reg.Get("zen-01")
	if ent.Account.Name != "" {
		t.Fatalf("name = %q, want empty", ent.Account.Name)
	}
	rec = authed(s, http.MethodPost, "/admin/api/accounts", `{"id":"zen-01","name":"kept","enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = authed(s, http.MethodPost, "/admin/api/accounts", `{"id":"zen-01","enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	ent, _ = reg.Get("zen-01")
	if ent.Account.Name != "kept" || !ent.Account.Enabled {
		t.Fatalf("toggle disturbed the note: %+v", ent.Account)
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
		`renderStat(`, `renderFree(`, `renderConfig(`, `renderExits(`, `renderAccounts(`, `replicaLine(`, `accountReplicaLine(`,
		`尝试失败`, `成功/请求`, `pool_shape`, `隔离原因`, `quarantineWord`, `raw === 'timeout'`, `exitIPWord`, `观察 · `, `e.slow`, `可走免费`, `含观察`, `到点自己回来`, `outage_breaker`, `统一入口关着`, `限流时先换出口`, `prune_btn`, `偏慢但还在用的也算`, `两边不一致`, `另一副本多退役`, `另一副本已退役`, `另一副本还在用`, `id="peer_ids"`, `这边还健康，但排在干净出口后面`, `peer_retired_class`,
		`这边隔离中，现在不参与轮换`, `挑选时会把它排在干净出口后面`,
		`TLS 累计 `, `连续失败 `, `retireWord`, `还没问过出口地址`, `accountIPCell`, `bound_exit_ip_note`, `bound_slow`, `bound_gate`, `bound_prunable`, `boundFlags`,
		`setExitFilter`, `exit_filters`, `这个筛选下没有出口`, `peer_quota_here`, `另一边没冷却`,
		`正在被免费准入的不退休`, `tls_poison_retire_after`,
		`代理地址`, `不是流量出去时的公网 IP`, `e.quota_reason`,
		`failureThreshold`, `次就隔离`, `隔离还剩`, `不到 1 分钟`, `napLeft`,
		`proxyCell`, `粘住`, `还能服务当前模型时不换`, `还没粘住`,
		`overCap`, `maxLatencyMS`, `超出 `,
		`openDetailIds`, `paintRows`,
		`sticky_line`, `个账号粘在`, `最挤的一个上面有`,
		`换出口`, `公网 IP 不一定变了`, `uptimeWord`,
		`sessionPins`, `点换出口会把这个账号的对话钉一起改到新出口`, `pins_moved`,
		`session_pins`, `个对话钉在`,
		`weight_hint`, `poolUsesWeight`, `不看权重`, `权重不参与挑选`,
		`不再把出口 ID 铺开`, `另一边记下的是`, `这边在冷却、另一边没有`,
		`latencyCell`, `还没量过`, `这次进程还没有一次成功计时`,
		`untimed_note`, `个这次还没量过延迟`,
		`空着保存会清空`, `钥匙留空才是不改`, `备注和自定义头空着保存都会清空`,
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
	for _, key := range []string{"upstreams", "listen", "allowed_hosts", "state_file", "replicas", "replica_split", "account_replicas", "success", "free_tier", "policy", "health", "exits_retired", "exits_slow", "exits_watching", "exits_gate", "quarantine", "quota", "region", "outage_breaker", "session_pins"} {
		if _, ok := status[key]; !ok {
			t.Errorf("status is missing %q", key)
		}
	}
	for obj, keys := range map[string][]string{
		"policy": {"max_attempts", "rotate_on_status", "rotate_on_transport", "max_body_bytes", "stream_idle_timeout", "upstream_timeout"},
		"health": {"interval", "timeout", "failure_threshold", "quarantine", "max_backoff", "max_latency", "max_latency_ms", "retire_after", "tls_poison_retire_after"},
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
	health, _ := status["health"].(map[string]any)
	if health["tls_poison_retire_after"] != float64(proxypool.PoisonRetireAfter) {
		t.Errorf("tls_poison_retire_after = %v, want %d", health["tls_poison_retire_after"], proxypool.PoisonRetireAfter)
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

// --- quota cooldown on the admin surface ------------------------------------

// quotaServer builds a server over a pool with real exits, which the shared
// fixture cannot do: it is built with an empty pool, and a cooldown has nothing
// to attach to without one.
func quotaServer(t *testing.T, exits ...string) (*Server, *proxypool.Pool) {
	t.Helper()
	specs := make([]proxypool.Spec, 0, len(exits))
	for _, id := range exits {
		specs = append(specs, proxypool.Spec{ID: id, URL: "http://127.0.0.1:9"})
	}
	pool, err := proxypool.NewPool(specs, config.Health{}, discardLogger())
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	reg := registry.New("", pool, discardLogger())
	cfg := config.Config{}
	cfg.Server.AdminToken = "sekret"
	return New(reg, pool, cfg, nil, discardLogger()), pool
}

func TestStatusReportsWhichExitsAreOutOfQuota(t *testing.T) {
	s, pool := quotaServer(t, "slot-01", "slot-02")
	pool.CoolForQuota("slot-01", time.Now().Add(time.Hour), "429")

	body := decode(t, authed(s, http.MethodGet, "/admin/api/status", ""))
	quota, ok := body["quota"].(map[string]any)
	if !ok {
		t.Fatalf("status has no quota block: %v", body)
	}
	if got := quota["count"].(float64); got != 1 {
		t.Errorf("quota.count = %v, want 1", got)
	}
	if got := quota["total"].(float64); got != 2 {
		t.Errorf("quota.total = %v, want 2", got)
	}
	cooled, _ := quota["cooled"].([]any)
	if len(cooled) != 1 {
		t.Fatalf("quota.cooled = %v, want one entry", cooled)
	}
	entry := cooled[0].(map[string]any)
	if entry["exit_id"] != "slot-01" {
		t.Errorf("quota.cooled[0].exit_id = %v", entry["exit_id"])
	}
	// The operator needs the remaining time, not just the fact.
	if entry["after"] == nil || entry["after"] == "" {
		t.Error("quota.cooled[0].after is empty; the panel would have to do date arithmetic")
	}

	// And the per-exit listing must distinguish spent from broken.
	exits := decode(t, authed(s, http.MethodGet, "/admin/api/proxies", ""))["exits"].([]any)
	for _, raw := range exits {
		e := raw.(map[string]any)
		switch e["id"] {
		case "slot-01":
			if e["quota_until"] == nil {
				t.Error("the cooled exit has no quota_until in /admin/api/proxies")
			}
			if e["healthy"] != true {
				t.Error("a quota-cooled exit must still report healthy")
			}
			if e["quarantined_until"] != nil {
				t.Error("a quota cooldown must not be reported as a quarantine")
			}
		case "slot-02":
			if e["quota_until"] != nil {
				t.Error("an uncooled exit reported a cooldown")
			}
		}
	}
}

func TestUncoolEndsACooldownAndSaysWhatItDid(t *testing.T) {
	s, pool := quotaServer(t, "slot-01")
	pool.CoolForQuota("slot-01", time.Now().Add(time.Hour), "429")

	body := decode(t, authed(s, http.MethodPost, "/admin/api/proxies/slot-01/uncool", ""))
	if body["changed"] != true {
		t.Fatalf("uncool reported changed=%v, want true", body["changed"])
	}
	if _, _, cool := pool.QuotaCooled("slot-01"); cool {
		t.Fatal("the exit is still cooled after uncool")
	}
	// Idempotent, and honest about it: a second call is a no-op, not an error.
	body = decode(t, authed(s, http.MethodPost, "/admin/api/proxies/slot-01/uncool", ""))
	if body["changed"] != false {
		t.Errorf("second uncool reported changed=%v, want false", body["changed"])
	}
	// An unknown exit is a typo and must be distinguished from a no-op, or the
	// operator cannot tell whether they named the wrong thing.
	rec := authed(s, http.MethodPost, "/admin/api/proxies/nope/uncool", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("uncool on an unknown exit = %d, want 404", rec.Code)
	}
}
