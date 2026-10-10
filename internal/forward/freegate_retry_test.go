package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// hasStructuralGate reports whether a request body carries the free-tier
// structural tool gate (bash + read). Prompt injection is no longer used.
func hasStructuralGate(raw string) bool {
	var b struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, tl := range b.Tools {
		name := tl.Function.Name
		if name == "" {
			name = tl.Name
		}
		if name != "" {
			seen[name] = true
		}
	}
	return seen["bash"] && seen["read"]
}

// The free-tier refusal is not the client's problem to solve: it is a verdict on
// the request shape, so the proxy answers it by rewriting the request and trying
// again. A model that is free but missing from the configured list is discovered
// exactly here, which is what keeps the list from having to be right.
func TestRefusedFreeModelIsRetriedInsideTheEnvelope(t *testing.T) {
	var (
		mu      sync.Mutex
		bodies  []string
		uas     []string
		encs    []string
		refused int
	)
	h := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		enveloped := hasStructuralGate(string(raw))
		mu.Lock()
		bodies = append(bodies, string(raw))
		uas = append(uas, r.Header.Get("User-Agent"))
		encs = append(encs, r.Header.Get("Accept-Encoding"))
		if !enveloped {
			refused++
		}
		mu.Unlock()

		if !enveloped {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleStream)
	}))
	if err := h.reg.Bind("zen-01", "live"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// stream:false also exercises the fold: the upstream only speaks SSE.
	rec := h.post(t, "/v1/chat/completions",
		`{"model":"mystery-free","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if rec.Code != http.StatusOK {
		mu.Lock()
		defer mu.Unlock()
		var flags []string
		for _, raw := range bodies {
			flags = append(flags, map[bool]string{true: "enveloped", false: "raw"}[hasStructuralGate(raw)])
		}
		t.Fatalf("status = %d, attempts = %v, body = %s (a refused free model must be retried, not surfaced)", rec.Code, flags, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "Paris is the capital.") {
		t.Errorf("folded body = %q", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q; the folded reply is one JSON object", rec.Header().Get("Content-Type"))
	}

	// The refusal taught the proxy the model, so the next call must be enveloped
	// up front — one upstream attempt, no wasted 403.
	mu.Lock()
	before := len(bodies)
	mu.Unlock()
	rec = h.post(t, "/v1/chat/completions",
		`{"model":"mystery-free","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	attempts := len(bodies) - before
	refusals := refused
	ua := uas[len(uas)-1]
	enc := encs[len(encs)-1]
	mu.Unlock()
	if attempts != 1 {
		t.Errorf("second call took %d upstream attempts, want 1 (the model should be learned)", attempts)
	}
	if refusals != 1 {
		t.Errorf("upstream refusals = %d, want exactly 1", refusals)
	}
	if !strings.HasPrefix(ua, "opencode/") {
		t.Errorf("upstream User-Agent = %q; the free tier is decided by it", ua)
	}
	// Forwarding the caller's "br, zstd" would hand this proxy a compressed body,
	// and the refusal above would be classified as an auth error instead.
	if strings.Contains(enc, "br") || strings.Contains(enc, "zstd") {
		t.Errorf("upstream Accept-Encoding = %q; compression must be left to the transport", enc)
	}
}
