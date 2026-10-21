package model

import (
	"strings"
	"testing"
)

// An account id becomes a URL path segment (and a proxy credential username),
// so anything that would silently produce an unreachable Base URL must be
// rejected up front rather than 404ing at request time.
func TestValidateAccountID(t *testing.T) {
	valid := []string{"zen-01", "zen_01", "a", "A1", "acc.1", "zen-2024-01"}
	for _, id := range valid {
		if err := ValidateAccountID(id); err != nil {
			t.Errorf("ValidateAccountID(%q) = %v, want nil", id, err)
		}
	}

	invalid := []struct{ id, why string }{
		{"", "empty"},
		{"zen 01", "contains a space"},
		{"zen/01", "contains a slash"},
		{"zen?01", "contains a query separator"},
		{"-zen", "starts with a dash"},
		{".zen", "starts with a dot"},
		{"admin", "collides with /admin"},
		{"Healthz", "collides with /healthz, case-insensitively"},
		{"metrics", "collides with /metrics"},
	}
	for _, c := range invalid {
		if err := ValidateAccountID(c.id); err == nil {
			t.Errorf("ValidateAccountID(%q) = nil, want an error (%s)", c.id, c.why)
		}
	}
}

func TestPanelReasonDropsADialAndKeepsAVendorSentence(t *testing.T) {
	dial := PanelReason(`Post "https://secret.example/v1": proxyconnect tcp://user:pass@10.1.2.3:8080: i/o timeout`)
	if dial != "timeout" || strings.Contains(dial, "secret") || strings.Contains(dial, "pass") {
		t.Fatalf("dial = %q", dial)
	}
	vendor := PanelReason(`HTTP 400: see https://secret.example/docs user:pass@host`)
	if strings.Contains(vendor, "secret") || strings.Contains(vendor, "pass") || !strings.Contains(vendor, "HTTP 400") {
		t.Fatalf("vendor = %q", vendor)
	}
	if got := DialClass("http proxy: network is unreachable"); got != "unreachable" {
		t.Fatalf("unreachable not at the start = %q", got)
	}
}
