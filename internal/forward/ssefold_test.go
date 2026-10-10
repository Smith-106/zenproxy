package forward

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1791559409,"model":"big-pickle","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Paris"},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{"content":" is the capital."},"finish_reason":null}]}

data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}

data: [DONE]

`

// A client that asked for one response must get one response, whatever the
// upstream talked in.
func TestFoldStreamReassemblesContent(t *testing.T) {
	out, errMsg := foldStream([]byte(sampleStream))
	if errMsg != "" {
		t.Fatalf("a good stream reported a failure: %s", errMsg)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("folded output is not JSON: %v\n%s", err, out)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if got.ID != "c1" || got.Model != "big-pickle" || got.Created != 1791559409 {
		t.Errorf("envelope lost: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	if got.Choices[0].Message.Content != "Paris is the capital." {
		t.Errorf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.Role != "assistant" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("role/finish = %q/%q", got.Choices[0].Message.Role, got.Choices[0].FinishReason)
	}
	if got.Usage["completion_tokens"] != 5 {
		t.Errorf("usage = %v", got.Usage)
	}
}

// Tool calls arrive as fragments too, and a folded reply that loses them turns
// an agent into a chatbot.
func TestFoldStreamAssemblesToolCalls(t *testing.T) {
	stream := `data: {"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ba","arguments":"{\"comm"}}]},"finish_reason":null}]}
data: {"id":"c2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"sh","arguments":"and\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}
data: [DONE]
`
	var got foldedCompletion
	folded, errMsg := foldStream([]byte(stream))
	if errMsg != "" {
		t.Fatalf("a good stream reported a failure: %s", errMsg)
	}
	if err := json.Unmarshal(folded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls were dropped: %s", folded)
	}
	tc := got.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "bash" || tc.Function.Arguments != `{"command":"ls"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if tc.Type != "function" {
		t.Errorf("type = %q", tc.Type)
	}
}

// Reasoning deltas must survive the fold. Dropping them turns a partially
// useful reply into an empty content field with no trace of what the model did.
func TestFoldStreamKeepsReasoningContent(t *testing.T) {
	stream := `data: {"id":"cR","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}
data: {"id":"cR","choices":[{"index":0,"delta":{"reasoning_content":"ing","content":"Hi"},"finish_reason":null}]}
data: {"id":"cR","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}
data: [DONE]
`
	out, errMsg := foldStream([]byte(stream))
	if errMsg != "" {
		t.Fatalf("fold failed: %s", errMsg)
	}
	var got foldedCompletion
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msg := got.Choices[0].Message
	if msg.Content != "Hi" {
		t.Errorf("content = %q", msg.Content)
	}
	if msg.ReasoningContent != "thinking" {
		t.Errorf("reasoning_content = %q", msg.ReasoningContent)
	}
}

// An upstream that accepts the request and then says nothing must produce an
// error the client can see, not an empty success.
//
// The failure has to come back as a message rather than as an error-shaped
// payload: a caller that can only see bytes has no way to choose a status code,
// and the envelope ends up inside a 200.
func TestFoldStreamReportsUnreadableStreams(t *testing.T) {
	for _, raw := range []string{"", "event: ping\n\n", "<html>gateway timeout</html>"} {
		out, errMsg := foldStream([]byte(raw))
		if errMsg == "" {
			t.Errorf("%q folded to %s and reported no failure", raw, out)
		}
		if out != nil {
			t.Errorf("%q returned a payload alongside the failure: %s", raw, out)
		}
	}
}

// Comments, keep-alives and a missing [DONE] must not stop the fold.
func TestFoldStreamToleratesNoise(t *testing.T) {
	stream := ": keep-alive\n\ndata: {\"id\":\"c3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"
	out, errMsg := foldStream([]byte(stream))
	if errMsg != "" {
		t.Fatalf("a noisy but good stream reported a failure: %s", errMsg)
	}
	if !strings.Contains(string(out), `"content":"ok"`) {
		t.Errorf("folded = %s", out)
	}
	if !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Errorf("a stream without a finish reason must still be reported as complete: %s", out)
	}
}

// The Responses stream ends with `response.completed`, and that event's
// `response` field already is the non-streaming object. Passing it through
// byte-exact is both simpler and more faithful than reassembling the deltas.
func TestFoldResponsesStreamPassesThroughTheCompletedResponse(t *testing.T) {
	response := `{"id":"resp_1","object":"response","created_at":1791559409,"model":"muse-spark-1.3-contributor-free","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Paris","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2}}`
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"Par"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"is"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":` + response + "}\n\n"

	out, errMsg := foldResponsesStream([]byte(stream))
	if errMsg != "" {
		t.Fatalf("a good stream reported a failure: %s", errMsg)
	}
	if string(out) != response {
		t.Errorf("the completed response must pass through byte-exact:\ngot  %s\nwant %s", out, response)
	}
}

// A stream that ends before `response.completed` still has the text in its
// deltas, so the answer is rebuilt rather than thrown away.
func TestFoldResponsesStreamRebuildsFromDeltas(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_2","model":"big-pickle","created_at":1791559409}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"Paris"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":" is the capital."}` + "\n\n"

	var got struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Model  string `json:"model"`
		Status string `json:"status"`
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	rebuilt, errMsg := foldResponsesStream([]byte(stream))
	if errMsg != "" {
		t.Fatalf("a stream with deltas reported a failure: %s", errMsg)
	}
	if err := json.Unmarshal(rebuilt, &got); err != nil {
		t.Fatalf("folded output is not JSON: %v", err)
	}
	if got.Object != "response" || got.Status != "completed" {
		t.Errorf("object = %q status = %q", got.Object, got.Status)
	}
	if got.ID != "resp_2" || got.Model != "big-pickle" {
		t.Errorf("id = %q model = %q", got.ID, got.Model)
	}
	if len(got.Output) != 1 || len(got.Output[0].Content) != 1 {
		t.Fatalf("output shape = %+v", got.Output)
	}
	tc := got.Output[0].Content[0]
	if tc.Type != "output_text" || tc.Text != "Paris is the capital." {
		t.Errorf("content = %+v", tc)
	}
}

// The one outcome worse than an error is a valid-looking empty answer: the
// client believes it succeeded. A Responses stream that carried no answer must
// never fold into one.
func TestFoldResponsesStreamNeverInventsAnEmptySuccess(t *testing.T) {
	for _, tc := range []struct{ name, stream string }{
		{"nothing at all", ""},
		{"a keep-alive only", ": ping\n\n"},
		{"html from a gateway", "<html>502</html>"},
		{"events but no answer", "event: response.created\n" + `data: {"type":"response.created","response":{"id":"r"}}` + "\n\n"},
		{"the response failed", "event: response.failed\n" + `data: {"type":"response.failed","response":{"id":"r"}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errMsg := foldResponsesStream([]byte(tc.stream))
			if errMsg == "" {
				t.Errorf("folded to %s and reported no failure", out)
			}
			if out != nil {
				t.Errorf("a failure must not carry a payload: %s", out)
			}
		})
	}
}

// The chat folder must not be able to read a Responses stream at all — that is
// the whole reason the surface is threaded through to the fold.
func TestChatFolderCannotReadAResponsesStream(t *testing.T) {
	stream := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r","output":[]}}` + "\n\n"
	out, errMsg := foldStream([]byte(stream))
	if errMsg == "" {
		t.Errorf("the chat folder read a Responses stream as %s", out)
	}
}
