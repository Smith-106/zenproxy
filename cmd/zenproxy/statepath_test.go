package main

import (
	"strings"
	"testing"
)

func TestPoolStatePathIsPerHost(t *testing.T) {
	got := poolStatePath("/data/state.json")
	if got == "/data/exits-state.json" {
		t.Fatalf("replicas would share one file: %s", got)
	}
	if !strings.Contains(got, "exits-state-") {
		t.Fatalf("path = %s, want a per-host exits-state file", got)
	}
	if poolStatePath("") != "" {
		t.Fatal("empty state file should disable persistence")
	}
}
