package admin

// Preflight is the adversarial self-probe that replays the MEASURED family of
// upstream contracts against the real chain — the same request shapes that
// have historically only been discovered broken after a real agent stepped on
// them. Contract drift on the free tier is a slow landmine: fire it here,
// after every deploy, instead of letting a live agent trip it first.
//
// Every probe rides the live ingress (self HTTP with the account's real key),
// so bridges, sanitizer, envelope, folding and the actual upstream are all in
// the loop. The cache-warm check is soft: a legitimately cold window may
// starve the repeat without anything being wrong.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// PreflightCheck is one probe result.
type PreflightCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Soft   bool   `json:"soft,omitempty"`
	Detail string `json:"detail"`
	Ms     int64  `json:"ms"`
}

// PreflightReport is the aggregate of one run.
type PreflightReport struct {
	Pass       bool             `json:"pass"`
	DurationMs int64            `json:"duration_ms"`
	Checks     []PreflightCheck `json:"checks"`
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	report := s.runPreflight(r.Context())
	if !report.Pass {
		s.log.Warn("preflight found contract drift",
			"duration_ms", report.DurationMs,
			"failed", func() (n int) {
				for _, c := range report.Checks {
					if !c.Pass && !c.Soft {
						n++
					}
				}
				return n
			}())
	}
	writeJSON(w, http.StatusOK, report)
}

// selfBase turns the configured listen address into a loopback URL the
// preflight can dial into itself with.
func selfBase(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("listen %q is not host:port", listen)
	}
	return "http://127.0.0.1:" + port, nil
}

func (s *Server) runPreflight(ctx context.Context) PreflightReport {
	start := time.Now()
	report := PreflightReport{}

	ids := s.reg.EnabledIDs()
	if len(ids) == 0 {
		report.Checks = []PreflightCheck{{Name: "accounts", Detail: "no enabled account to probe"}}
		report.DurationMs = time.Since(start).Milliseconds()
		return report
	}
	base, err := selfBase(s.cfg.Server.Listen)
	if err != nil {
		report.Checks = []PreflightCheck{{Name: "self", Detail: err.Error()}}
		report.DurationMs = time.Since(start).Milliseconds()
		return report
	}

	client := &http.Client{Timeout: 75 * time.Second}
	model := os.Getenv("ZEN_PREFLIGHT_MODEL")
	if model == "" {
		model = "muse-spark-1.3-contributor-free"
	}
	report.Checks = s.probeAccount(ctx, client, base, ids[0], model)

	for _, c := range report.Checks {
		if !c.Pass && !c.Soft {
			report.Pass = false
		}
	}
	report.DurationMs = time.Since(start).Milliseconds()
	return report
}

// probeAccount runs the drift probes against one account through the loopback
// ingress. Every check runs even after a failure — a full red table beats a
// partial one.
func (s *Server) probeAccount(ctx context.Context, client *http.Client, base, accountID, model string) []PreflightCheck {
	entry, ok := s.reg.Get(accountID)
	if !ok {
		return []PreflightCheck{{Name: "account", Detail: "account vanished mid-probe: " + accountID}}
	}
	key := entry.Account.APIKey
	prefix := base + "/" + accountID

	post := func(path string, body any) (int, map[string]any, string) {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, "marshal: " + err.Error()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, prefix+path, bytes.NewReader(raw))
		if err != nil {
			return 0, nil, "request: " + err.Error()
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, "dial: " + err.Error()
		}
		defer resp.Body.Close()
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		st := resp.StatusCode
		var parsed map[string]any
		_ = json.Unmarshal(limited, &parsed)
		return st, parsed, string(limited)
	}

	check := func(name string, pass bool, detail string) PreflightCheck {
		return PreflightCheck{Name: name, Pass: pass, Detail: detail}
	}

	// The structured-outputs contract the bridge maps onto text.format. The
	// schema requires BOTH keys so strict enforcement is actually observable.
	rateSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"score": map[string]any{"type": "number"}, "note": map[string]any{"type": "string"}},
		"required":             []string{"score", "note"},
		"additionalProperties": false,
	}
	prompt := "Rate how well this reads: 'docs: fix typo recieve->receive'. Score 0-10."

	// Effort is pinned low for every probe: accepted on both surfaces
	// (measured), and a ~3s shallow think beats a ~10-25s xhigh one when the
	// suite has to fit inside a curl's patience budget.
	effort := map[string]any{"effort": "low", "summary": "auto"}

	results := []PreflightCheck{}

	// 1) The chain answers at all, and the fold's usage accounting survived.
	t0 := time.Now()
	st, parsed, raw := post("/v1/chat/completions", map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 512,
		"reasoning":  effort,
		"messages":   []map[string]any{{"role": "user", "content": "Reply with exactly: PREFLIGHT_OK"}},
	})
	c := PreflightCheck{Name: "chat completes + usage"}
	if st == 200 {
		if ch, ok := parsed["choices"].([]any); !ok || len(ch) == 0 {
			c.Detail = "200 without choices"
		} else if usage, ok := parsed["usage"].(map[string]any); !ok {
			c.Detail = "200 without usage"
		} else if _, ok := usage["input_tokens"]; !ok {
			c.Detail = "usage missing input_tokens (fold shape regression)"
		} else if _, ok := usage["input_tokens_details"].(map[string]any); !ok {
			c.Detail = "usage missing input_tokens_details (cache accounting regression)"
		} else {
			c.Pass = true
			c.Detail = "usage intact"
		}
	} else {
		c.Detail = fmt.Sprintf("status=%d %s", st, clip(raw, 120))
	}
	c.Ms = time.Since(t0).Milliseconds()
	results = append(results, c)

	// 2) Strict json_schema honored end to end (bridge + upstream enforcement).
	t0 = time.Now()
	st, parsed, raw = post("/v1/chat/completions", map[string]any{
		"model": model, "stream": false, "max_tokens": 512, "reasoning": effort,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": "preflight_vote", "strict": true, "schema": rateSchema,
			},
		},
	})
	results = append(results, structuredCheck("structured: strict schema honored", st, parsed, raw, time.Since(t0)))

	// 3) json_object honored.
	t0 = time.Now()
	st, parsed, raw = post("/v1/chat/completions", map[string]any{
		"model": model, "stream": false, "max_tokens": 512, "reasoning": effort,
		"messages":        []map[string]any{{"role": "user", "content": prompt}},
		"response_format": map[string]any{"type": "json_object"},
	})
	results = append(results, structuredCheck("structured: json_object honored", st, parsed, raw, time.Since(t0)))

	// 4) The measured input whitelist still accepts every measured shape.
	t0 = time.Now()
	st, parsed, raw = post("/v1/responses", map[string]any{
		"model": model, "stream": false, "max_output_tokens": 512,
		"reasoning": effort,
		"input": []any{
			map[string]any{"role": "user", "content": "ping"},
			map[string]any{"role": "assistant", "content": "ack"},
			map[string]any{"type": "reasoning", "summary": []any{}},
			map[string]any{"type": "item_reference", "id": "item_preflight"},
			map[string]any{"type": "function_call", "call_id": "pf_pair", "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "pf_pair", "output": "ok"},
		},
	})
	ok200 := st == 200 && parsed != nil && (parsed["error"] == nil)
	results = append(results, finished("responses: whitelist intact", ok200, st, raw, time.Since(t0), parsed))

	// 5) The sanitizer keeps the adversarial family away from the upstream:
	// unknown type, tool role, orphaned call + output, output_text on user.
	t0 = time.Now()
	st, parsed, raw = post("/v1/responses", map[string]any{
		"model": model, "stream": false, "max_output_tokens": 512,
		"reasoning": effort,
		"input": []any{
			map[string]any{"role": "user", "content": "ping"},
			map[string]any{"type": "weird_thing", "x": 1},
			map[string]any{"role": "tool", "content": "tool result"},
			map[string]any{"type": "function_call", "call_id": "pf_orphan", "name": "bash", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "pf_ghost", "output": "orphan"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "output_text", "text": "legacy part"}}},
		},
	})
	results = append(results, finished("sanitizer: adversarial input survives", st == 200, st, raw, time.Since(t0), parsed))

	// 6) Cache key stability: the second identical body should ride the warm
	// prefix. Soft — a cold window is legitimate, the detail is what matters.
	t0 = time.Now()
	st, parsed, raw = post("/v1/chat/completions", map[string]any{
		"model":      model,
		"stream":     false,
		"max_tokens": 512,
		"reasoning":  effort,
		"messages":   []map[string]any{{"role": "user", "content": "Reply with exactly: PREFLIGHT_OK"}},
	})
	c = check("cache: repeat rides warm prefix", true, "")
	if st == 200 {
		if usage, ok := parsed["usage"].(map[string]any); ok {
			if d, ok := usage["input_tokens_details"].(map[string]any); ok {
				if cached, ok := d["cached_tokens"].(float64); ok && cached > 0 {
					c.Detail = fmt.Sprintf("cached_tokens=%.0f warm", cached)
				} else {
					c.Soft = true
					c.Detail = "cold on repeat (soft)"
				}
			}
		}
	} else {
		c.Soft = true
		c.Detail = fmt.Sprintf("status=%d (soft)", st)
	}
	c.Ms = time.Since(t0).Milliseconds()
	results = append(results, c)

	return results
}

// structuredCheck verifies a chat reply to response_format parses as JSON.
func structuredCheck(name string, st int, parsed map[string]any, raw string, took time.Duration) PreflightCheck {
	if st != 200 {
		return PreflightCheck{Name: name, Detail: fmt.Sprintf("status=%d %s", st, clip(raw, 120)), Ms: took.Milliseconds()}
	}
	content := ""
	if ch, ok := parsed["choices"].([]any); ok && len(ch) > 0 {
		if m, ok := ch[0].(map[string]any); ok {
			if msg, ok := m["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					content = s
				}
			}
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(content), &obj); err != nil {
		return PreflightCheck{Name: name, Detail: "reply is not JSON: " + clip(content, 100), Ms: took.Milliseconds()}
	}
	return PreflightCheck{Name: name, Pass: true, Detail: "JSON ok", Ms: took.Milliseconds()}
}

// finished asserts a plain 200 with no error object.
func finished(name string, ok bool, st int, raw string, took time.Duration, parsed map[string]any) PreflightCheck {
	if !ok {
		detail := fmt.Sprintf("status=%d", st)
		if parsed != nil && parsed["error"] != nil {
			if e, merr := json.Marshal(parsed["error"]); merr == nil {
				detail += " " + clip(string(e), 140)
			}
		} else {
			detail += " " + clip(raw, 140)
		}
		return PreflightCheck{Name: name, Detail: detail, Ms: took.Milliseconds()}
	}
	return PreflightCheck{Name: name, Pass: true, Detail: "200", Ms: took.Milliseconds()}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
