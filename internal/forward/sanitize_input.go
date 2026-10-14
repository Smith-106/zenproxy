package forward

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The free-tier upstream validates the Responses input array against a strict
// item-type whitelist and answers 400 "`input[i]` did not match any supported
// type" for anything else — measured against the live upstream:
//
//	{"type":"message",...} (string or input_text/array content)      accepted
//	typeless {"role":"assistant"|"user",...,"content":"..."}          accepted
//	{"type":"reasoning",...}  /  {"type":"item_reference",...}        accepted
//	{"type":"function_call"} + paired function_call_output            accepted
//	orphan function_call / function_call_output                       400 pairing
//	{"role":"tool","content":...} (typeless, non-message role)        400 exactly "did not match any supported type"
//	{"type":"<unknown>",...}                                          400 exactly "did not match any supported type"
//	{"role":"user","content":[{"type":"output_text",...}]}            400 part/role mismatch
//
// An agent harness replaying its own transcript dies on the first item it
// stores in any richer shape than the whitelist lists. The sanitizer
// normalizes every measured failure class before preparation and seed
// synthesis — all deterministic, so a conversation's synthesized cache key
// never changes:
//
//   - a bare string item is wrapped into a user message;
//   - a typeless item with a message-shaped role gains "type":"message";
//   - a "tool"/"function" role item is translated to a user message marked
//     "[tool result]" so its information survives;
//   - user-role content parts of type output_text are re-stamped input_text;
//   - function_call/function_call_output survive only as valid pairs —
//     orphans from a truncated transcript are dropped;
//   - any other unknown item type is dropped, logged once at WARN with its
//     actual type so the harness's shape lands on record even though the
//     request now rides through.
//
// Bodies without an input array (chat bodies, string input) pass through
// untouched.
func (e *Engine) sanitizeResponsesInput(body []byte, account string) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return body
	}
	rawIn, ok := top["input"]
	if !ok {
		return body
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawIn, &items); err != nil || len(items) == 0 {
		return body
	}

	stage := make([]inputItem, 0, len(items))
	changed := false
	for idx, raw := range items {
		s, ok := e.sanitizeInputItem(raw, idx, account)
		if !ok {
			changed = true
			continue
		}
		if !bytes.Equal(s.out, raw) {
			changed = true
		}
		stage = append(stage, s)
	}
	// Function pairing: a call or output survives only when BOTH sides of its
	// call_id are present. Truncated transcripts orphan one side; the upstream
	// 400s the whole request over it, so dropping the loose ends is what lets
	// the ride continue.
	ids := map[string][2]int{}
	for _, s := range stage {
		if s.kind == "" {
			continue
		}
		prev := ids[s.callID]
		if s.kind == "call" {
			prev[0]++
		} else {
			prev[1]++
		}
		ids[s.callID] = prev
	}
	final := make([]json.RawMessage, 0, len(stage))
	for _, s := range stage {
		if s.kind != "" {
			n := ids[s.callID]
			if n[0] == 0 || n[1] == 0 {
				e.logSanitizeWarn(account, s.kind+"/orphan:"+s.callID, -1, "unpaired function item dropped")
				changed = true
				continue
			}
		}
		final = append(final, s.out)
	}
	if !changed {
		return body
	}
	rawIn2, err := json.Marshal(final)
	if err != nil {
		return body
	}
	top["input"] = rawIn2
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// inputItem is one staged element of the input array: its (possibly
// rewritten) bytes, and — for function items — the pairing identity.
type inputItem struct {
	out    json.RawMessage
	callID string
	kind   string // "call" | "output" | ""
}

// sanitizeInputItem stages one element: (item, true) = keep as-is or keep
// rewritten, (_, false) = dropped.
func (e *Engine) sanitizeInputItem(raw json.RawMessage, idx int, account string) (inputItem, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return inputItem{}, false
	}
	if trimmed[0] == '"' {
		// A bare string is a user turn in most client dialects, but the
		// upstream's item union has no string member: wrap it deterministically.
		norm, _ := json.Marshal(map[string]any{
			"type": "message", "role": "user", "content": json.RawMessage(trimmed),
		})
		return inputItem{out: norm}, true
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || len(obj) == 0 {
		e.logSanitizeWarn(account, "item", idx, "not an object")
		return inputItem{}, false
	}
	var typ, role string
	_ = json.Unmarshal(obj["type"], &typ)
	_ = json.Unmarshal(obj["role"], &role)
	role = strings.ToLower(strings.TrimSpace(role))

	switch typ {
	case "message":
		if role == "tool" || role == "function" {
			// A typed message wearing a chat tool role: same treatment as the
			// typeless form — translate, don't drop.
			norm, _ := json.Marshal(map[string]any{
				"type": "message", "role": "user",
				"content": "[tool result] " + rawTextField(obj),
			})
			return inputItem{out: norm}, true
		}
		norm, ok := normalizeMessageItem(obj)
		if !ok {
			e.logSanitizeWarn(account, "message", idx, "unusable content")
			return inputItem{}, false
		}
		return inputItem{out: norm}, true
	case "reasoning", "item_reference":
		return inputItem{out: json.RawMessage(trimmed)}, true
	case "function_call", "function_call_output":
		var callID string
		_ = json.Unmarshal(obj["call_id"], &callID)
		if callID == "" {
			e.logSanitizeWarn(account, typ, idx, "missing call_id")
			return inputItem{}, false
		}
		return inputItem{out: json.RawMessage(trimmed), callID: callID,
			kind: map[string]string{"function_call": "call", "function_call_output": "output"}[typ]}, true
	case "":
		switch role {
		case "user", "assistant", "system", "developer":
			norm, ok := normalizeMessageItem(obj)
			if !ok {
				e.logSanitizeWarn(account, "(typeless "+role+")", idx, "unusable content")
				return inputItem{}, false
			}
			return inputItem{out: norm}, true
		case "tool", "function":
			// A chat-convention result item carries information the model
			// should still see; without a call_id it cannot be a paired
			// function output, so it is translated to a marked user message.
			norm, _ := json.Marshal(map[string]any{
				"type": "message", "role": "user",
				"content": "[tool result] " + rawTextField(obj),
			})
			return inputItem{out: norm}, true
		default:
			e.logSanitizeWarn(account, "role:"+role, idx, "unsupported role")
			return inputItem{}, false
		}
	default:
		// The exact offender behind "input[i] did not match any supported
		// type": dropped so the request rides, and logged so the harness's
		// shape is on record for a follow-up translation if one earns it.
		e.logSanitizeWarn(account, typ, idx, "unsupported item type")
		return inputItem{}, false
	}
}

// normalizeMessageItem guarantees the item matches what the upstream accepts:
// an explicit "type":"message" when missing, and user-role content parts that
// the whitelist actually allows (output_text on a user message is measured
// invalid; the text is the same, only the stamp differs). The bool reports
// whether the item is usable at all.
func normalizeMessageItem(obj map[string]json.RawMessage) (json.RawMessage, bool) {
	cloned := make(map[string]json.RawMessage, len(obj)+1)
	for k, v := range obj {
		cloned[k] = v
	}
	if _, has := cloned["type"]; !has {
		cloned["type"] = json.RawMessage(`"message"`)
	}
	var role string
	_ = json.Unmarshal(cloned["role"], &role)
	if strings.EqualFold(role, "user") {
		if parts, ok := cloned["content"]; ok && bytes.HasPrefix(bytes.TrimSpace(parts), []byte("[")) {
			var arr []map[string]json.RawMessage
			if err := json.Unmarshal(parts, &arr); err == nil {
				reshaped := false
				for i, p := range arr {
					var pt string
					_ = json.Unmarshal(p["type"], &pt)
					if pt == "output_text" {
						n := map[string]json.RawMessage{"type": json.RawMessage(`"input_text"`)}
						if t, has := p["text"]; has {
							n["text"] = t
						}
						arr[i] = n
						reshaped = true
					}
				}
				if reshaped {
					fixed, err := json.Marshal(arr)
					if err == nil {
						cloned["content"] = fixed
					}
				}
			}
		}
	}
	out, err := json.Marshal(cloned)
	if err != nil {
		return nil, false
	}
	return out, true
}

func isBareStringJSON(raw []byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '"'
}

// rawTextField pulls a readable text out of an item whose content may be a
// bare string ("content":"...") or an array of typed parts.
func rawTextField(obj map[string]json.RawMessage) string {
	var s string
	if err := json.Unmarshal(obj["content"], &s); err == nil {
		return s
	}
	return messageText(obj["content"])
}

func (e *Engine) logSanitizeWarn(account, typ string, idx int, why string) {
	e.log.Warn("responses input item normalized away by sanitizer",
		"account", account, "item", typ, "index", idx, "reason", why)
}
