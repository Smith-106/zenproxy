package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"zenproxy/internal/config"
	"zenproxy/internal/model"
)

// The preflight endpoint must always answer with a full report — even with no
// upstream reachable (as in unit tests), a red table beats a partial one, and
// a structurally empty report would be worthless as a deploy gate.
func TestPreflightFailsClosedWithFullReport(t *testing.T) {
	s, reg := testServer(t, config.Config{}, nil)
	if err := reg.Upsert(model.Account{ID: "zen-01", Upstream: "http://upstream.invalid", Enabled: true}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	rec := authed(s, http.MethodPost, "/admin/api/preflight", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200 with report body", rec.Code)
	}
	var report PreflightReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("report parse: %v", err)
	}
	if len(report.Checks) == 0 {
		t.Fatal("preflight returned no checks")
	}
	for _, c := range report.Checks {
		if c.Name == "" {
			t.Fatal("a check has no name")
		}
	}
	// An unset listen address cannot be probed: the run must report failure,
	// not silently pass.
	if report.Pass {
		t.Fatal("empty-listen report claimed pass")
	}
}
