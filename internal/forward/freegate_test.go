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

// A request without tools must carry the title prompt: that is the shape the
// upstream accepts for a tool-less body.
func TestPrepareUsesTitleTemplateWithoutTools(t *testing.T) {
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
	if plan.Template != "title" {
		t.Errorf("template = %q, want title", plan.Template)
	}
	if plan.Model != "big-pickle" {
		t.Errorf("model = %q", plan.Model)
	}
	obj := decode(t, plan.Body)
	msgs := msgsOf(t, obj)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want the injected one plus two", len(msgs))
	}
	if msgs[0].Role != "system" || !strings.HasPrefix(msgs[0].Content, gateTitleChunk) {
		t.Error("the injected message must lead with the title prompt chunk")
	}
	if !strings.HasSuffix(msgs[0].Content, gateOverride) {
		t.Error("the injected chunk must be followed by the override")
	}
	// The client's own messages must survive, in order, byte for byte.
	if msgs[1].Content != "You are a pirate." || msgs[2].Content != "hi" {
		t.Errorf("client messages were disturbed: %+v", msgs)
	}
}

// A request with tools must carry the agent prompt instead; the two are not
// interchangeable, and getting it wrong is a 403.
func TestPrepareUsesBuildTemplateWithTools(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "bash"},
		}},
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	if plan.Template != "build" {
		t.Errorf("template = %q, want build", plan.Template)
	}
	obj := decode(t, plan.Body)
	msgs := msgsOf(t, obj)
	if !strings.HasPrefix(msgs[0].Content, gateBuildChunk) {
		t.Error("a request with tools must be enveloped with the agent prompt")
	}
	if _, ok := obj["tools"]; !ok {
		t.Error("tools must survive the rewrite")
	}
}

// An explicitly empty tool list is not "has tools": the upstream refuses a
// title-prompt request only when tools are actually present.
func TestPrepareTreatsEmptyToolListAsAbsent(t *testing.T) {
	raw := []byte(`{"model":"big-pickle","stream":true,"tools":[],"messages":[{"role":"user","content":"hi"}]}`)
	plan := testFreeTier(t).Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil || plan.Template != "title" {
		t.Fatalf("plan = %+v, want the title template", plan)
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

// A request that carries tools is judged against the agent prompt *and* must
// advertise enough of the client's tool names. Four names are refused, five
// pass, so the proxy tops the list up instead of dropping the caller's tools.
func TestPrepareTopsUpToolNames(t *testing.T) {
	ft := testFreeTier(t)
	raw := body(t, map[string]any{
		"model":    "big-pickle",
		"stream":   true,
		"tools":    []map[string]any{{"type": "function", "function": map[string]any{"name": "get_weather"}}},
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	plan := ft.Prepare("/zen/v1/chat/completions", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	obj := decode(t, plan.Body)
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(obj["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	if tools[0].Function.Name != "get_weather" {
		t.Errorf("the caller's own tool must stay in front, got %q", tools[0].Function.Name)
	}
	hits := 0
	for _, tl := range tools {
		for _, g := range gateToolNames {
			if tl.Function.Name == g {
				hits++
			}
		}
	}
	if hits < gateMinToolNames {
		t.Errorf("only %d of the client's tool names survived; the upstream insists on %d", hits, gateMinToolNames)
	}
	seen := map[string]bool{}
	for _, tl := range tools {
		if seen[tl.Function.Name] {
			t.Errorf("duplicate tool name %q", tl.Function.Name)
		}
		seen[tl.Function.Name] = true
	}

	// A client that already advertises the names must not be padded: the extra
	// entries cost tokens and invite the model to call a tool nobody implements.
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

// Paid models must travel unmodified: injecting 1600 characters of prompt into
// a model that never needed it would cost tokens and skew the answer.
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

// A real OpenCode client already satisfies the matcher; wrapping it again would
// only add tokens.
func TestPrepareSkipsRequestsThatAlreadyCarryAPrompt(t *testing.T) {
	ft := testFreeTier(t)
	for _, marker := range []string{"You are a title generator", "You are opencode, an interactive CLI tool"} {
		raw := body(t, map[string]any{
			"model":    "big-pickle",
			"stream":   true,
			"messages": []map[string]string{{"role": "system", "content": marker + " ..."}, {"role": "user", "content": "hi"}},
		})
		if plan := ft.Prepare("/zen/v1/chat/completions", raw, false); plan != nil {
			t.Errorf("%q: an already enveloped request must be left alone", marker)
		}
	}
}

func TestPrepareScope(t *testing.T) {
	ft := testFreeTier(t)
	chat := []byte(`{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// Only the chat-completions shape has been characterised.
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

// The injected chunk is data that was extracted from a capture; a truncated or
// retyped constant is exactly the failure mode that costs every request.
func TestGateChunksLookLikeTheRealPrompts(t *testing.T) {
	for name, chunk := range map[string]string{"title": gateTitleChunk, "build": gateBuildChunk} {
		if len(chunk) < 1200 {
			t.Errorf("%s chunk is %d characters; the upstream stops recognising the request below ~1200", name, len(chunk))
		}
		if strings.Contains(chunk, "%!") || strings.Contains(chunk, "\\n") {
			t.Errorf("%s chunk looks mis-escaped", name)
		}
	}
	if !strings.HasPrefix(gateTitleChunk, "You are a title generator") {
		t.Error("title chunk must be the head of the real prompt")
	}
	if !strings.HasPrefix(gateBuildChunk, "You are opencode, an interactive CLI tool") {
		t.Error("build chunk must be the head of the real prompt")
	}
}
