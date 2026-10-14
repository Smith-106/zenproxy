package forward

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNeedsChatViaResponses(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"muse-spark-1.3-contributor-free", true},
		{"muse-spark-1.2-contributor-free", true},
		{"Muse-Spark-1.3-Contributor-Free", true},
		{"big-pickle", false},
		{"muse-spark-1.3", false},
		{"", false},
	} {
		if got := needsChatViaResponses(tc.model); got != tc.want {
			t.Errorf("needsChatViaResponses(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestChatToResponsesBodyUsesUserTextAsInput(t *testing.T) {
	raw := []byte(`{
		"model":"muse-spark-1.3-contributor-free",
		"stream":false,
		"max_tokens":64,
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"}
		]
	}`)
	out, stream, err := chatToResponsesBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		t.Fatal("client stream should be false")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var input []map[string]any
	if err := json.Unmarshal(obj["input"], &input); err != nil {
		t.Fatalf("input must be a message array, got %s err=%v", obj["input"], err)
	}
	if len(input) != 1 || input[0]["role"] != "user" || input[0]["content"] != "hi" {
		t.Fatalf("input = %#v", input)
	}
	var instr string
	if err := json.Unmarshal(obj["instructions"], &instr); err != nil || instr != "be brief" {
		t.Fatalf("instructions = %s err=%v", obj["instructions"], err)
	}
	if !strings.Contains(string(obj["max_output_tokens"]), "64") {
		t.Fatalf("max_output_tokens = %s", obj["max_output_tokens"])
	}
	if got := rewritePathChatToResponses("/zen/v1/chat/completions"); got != "/zen/v1/responses" {
		t.Fatalf("path = %q", got)
	}
}

// session-sticky injects prompt_cache_key on the chat body. The muse rewrite
// must keep it, otherwise the upstream prompt cache has no stable identity.
func TestChatToResponsesBodyKeepsPromptCacheKey(t *testing.T) {
	raw := []byte(`{
		"model":"muse-spark-1.3-contributor-free",
		"stream":false,
		"prompt_cache_key":"pfx_24397c586d40f4f9272eaaaaaaaaaaaa",
		"prompt_cache_retention":"in_memory",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"ack"},
			{"role":"user","content":"again"}
		]
	}`)
	out, _, err := chatToResponsesBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := json.Unmarshal(obj["prompt_cache_key"], &key); err != nil || key != "pfx_24397c586d40f4f9272eaaaaaaaaaaaa" {
		t.Fatalf("prompt_cache_key = %s err=%v", obj["prompt_cache_key"], err)
	}
	var retention string
	if err := json.Unmarshal(obj["prompt_cache_retention"], &retention); err != nil || retention != "in_memory" {
		t.Fatalf("prompt_cache_retention = %s err=%v", obj["prompt_cache_retention"], err)
	}
}

func TestResponsesToChatCompletionExtractsText(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1",
		"model":"muse-spark-1.3-contributor-free",
		"created_at":1791643528,
		"status":"completed",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello there"}]}]
	}`)
	out, err := responsesToChatCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got foldedCompletion
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || got.Choices[0].Message.Content != "hello there" {
		t.Fatalf("got %+v", got)
	}
	sse, err := chatCompletionToSSE(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), `"content":"hello there"`) || !strings.Contains(string(sse), "data: [DONE]") {
		t.Fatalf("sse = %s", sse)
	}
}

func TestChatToResponsesBodyMapsReasoningEffort(t *testing.T) {
	raw := []byte(`{
		"model":"muse-spark-1.3-contributor-free",
		"stream":false,
		"reasoning_effort":"xhigh",
		"messages":[{"role":"user","content":"think hard"}]
	}`)
	out, _, err := chatToResponsesBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	var reasoning struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(obj["reasoning"], &reasoning); err != nil {
		t.Fatalf("reasoning missing: %s", obj["reasoning"])
	}
	if reasoning.Effort != "xhigh" || reasoning.Summary != "auto" {
		t.Fatalf("reasoning = %+v", reasoning)
	}
}

func TestChatToResponsesBodyDefaultsMuseEffort(t *testing.T) {
	raw := []byte(`{
		"model":"muse-spark-1.3-contributor-free",
		"stream":false,
		"messages":[{"role":"user","content":"plan carefully"}]
	}`)
	parse := func(t *testing.T) (string, string) {
		t.Helper()
		out, _, err := chatToResponsesBody(raw)
		if err != nil {
			t.Fatal(err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatal(err)
		}
		var reasoning struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(obj["reasoning"], &reasoning); err != nil {
			t.Fatalf("reasoning missing: %s", obj["reasoning"])
		}
		return reasoning.Effort, reasoning.Summary
	}
	// Default: low — the effort whose turns finish inside the streaming
	// client's ~220s ceiling. Muse xhigh turns (190-240s) were aborted by the
	// client seconds before completion, surfacing as error bursts.
	effort, summary := parse(t)
	if effort != "low" || summary != "auto" {
		t.Fatalf("default reasoning = %s/%s, want low/auto", effort, summary)
	}
	// The environment restores any depth explicitly.
	t.Setenv("ZEN_REASONING_EFFORT", "xhigh")
	effort, _ = parse(t)
	if effort != "xhigh" {
		t.Fatalf("override reasoning effort = %s, want xhigh", effort)
	}
}

func TestResponsesToChatCompletionKeepsReasoningSummary(t *testing.T) {
	raw := []byte(`{
		"id":"resp_2",
		"model":"muse-spark-1.3-contributor-free",
		"created_at":1,
		"status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"step one; "},{"type":"summary_text","text":"step two"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"42"}]}
		]
	}`)
	out, err := responsesToChatCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got foldedCompletion
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Choices[0].Message.Content != "42" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.ReasoningContent != "step one; step two" {
		t.Fatalf("reasoning_content = %q", got.Choices[0].Message.ReasoningContent)
	}
	sse, err := chatCompletionToSSE(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sse), `"reasoning_content":"step one; step two"`) {
		t.Fatalf("sse missing reasoning: %s", sse)
	}
}

func TestChatUsageFromResponsesCarriesCachedTokens(t *testing.T) {
	in := json.RawMessage(`{"input_tokens":1000,"output_tokens":50,"total_tokens":1050,"input_tokens_details":{"cached_tokens":800}}`)
	out := chatUsageFromResponses(in)
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		Details          *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(out, &u); err != nil {
		t.Fatal(err)
	}
	if u.PromptTokens != 1000 || u.CompletionTokens != 50 || u.TotalTokens != 1050 {
		t.Fatalf("converted usage = %+v", u)
	}
	if u.Details == nil || u.Details.CachedTokens != 800 {
		t.Fatalf("cached_tokens lost in conversion: %+v", u)
	}
	// Chat-shaped input passes through untouched.
	chat := json.RawMessage(`{"prompt_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}`)
	if !bytes.Equal(chatUsageFromResponses(chat), chat) {
		t.Fatalf("chat-shaped usage must pass through byte-exact: %s", chatUsageFromResponses(chat))
	}
}

func TestChatCompletionToSSECarriesUsageTail(t *testing.T) {
	raw := []byte(`{
		"id":"resp_u1","model":"muse-spark-1.3-contributor-free","created_at":1,
		"status":"completed",
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],
		"usage":{"input_tokens":9531,"output_tokens":42,"total_tokens":9573,
			"input_tokens_details":{"cached_tokens":4209}}
	}`)
	chat, err := responsesToChatCompletion(raw)
	if err != nil {
		t.Fatal(err)
	}
	sse, err := chatCompletionToSSE(chat)
	if err != nil {
		t.Fatal(err)
	}
	body := string(sse)
	for _, want := range []string{`"usage"`, "4209", "prompt_tokens_details", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SSE body missing %s:\n%.400s", want, body)
		}
	}
}
