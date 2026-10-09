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
	out := foldStream([]byte(sampleStream))
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
	if err := json.Unmarshal(foldStream([]byte(stream)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || len(got.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls were dropped: %s", foldStream([]byte(stream)))
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

// An upstream that accepts the request and then says nothing must produce an
// error the client can see, not an empty success.
func TestFoldStreamReportsUnreadableStreams(t *testing.T) {
	for _, raw := range []string{"", "event: ping\n\n", "<html>gateway timeout</html>"} {
		out := foldStream([]byte(raw))
		if !strings.Contains(string(out), "zenproxy_error") {
			t.Errorf("%q folded to %s, want an error envelope", raw, out)
		}
	}
}

// Comments, keep-alives and a missing [DONE] must not stop the fold.
func TestFoldStreamToleratesNoise(t *testing.T) {
	stream := ": keep-alive\n\ndata: {\"id\":\"c3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"
	out := foldStream([]byte(stream))
	if !strings.Contains(string(out), `"content":"ok"`) {
		t.Errorf("folded = %s", out)
	}
	if !strings.Contains(string(out), `"finish_reason":"stop"`) {
		t.Errorf("a stream without a finish reason must still be reported as complete: %s", out)
	}
}
