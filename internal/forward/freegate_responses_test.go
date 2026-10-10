package forward

import (
	"encoding/json"
	"testing"

	"zenproxy/internal/config"
)

// The Responses surface needs its own envelope because it names everything
// differently: there is no system message, `instructions` carries what chat
// carries as one, and `input` replaces `messages`.
//
// It matters for a concrete reason. Both muse-spark free variants answer on
// /responses and refuse /chat/completions with ModelProtocolUnsupported, so a
// proxy that envelops only chat does not merely degrade those models — it makes
// them look unobtainable, with a 403 FreeTierError that reads like an auth
// problem. These tests pin the envelope that stops that.

func responsesFreeTier(t *testing.T) *FreeTier {
	t.Helper()
	return NewFreeTier(config.FreeTier{
		Enabled: true,
		Models:  []string{"big-pickle", "muse-spark-1.3-contributor-free"},
	})
}

func flatTool(name string) map[string]any {
	return map[string]any{
		"type":       "function",
		"name":       name,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func flatToolNames(t *testing.T, obj map[string]json.RawMessage) []string {
	t.Helper()
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(obj["tools"], &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		var name string
		if err := json.Unmarshal(tool["name"], &name); err != nil || name == "" {
			t.Fatalf("every entry must name its function at the top level: %s", tool["name"])
		}
		if _, nested := tool["function"]; nested {
			t.Errorf("tool %q was written in the chat shape on the Responses surface", name)
		}
		out = append(out, name)
	}
	return out
}

// A Responses request must gain bash+read (flat shape) and tool_choice=auto.
// instructions / input must survive untouched — no prompt injection.
func TestPrepareResponsesInjectsToolsWithoutTouchingInstructions(t *testing.T) {
	raw := body(t, map[string]any{
		"model":        "muse-spark-1.3-contributor-free",
		"stream":       true,
		"input":        "hi",
		"instructions": "You are a pirate.",
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected the free model to be enveloped on /responses")
	}
	if plan.Template != "responses-tools" {
		t.Errorf("template = %q, want responses-tools", plan.Template)
	}
	if plan.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("model = %q", plan.Model)
	}
	obj := decode(t, plan.Body)

	var instructions string
	if err := json.Unmarshal(obj["instructions"], &instructions); err != nil || instructions != "You are a pirate." {
		t.Errorf("instructions = %q (err %v), want the caller's own text untouched", instructions, err)
	}
	var input string
	if err := json.Unmarshal(obj["input"], &input); err != nil || input != "hi" {
		t.Errorf("input = %q (err %v), want hi", input, err)
	}
	if _, ok := obj["messages"]; ok {
		t.Error("a Responses body must not gain a messages field")
	}

	names := flatToolNames(t, obj)
	if len(names) != 2 || names[0] != "bash" || names[1] != "read" {
		t.Errorf("tools = %v, want [bash read]", names)
	}
	var choice string
	if err := json.Unmarshal(obj["tool_choice"], &choice); err != nil || choice != "auto" {
		t.Errorf("tool_choice = %q (err %v), want auto", choice, err)
	}
}

// Client tools stay in front; missing gate names are appended in the flat shape;
// tool_choice is forced to auto even if the client sent something else.
func TestPrepareResponsesTopsUpFlatToolsAndForcesAuto(t *testing.T) {
	raw := body(t, map[string]any{
		"model":       "big-pickle",
		"stream":      true,
		"input":       "hi",
		"tools":       []any{flatTool("custom_search")},
		"tool_choice": "none", // Responses rejects this; envelope must overwrite.
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected an envelope")
	}
	obj := decode(t, plan.Body)
	names := flatToolNames(t, obj)
	if names[0] != "custom_search" {
		t.Errorf("the caller's own tool must stay in front, got %q", names[0])
	}
	hits := 0
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
		for _, g := range gateToolNames {
			if n == g {
				hits++
			}
		}
	}
	if hits < gateMinToolNames {
		t.Errorf("only %d official tool names present, want >= %d", hits, gateMinToolNames)
	}
	if !seen["bash"] || !seen["read"] {
		t.Error("bash and read must be present after top-up")
	}
	var choice string
	if err := json.Unmarshal(obj["tool_choice"], &choice); err != nil || choice != "auto" {
		t.Errorf("tool_choice = %q (err %v), want auto (Responses rejects none)", choice, err)
	}
}

// The upstream refuses a non-streaming request even with a perfect identity, so
// the envelope must ask for a stream while remembering that the caller did not
// want one — the engine folds the stream back for them.
func TestPrepareResponsesForcesStreamAndRemembersTheClientWish(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": false,
		"input":  "hi",
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected an envelope")
	}
	if plan.Stream {
		t.Error("Stream must report what the client asked for, not what we sent")
	}
	var sent bool
	if err := json.Unmarshal(decode(t, plan.Body)["stream"], &sent); err != nil || !sent {
		t.Errorf("upstream stream = %v (err %v), want true", sent, err)
	}
}

// A request that already advertises bash+read must not grow further.
func TestPrepareResponsesDoesNotPadWhenGateNamesAlreadyPresent(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"input":  "hi",
		"tools":  []any{flatTool("bash"), flatTool("read")},
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected an envelope")
	}
	names := flatToolNames(t, decode(t, plan.Body))
	if len(names) != 2 {
		t.Errorf("tools grew to %d without need: %v", len(names), names)
	}
}

// The Responses envelope is scoped by the same rules as the chat one.
func TestPrepareResponsesScope(t *testing.T) {
	ft := responsesFreeTier(t)
	ok := []byte(`{"model":"big-pickle","stream":true,"input":"hi"}`)

	// A body with no input carries no request.
	if plan := ft.Prepare("/zen/v1/responses", []byte(`{"model":"big-pickle","stream":true}`), false); plan != nil {
		t.Error("a body with no input must not be rewritten")
	}
	// Null input is the same as absent.
	if plan := ft.Prepare("/zen/v1/responses", []byte(`{"model":"big-pickle","input":null}`), false); plan != nil {
		t.Error("a null input must not be rewritten")
	}
	// A body that is not JSON is left alone rather than mangled.
	if plan := ft.Prepare("/zen/v1/responses", []byte(`not json`), false); plan != nil {
		t.Error("an unparseable body must not be rewritten")
	}
	// The path may or may not end in a slash.
	if plan := ft.Prepare("/zen/v1/responses/", ok, false); plan == nil {
		t.Error("a trailing slash must not defeat the match")
	}
	// A model outside the free list is not enveloped unless the caller forces it.
	if plan := ft.Prepare("/zen/v1/responses", []byte(`{"model":"gpt-5","input":"hi"}`), false); plan != nil {
		t.Error("an unknown model must be left alone")
	}
	if plan := ft.Prepare("/zen/v1/responses", []byte(`{"model":"gpt-5","input":"hi"}`), true); plan == nil {
		t.Error("a forced retry must envelop even an unknown model")
	}
	// Disabled means disabled.
	off := NewFreeTier(config.FreeTier{Enabled: false, Models: []string{"big-pickle"}})
	if plan := off.Prepare("/zen/v1/responses", ok, false); plan != nil {
		t.Error("a disabled envelope must not rewrite anything")
	}
}

// Counting tool names has to read both shapes.
func TestToolNameOfReadsBothShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"chat nests the name", `{"type":"function","function":{"name":"bash"}}`, "bash"},
		{"responses flattens it", `{"type":"function","name":"read"}`, "read"},
		{"neither", `{"type":"function"}`, ""},
		{"not an object", `"bash"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolNameOf(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("toolNameOf(%s) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestDetectToolShape(t *testing.T) {
	if got := detectToolShape([]json.RawMessage{json.RawMessage(`{"type":"function","name":"read"}`)}); got != toolShapeFlat {
		t.Error("a top-level name means the flat shape")
	}
	if got := detectToolShape([]json.RawMessage{json.RawMessage(`{"function":{"name":"bash"}}`)}); got != toolShapeNested {
		t.Error("a nested name means the chat shape")
	}
	// Nothing to learn from means the historical default.
	if got := detectToolShape(nil); got != toolShapeNested {
		t.Error("an empty list must default to the chat shape")
	}
}

// The dispatch itself: two surfaces enveloped, everything else untouched.
func TestSurfaceForRecognisesBothSurfaces(t *testing.T) {
	for _, tc := range []struct {
		path string
		want surface
	}{
		{"/zen/v1/chat/completions", surfaceChat},
		{"/zen/v1/chat/completions/", surfaceChat},
		{"/zen/v1/responses", surfaceResponses},
		{"/zen/v1/responses/", surfaceResponses},
		{"/zen/go/v1/responses", surfaceResponses},
		{"/zen/v1/messages", surfaceNone},
		{"/zen/v1/models", surfaceNone},
		// A path that merely contains the word must not match.
		{"/zen/v1/responses/extra", surfaceNone},
		{"/zen/v1/chat/completions/extra", surfaceNone},
	} {
		if got := surfaceFor(tc.path); got != tc.want {
			t.Errorf("surfaceFor(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestPrepareResponsesRaisesTinyMaxOutputTokens(t *testing.T) {
	raw := body(t, map[string]any{
		"model":             "muse-spark-1.3-contributor-free",
		"input":             "hello",
		"max_output_tokens": 32,
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	obj := decode(t, plan.Body)
	var got int
	if err := json.Unmarshal(obj["max_output_tokens"], &got); err != nil {
		t.Fatalf("max_output_tokens: %v", err)
	}
	if got != gateMinMaxTokens {
		t.Errorf("max_output_tokens = %d, want floor %d", got, gateMinMaxTokens)
	}
}
