package forward

import (
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

func TestChatToResponsesBodyDefaultsMuseToXHigh(t *testing.T) {
	raw := []byte(`{
		"model":"muse-spark-1.3-contributor-free",
		"stream":false,
		"messages":[{"role":"user","content":"plan carefully"}]
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
		t.Fatalf("default reasoning = %+v, want xhigh/auto", reasoning)
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
