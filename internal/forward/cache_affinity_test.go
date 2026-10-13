package forward

import (
	"encoding/json"
	"strings"
	"testing"
)

func unmarshalForTest(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// The synthesized key must be stable across the turns of one conversation
// (only conversation-stable parts are hashed) and distinct across
// conversations. This is the property that makes upstream prompt caches hit.
func TestSynthesizeConversationSeedStableAcrossTurns(t *testing.T) {
	turn1 := `{"model":"muse-spark-1.3-contributor-free","stream":true,
	 "messages":[
	   {"role":"system","content":"You are a careful assistant."},
	   {"role":"user","content":"Explain goroutines to a C programmer."}
	 ]}`
	turn2 := `{"model":"muse-spark-1.3-contributor-free","stream":true,
	 "messages":[
	   {"role":"system","content":"You are a careful assistant."},
	   {"role":"user","content":"Explain goroutines to a C programmer."},
	   {"role":"assistant","content":"They are lightweight coroutines."},
	   {"role":"user","content":"Show me a worker pool."}
	 ]}`
	s1, ok1 := synthesizeConversationSeed([]byte(turn1))
	s2, ok2 := synthesizeConversationSeed([]byte(turn2))
	if !ok1 || !ok2 {
		t.Fatalf("expected seeds, got ok1=%v ok2=%v", ok1, ok2)
	}
	if s1 != s2 {
		t.Fatalf("same conversation must seed identically: %q vs %q", s1, s2)
	}
	if l := len(s1); l != 32 {
		t.Fatalf("seed length = %d, want 32 hex chars", l)
	}

	other := `{"model":"muse-spark-1.3-contributor-free","stream":true,
	 "messages":[
	   {"role":"system","content":"You are a careful assistant."},
	   {"role":"user","content":"Explain goroutines to a Python programmer."}
	 ]}`
	s3, ok3 := synthesizeConversationSeed([]byte(other))
	if !ok3 || s3 == s1 {
		t.Fatalf("distinct conversations must seed differently: %q vs %q", s1, s3)
	}

	// A different system prompt is part of the stable prefix too: it changes
	// what the upstream caches, so it must change the seed.
	sys := strings.Replace(turn1, "You are a careful assistant.", "You are a pirate.", 1)
	s4, _ := synthesizeConversationSeed([]byte(sys))
	if s4 == s1 {
		t.Fatal("a changed system prompt must change the seed")
	}

	// Responses shape: instructions + input array; and the bare-string input.
	resp1 := `{"model":"m","instructions":"be brief","input":[
	   {"type":"message","role":"user","content":[{"type":"input_text","text":"hello world"}]}]}`
	resp2 := `{"model":"m","instructions":"be brief","input":"hello world"}`
	r1, ok4 := synthesizeConversationSeed([]byte(resp1))
	r2, ok5 := synthesizeConversationSeed([]byte(resp2))
	if !ok4 || !ok5 || r1 != r2 {
		t.Fatalf("responses shapes must seed identically: %q ok=%v %q ok=%v", r1, ok4, r2, ok5)
	}

	// Tools are part of the cached prefix: changing them changes the seed.
	withTools := `{"model":"m","messages":[
	   {"role":"system","content":"sys"},{"role":"user","content":"hello"}],
	   "tools":[{"type":"function","function":{"name":"bash"}}]}`
	withoutTools := `{"model":"m","messages":[
	   {"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`
	w1, ok6 := synthesizeConversationSeed([]byte(withTools))
	w2, ok7 := synthesizeConversationSeed([]byte(withoutTools))
	if !ok6 || !ok7 || w1 == w2 {
		t.Fatalf("tools presence must change the seed: %q vs %q (ok=%v/%v)", w1, w2, ok6, ok7)
	}

	// Nothing usable: no seed, no affinity.
	if _, ok := synthesizeConversationSeed([]byte(`{"model":"m"}`)); ok {
		t.Fatal("an empty conversation must not synthesize a seed")
	}
	if _, ok := synthesizeConversationSeed([]byte(`not json`)); ok {
		t.Fatal("non-JSON must not synthesize a seed")
	}
}

// The injector must add the key exactly once and never overwrite one the
// caller already supplied — a client-chosen key is authoritative.
func TestInjectPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"m","messages":[],"stream":true}`)
	out := injectPromptCacheKey(body, "pfx_abc")
	var obj map[string]any
	if err := unmarshalForTest(out, &obj); err != nil {
		t.Fatalf("injected body unmarshal: %v", err)
	}
	if got := obj["prompt_cache_key"]; got != "pfx_abc" {
		t.Fatalf("prompt_cache_key = %v", got)
	}
	if again := injectPromptCacheKey(out, "pfx_other"); string(again) != string(out) {
		t.Fatal("must never overwrite an existing prompt_cache_key")
	}
	// Non-JSON passes through untouched.
	if fixed := injectPromptCacheKey([]byte("not json"), "pfx_x"); string(fixed) != "not json" {
		t.Fatal("non-JSON body must pass through")
	}
}

// The usage reader understands both naming schemes for the cached prefix:
// OpenAI chat (prompt_tokens_details) and Responses (input_tokens_details).
func TestUsageCacheTokens(t *testing.T) {
	c, tot := usageCacheTokens([]byte(`{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":768},"completion_tokens":9}`))
	if c != 768 || tot != 1000 {
		t.Fatalf("chat usage: cached=%d total=%d, want 768/1000", c, tot)
	}
	c, tot = usageCacheTokens([]byte(`{"input_tokens":500,"input_tokens_details":{"cached_tokens":400}}`))
	if c != 400 || tot != 500 {
		t.Fatalf("responses usage: cached=%d total=%d, want 400/500", c, tot)
	}
	if c, _ := usageCacheTokens(nil); c != 0 {
		t.Fatal("nil usage must report nothing")
	}
	if c, _ := usageCacheTokens([]byte(`null`)); c != 0 {
		t.Fatal("null usage must report nothing")
	}
}
