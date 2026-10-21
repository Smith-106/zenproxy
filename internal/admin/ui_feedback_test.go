package admin

import "strings"
import "testing"

func TestDashboardFailureCopyMatchesTheClasses(t *testing.T) {
	for _, want := range []string{
		"region: ['warn', '地区拒绝']",
		"policy_gate: ['warn', '出口被风控']",
		"function shortReason",
		"function scrubReason",
		"出口连不上游",
		"出口 TLS 失败",
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(dashboardHTML, ".slice(0, 42)") {
		t.Fatal("account errors are still truncated to the useless URL prefix")
	}
}
