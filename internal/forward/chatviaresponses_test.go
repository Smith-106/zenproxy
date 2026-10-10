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
	var input string
	if err := json.Unmarshal(obj["input"], &input); err != nil || input != "hi" {
		t.Fatalf("input = %s err=%v", obj["input"], err)
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
