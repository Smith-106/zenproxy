package forward

import (
	"encoding/json"
	"strings"
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

// instructionsOf reads the rewritten `instructions` field as text.
func instructionsOf(t *testing.T, obj map[string]json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(obj["instructions"], &s); err != nil {
		t.Fatalf("unmarshal instructions: %v", err)
	}
	return s
}

func flatTool(name string) map[string]any {
	return map[string]any{
		"type":       "function",
		"name":       name,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func TestPrepareResponsesUsesBuildTemplateWithTools(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "muse-spark-1.3-contributor-free",
		"stream": true,
		"input":  "hi",
		"tools":  []any{flatTool("bash"), flatTool("read")},
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected the free model to be enveloped on /responses")
	}
	if plan.Template != "responses-build" {
		t.Errorf("template = %q, want responses-build", plan.Template)
	}
	if plan.Model != "muse-spark-1.3-contributor-free" {
		t.Errorf("model = %q", plan.Model)
	}
	obj := decode(t, plan.Body)
	if got := instructionsOf(t, obj); !strings.HasPrefix(got, gateBuildChunk) {
		t.Error("instructions must start with the agent prompt chunk")
	}
	// `input` is the caller's content and must survive untouched.
	var input string
	if err := json.Unmarshal(obj["input"], &input); err != nil || input != "hi" {
		t.Errorf("input = %q (err %v), want hi", input, err)
	}
	// The chat-shaped field must not appear on this surface.
	if _, ok := obj["messages"]; ok {
		t.Error("a Responses body must not gain a messages field")
	}
}

// A tool-less Responses request is judged on the title prompt, exactly as a
// tool-less chat request is.
func TestPrepareResponsesUsesTitleTemplateWithoutTools(t *testing.T) {
	raw := body(t, map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"input":  "hi",
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected the free model to be enveloped on /responses")
	}
	if plan.Template != "responses-title" {
		t.Errorf("template = %q, want responses-title", plan.Template)
	}
	if got := instructionsOf(t, decode(t, plan.Body)); !strings.HasPrefix(got, gateTitleChunk) {
		t.Error("instructions must start with the title prompt chunk")
	}
}

// The caller's own instructions are prefixed, not replaced: the gate needs the
// chunk first, and the caller still needs its own prompt to reach the model.
func TestPrepareResponsesKeepsCallersInstructions(t *testing.T) {
	raw := body(t, map[string]any{
		"model":        "big-pickle",
		"stream":       true,
		"input":        "hi",
		"instructions": "You are a pirate.",
	})
	plan := responsesFreeTier(t).Prepare("/zen/v1/responses", raw, false)
	if plan == nil {
		t.Fatal("expected an envelope")
	}
	got := instructionsOf(t, decode(t, plan.Body))
	if !strings.HasPrefix(got, gateTitleChunk) {
		t.Fatal("the caller's instructions must not displace the chunk")
	}
	if !strings.HasSuffix(got, "You are a pirate.") {
		t.Error("the caller's own instructions must still reach the model")
	}
	if !strings.Contains(got, gateOverride) {
		t.Error("the injected chunk must still be neutralised by the override")
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

// Responses names tools at the top level. The top-up entries must match that
// shape — a mixed array is not something the upstream ever sees from a real
// client, and the count is what the gate reads.
func TestPrepareResponsesTopsUpFlatToolsKeepingTheirShape(t *testing.T) {
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
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(decode(t, plan.Body)["tools"], &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}

	names := map[string]bool{}
	for _, tool := range tools {
		var name string
		if err := json.Unmarshal(tool["name"], &name); err != nil || name == "" {
			t.Fatalf("every entry must name its function at the top level: %s", tool["name"])
		}
		if _, nested := tool["function"]; nested {
			t.Errorf("tool %q was written in the chat shape on the Responses surface", name)
		}
		names[name] = true
	}
	hits := 0
	for _, want := range gateToolNames {
		if names[want] {
			hits++
		}
	}
	if hits < gateMinToolNames {
		t.Errorf("only %d official tool names present, want >= %d", hits, gateMinToolNames)
	}
	// The caller's own tools stay in front so its tool calling keeps working.
	if len(tools) < 2 || !names["bash"] || !names["read"] {
		t.Error("the caller's own tools must be preserved")
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
	// A body that already carries a client prompt must not be wrapped twice.
	already := body(t, map[string]any{"model": "big-pickle", "input": "hi", "instructions": gateBuildChunk})
	if plan := ft.Prepare("/zen/v1/responses", already, false); plan != nil {
		t.Error("an already-enveloped body must not be wrapped again")
	}
	// Disabled means disabled.
	off := NewFreeTier(config.FreeTier{Enabled: false, Models: []string{"big-pickle"}})
	if plan := off.Prepare("/zen/v1/responses", ok, false); plan != nil {
		t.Error("a disabled envelope must not rewrite anything")
	}
}

// Counting tool names has to read both shapes, or a Responses request looks
// tool-less and is handed the title prompt — the wrong one for a request that
// does carry tools.
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
