package forward

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

func sanitizeEngineForTest() *Engine {
	return &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The measured failure classes behind "input[i] did not match any supported
// type": an unknown item type must be dropped (and logged with its type), a
// typeless non-message role must be translated, and clean conversation bodies
// must pass byte-identical.
func TestSanitizeResponsesInput(t *testing.T) {
	e := sanitizeEngineForTest()

	// The exact repro: input[2] wears an unknown type. The call that 400s
	// upstream must come out transformed into a two-item body.
	weird := []byte(`{"model":"m","input":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":"hello"},
	  {"type":"weird_thing","x":1}]}`)
	out := e.sanitizeResponsesInput(weird, "zen-01")
	var obj struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("sanitized body unmarshal: %v", err)
	}
	if len(obj.Input) != 2 {
		t.Fatalf("unknown item must be dropped, got %d items: %s", len(obj.Input), out)
	}

	// A typeless tool-role item carries information: translate, never 400.
	toolRole := []byte(`{"model":"m","input":[
	  {"role":"tool","content":"exit code 0"}]}`)
	out = e.sanitizeResponsesInput(toolRole, "zen-01")
	if !strings.Contains(string(out), "[tool result] exit code 0") ||
		!strings.Contains(string(out), `"role":"user"`) {
		t.Fatalf("tool item must be translated: %s", out)
	}

	// Typeless message items gain an explicit type (deterministic form).
	typeless := []byte(`{"model":"m","input":[{"role":"user","content":"q"}]}`)
	out = e.sanitizeResponsesInput(typeless, "zen-01")
	if !strings.Contains(string(out), `"type":"message"`) {
		t.Fatalf("typeless message must be typed: %s", out)
	}

	// user-role output_text parts are re-stamped input_text; the text stays.
	outs := []byte(`{"model":"m","input":[
	  {"role":"user","content":[{"type":"output_text","text":"echo"}]}]}`)
	out = e.sanitizeResponsesInput(outs, "zen-01")
	if !strings.Contains(string(out), `"input_text"`) || !strings.Contains(string(out), `"echo"`) {
		t.Fatalf("output_text on user must be re-stamped: %s", out)
	}

	// Orphan function items die with the pairing pass; pairs survive.
	orph := []byte(`{"model":"m","input":[
	  {"role":"user","content":"q"},
	  {"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},
	  {"type":"function_call_output","call_id":"GHOST","output":"x"}]}`)
	out = e.sanitizeResponsesInput(orph, "zen-01")
	if strings.Contains(string(out), `GHOST`) {
		t.Fatalf("orphan output must be dropped: %s", out)
	}
	pair := []byte(`{"model":"m","input":[
	  {"type":"function_call","call_id":"c1","name":"bash","arguments":"{}"},
	  {"type":"function_call_output","call_id":"c1","output":"done"}]}`)
	out = e.sanitizeResponsesInput(pair, "zen-01")
	if !strings.Contains(string(out), "call_id") || !strings.Contains(string(out), "done") {
		t.Fatalf("a valid pair must survive: %s", out)
	}

	// A bare string element is wrapped into a user message.
	bare := []byte(`{"model":"m","input":["just a question"]}`)
	out = e.sanitizeResponsesInput(bare, "zen-01")
	if !strings.Contains(string(out), `"type":"message"`) || !strings.Contains(string(out), "just a question") {
		t.Fatalf("bare string item must be wrapped: %s", out)
	}

	// reasoning / item_reference pass untouched.
	native := []byte(`{"model":"m","input":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"t"}]},
	  {"type":"item_reference","id":"item_1"}]}`)
	if out := e.sanitizeResponsesInput(native, "zen-01"); string(out) != string(native) {
		t.Fatalf("accepted shapes must pass byte-identical, got %s", out)
	}

	// Bodies without an input array are untouched (chat path safety).
	chat := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if out := e.sanitizeResponsesInput(chat, "zen-01"); string(out) != string(chat) {
		t.Fatalf("chat body must pass through: %s", out)
	}

	// The same body sanitizes identically twice — the property that keeps the
	// synthesized conversation key stable across turns.
	if a := e.sanitizeResponsesInput(weird, "zen-01"); string(a) != out0Of(e, weird) {
		// cross-check done via determinism test below
		_ = a
	}
	if a, b := e.sanitizeResponsesInput(weird, "zen-01"), e.sanitizeResponsesInput(weird, "zen-01"); string(a) != string(b) {
		t.Fatal("sanitization must be deterministic")
	}
}

func out0Of(e *Engine, b []byte) string { return string(e.sanitizeResponsesInput(b, "zen-01")) }
