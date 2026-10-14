package forward

import (
	"encoding/json"
	"testing"
)

// The bridge is pi's (and any chat client's) only path to structured output on
// the muse models: the chat surface says response_format, the Responses
// surface says text.format, and dropping the link made the sub-agent answer
// in free text — the observed "structured result missing" fallback loop.
func TestChatToResponsesBodyPreservesStructuredOutput(t *testing.T) {
	bridge := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		out, _, err := chatToResponsesBody([]byte(raw))
		if err != nil {
			t.Fatalf("chatToResponsesBody: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("out unmarshal: %v", err)
		}
		return m
	}

	// json_schema: name/schema/strict all land in text.format, verbatim.
	m := bridge(t, `{"model":"muse-spark-1.3-contributor-free","stream":false,
	  "messages":[{"role":"user","content":"rate it"}],
	  "response_format":{"type":"json_schema","json_schema":{
	     "name":"vote","strict":true,
	     "schema":{"type":"object","properties":{"score":{"type":"number"},"note":{"type":"string"}},
	               "required":["score","note"],"additionalProperties":false}}}}`)
	text, hasText := m["text"].(map[string]any)
	if !hasText {
		t.Fatalf("text.format missing after rewrite: %v", m)
	}
	format, hasFormat := text["format"].(map[string]any)
	if !hasFormat || format["type"] != "json_schema" || format["name"] != "vote" {
		t.Fatalf("text.format wrong: %v", text)
	}
	schema, hasSchema := format["schema"].(map[string]any)
	if !hasSchema || schema["type"] != "object" {
		t.Fatalf("schema must be forwarded verbatim: %v", format)
	}
	if strict, ok := format["strict"].(bool); !ok || !strict {
		t.Fatalf("strict flag lost: %v", format)
	}
	// prompt_cache_key preservation untouched by this change.
	m2 := bridge(t, `{"model":"muse-spark-1.3-contributor-free","stream":false,
	  "messages":[{"role":"user","content":"q"}],
	  "prompt_cache_key":"pfx_24397c586d40f4f9272ezzzzzzzzzzzz",
	  "response_format":{"type":"json_object"}}`)
	text2, ok := m2["text"].(map[string]any)
	if !ok || text2["format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("json_object must map through: %v", m2)
	}
	if m2["prompt_cache_key"] != "pfx_24397c586d40f4f9272ezzzzzzzzzzzz" {
		t.Fatalf("prompt_cache_key must survive: %v", m2)
	}

	// response_type "text" carries no enforcement contract: no text key.
	m3 := bridge(t, `{"model":"muse-spark-1.3-contributor-free","stream":false,
	  "messages":[{"role":"user","content":"q"}],
	  "response_format":{"type":"text"}}`)
	if _, has := m3["text"]; has {
		t.Fatalf("response_format=text must not create text.format: %v", m3)
	}
	// No response_format at all: no text key, behavior unchanged.
	m4 := bridge(t, `{"model":"muse-spark-1.3-contributor-free","stream":false,
	  "messages":[{"role":"user","content":"q"}]}`)
	if _, has := m4["text"]; has {
		t.Fatalf("a plain body must not gain text.format: %v", m4)
	}
}
