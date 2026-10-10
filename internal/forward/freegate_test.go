package forward

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

func testFreeTier(t *testing.T) *FreeTier {
	t.Helper()
	return NewFreeTier(config.FreeTier{Enabled: true, Models: []string{"big-pickle"}})
}

func body(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	return obj
}

func msgsOf(t *testing.T, obj map[string]json.RawMessage) []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
} {
	t.Helper()
	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(obj["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return msgs
}

func nestedToolNames(t *testing.T, obj map[string]json.RawMessage) []string {
	t.Helper()
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(obj["tools"], &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Function.Name)
	}
	return out
}

// A tool-less chat request must gain bash+read stubs and tool_choice=none.
// Messages must survive byte for byte — no prompt injection.
func TestPrepareInjectsToolsWithoutTouchingMessages(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a pirate."},
			{"role": "user", "content": "hi"},
		},
	})
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected the free model to be enveloped")
	}
	if plan.Template != "tools" {
		t.Errorf("template = %q, want tools", plan.Template)
	}
	if plan.Model != "big-pickle" {
		t.Errorf("model = %q", plan.Model)
	}
	obj := decode(t, plan.Body)
	msgs := msgsOf(t, obj)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the client's two untouched", len(msgs))
	}
	if msgs[0].Content != "You are a pirate." || msgs[1].Content != "hi" {
		t.Errorf("client messages were disturbed: %+v", msgs)
	}
	names := nestedToolNames(t, obj)
	if len(names) != 2 || names[0] != "bash" || names[1] != "read" {
		t.Errorf("tools = %v, want [bash read]", names)
	}
	var choice string
	if err := json.Unmarshal(obj["tool_choice"], &choice); err != nil || choice != "none" {
		t.Errorf("tool_choice = %q (err %v), want none", choice, err)
	}
}

// Tiny client budgets are raised so reasoning-heavy free models do not come
// back as finish_reason=length with an empty content field.
func TestPrepareRaisesTinyMaxTokens(t *testing.T) {
	raw := body(t, map[string]any{
		"model":      "big-pickle",
		"stream":     false,
		"max_tokens": 32,
		"messages":   []map[string]string{{"role": "user", "content": "hello"}},
	})
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	obj := decode(t, plan.Body)
	var got int
	if err := json.Unmarshal(obj["max_tokens"], &got); err != nil {
		t.Fatalf("max_tokens: %v", err)
	}
	if got != gateMinMaxTokens {
		t.Errorf("max_tokens = %d, want floor %d", got, gateMinMaxTokens)
	}

	rawOK := body(t, map[string]any{
		"model":      "big-pickle",
		"max_tokens": 512,
		"messages":   []map[string]string{{"role": "user", "content": "hello"}},
	})
	objOK := decode(t, testFreeTier(t).Prepare("/zen/v1/chat/completions", rawOK, false).Body)
	var kept int
	if err := json.Unmarshal(objOK["max_tokens"], &kept); err != nil || kept != 512 {
		t.Errorf("budget above the floor must stay put: %d err=%v", kept, err)
	}
}

// A request that already carries tools must keep them in front, gain any
// missing gate names, and leave the client's tool_choice alone.
func TestPreparePreservesClientToolsAndToolChoice(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "get_weather"},
		}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		"messages":    []map[string]string{{"role": "user", "content": "hi"}},
	})
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	if plan.Template != "tools" {
		t.Errorf("template = %q, want tools", plan.Template)
	}
	obj := decode(t, plan.Body)
	names := nestedToolNames(t, obj)
	if names[0] != "get_weather" {
		t.Errorf("the caller's own tool must stay in front, got %q", names[0])
	}
	hits := 0
	for _, n := range names {
		for _, g := range gateToolNames {
			if n == g {
				hits++
			}
		}
	}
	if hits < gateMinToolNames {
		t.Errorf("only %d of the gate tool names present; want >= %d", hits, gateMinToolNames)
	}
	// Client tool_choice must survive untouched.
	var choice map[string]any
	if err := json.Unmarshal(obj["tool_choice"], &choice); err != nil {
		t.Fatal(err)
	}
	fn, _ := choice["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool_choice was disturbed: %+v", choice)
	}
	// No system prompt was injected.
	msgs := msgsOf(t, obj)
	if len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Errorf("messages were disturbed: %+v", msgs)
	}
}

// An explicitly empty tool list is treated as absent: stubs are added and
// tool_choice is forced to none.
func TestPrepareTreatsEmptyToolListAsAbsent(t *testing.T) {
	raw := []byte(`{"model":"big-pickle","stream":true,"tools":[],"messages":[{"role":"user","content":"hi"}]}`)
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil || plan.Template != "tools" {
		t.Fatalf("plan = %+v, want the tools template", plan)
	}
	obj := decode(t, plan.Body)
	var choice string
	if err := json.Unmarshal(obj["tool_choice"], &choice); err != nil || choice != "none" {
		t.Errorf("tool_choice = %q (err %v), want none", choice, err)
	}
}

// The free tier only answers streaming requests, so a non-streaming client must
// be served by asking for a stream and folding it back.
func TestPrepareForcesStreamAndRemembersTheClientWish(t *testing.T) {
	raw := []byte(`{"model":"big-pickle","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	if plan.Stream {
		t.Error("plan.Stream must record that the client did NOT ask for a stream")
	}
	var sent struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(plan.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.Stream {
		t.Error("the upstream body must ask for a stream")
	}

	if p := testFreeTier(t).Prepare("/zen/v1/chat/completions",
		[]byte(`{"model":"big-pickle","messages":[{"role":"user","content":"hi"}]}`), false); p == nil || p.Stream {
		t.Error("an absent stream flag is the OpenAI default of no stream, so the reply must be folded back")
	}
}

// A request that already advertises bash+read must not be padded further.
func TestPrepareDoesNotPadWhenGateNamesAlreadyPresent(t *testing.T) {
	ft := testFreeTier(t)
	var many []map[string]any
	for _, g := range gateToolNames {
		many = append(many, map[string]any{"type": "function", "function": map[string]any{"name": g}})
	}
	rawFull := body(t, map[string]any{
		"model":    "big-pickle",
		"stream":   true,
		"tools":    many,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	full := ft.Prepare("/zen/v1/chat/completions", rawFull, false)
	var got []json.RawMessage
	if err := json.Unmarshal(decode(t, full.Body)["tools"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(many) {
		t.Errorf("tools grew from %d to %d without need", len(many), len(got))
	}
}

// Paid models must travel unmodified: injecting tools into a model that never
// needed them would skew tool calling.
func TestPrepareLeavesUnknownModelsAlone(t *testing.T) {
	ft := testFreeTier(t)
	raw := []byte(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if plan := ft.Prepare("/zen/v1/chat/completions", raw, false); plan != nil {
		t.Fatal("an unknown model must not be enveloped on the first attempt")
	}
	// ...until the upstream refuses it, which is what forced and Learn cover.
	if plan := ft.Prepare("/zen/v1/chat/completions", raw, true); plan == nil {
		t.Fatal("forced must envelope any model")
	}
	ft.Learn("claude-sonnet-4")
	if plan := ft.Prepare("/zen/v1/chat/completions", raw, false); plan == nil {
		t.Fatal("a learned model must be enveloped on the first attempt next time")
	}
	if got := ft.Learned(); len(got) != 1 || got[0] != "claude-sonnet-4" {
		t.Errorf("Learned() = %v", got)
	}
}

func TestPrepareScope(t *testing.T) {
	ft := testFreeTier(t)
	chat := []byte(`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// /messages carries a third shape that has not been characterised, so it is
	// left alone. /responses is enveloped — see freegate_responses_test.go.
	if plan := ft.Prepare("/zen/v1/messages", chat, false); plan != nil {
		t.Error("/messages must not be enveloped")
	}
	// A body that is not a chat request is left alone rather than mangled.
	if plan := ft.Prepare("/zen/v1/chat/completions", []byte(`not json`), false); plan != nil {
		t.Error("an unparseable body must not be rewritten")
	}
	if plan := ft.Prepare("/zen/v1/chat/completions", []byte(`{"model":"big-pickle","stream":true}`), false); plan != nil {
		t.Error("a body with no messages must not be rewritten")
	}
	// The path may or may not end in a slash.
	if plan := ft.Prepare("/zen/v1/chat/completions/", chat, false); plan == nil {
		t.Error("a trailing slash must not defeat the match")
	}
	// Disabled means disabled.
	off := NewFreeTier(config.FreeTier{Enabled: false, Models: []string{"big-pickle"}})
	if plan := off.Prepare("/zen/v1/chat/completions", chat, false); plan != nil {
		t.Error("a disabled envelope must not rewrite anything")
	}
}

// The envelope is headers plus body; the ids are minted per request and must
// have the shape the upstream accepts.
func TestApplyHeadersMintsClientShapedIDs(t *testing.T) {
	hdrs := http.Header{}
	testFreeTier(t).ApplyHeaders(hdrs)

	if got := hdrs.Get("User-Agent"); got != defaultClientUA {
		t.Errorf("User-Agent = %q", got)
	}
	if got := hdrs.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client = %q", got)
	}
	for _, tc := range []struct{ header, prefix string }{
		{"x-opencode-request", "msg_"},
		{"x-opencode-session", "ses_"},
		{"x-opencode-session-id", "ses_"},
	} {
		val := hdrs.Get(tc.header)
		if !strings.HasPrefix(val, tc.prefix) {
			t.Errorf("%s = %q, want the %s prefix", tc.header, val, tc.prefix)
		}
		if len(val) != len(tc.prefix)+26 {
			t.Errorf("%s = %q, want %d characters", tc.header, val, len(tc.prefix)+26)
		}
		for _, r := range val[len(tc.prefix) : 12+len(tc.prefix)] {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Errorf("%s = %q, the middle 12 characters must be lowercase hex", tc.header, val)
			}
		}
		for _, r := range val[len(tc.prefix)+12:] {
			if !strings.ContainsRune(idAlphabet, r) {
				t.Errorf("%s = %q, the suffix must be base62", tc.header, val)
			}
		}
	}
	if hdrs.Get("x-opencode-request") == hdrs.Get("x-opencode-session") {
		t.Error("request and session ids must differ")
	}
}

// The gate constants must stay at the measured minimum: bash + read.
func TestGateToolNamesAreTheMeasuredPair(t *testing.T) {
	if len(gateToolNames) != 2 || gateToolNames[0] != "bash" || gateToolNames[1] != "read" {
		t.Errorf("gateToolNames = %v, want [bash read]", gateToolNames)
	}
	if gateMinToolNames != 2 {
		t.Errorf("gateMinToolNames = %d, want 2", gateMinToolNames)
	}
}
