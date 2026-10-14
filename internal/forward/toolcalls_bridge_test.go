package forward

import (
	"encoding/json"
	"strings"
	"testing"
)

// The muse bridge is the only tool channel for agents on the free tier. Both
// directions must preserve the tool loop: the model's function_call items
// arrive as chat tool_calls, and the harness's tool results return as paired
// function items. Dropping EITHER side strands the agent — measured as the
// "model idles, harness nudges forever" loop in production.
func TestResponsesToChatCompletionCarriesToolCalls(t *testing.T) {
	raw := []byte(`{
	  "id":"resp_x","model":"muse-spark-1.3-contributor-free","created_at":1791944400,
	  "output":[
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"I'll check the file"}]},
	    {"type":"function_call","call_id":"call_9","name":"read",
	     "arguments":"{\"file\":\"main.go\"}"},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]}
	  ],
	  "usage":{"input_tokens":10,"output_tokens":5}}`)
	out, err := responsesToChatCompletion(raw)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("json: %v", err)
	}
	choice := m["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v, want 1", calls)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_9" || call["type"] != "function" {
		t.Fatalf("call identity lost: %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "read" || fn["arguments"] != `{"file":"main.go"}` {
		t.Fatalf("call body lost: %v", fn)
	}
	if msg["reasoning_content"] != "I'll check the file" {
		t.Fatalf("reasoning lost beside tool calls: %v", msg)
	}
}

func TestSplitChatMessagesMapsToolLoop(t *testing.T) {
	instr, input, ok := splitChatMessages(json.RawMessage(`[
	  {"role":"system","content":"be terse"},
	  {"role":"user","content":"list files"},
	  {"role":"assistant","content":"checking", "tool_calls":[
	     {"id":"call_9","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},
	  {"role":"tool","tool_call_id":"call_9","content":"main.go\nREADME.md"},
	  {"role":"assistant","content":"two files"}
	]`))
	if !ok {
		t.Fatal("split failed")
	}
	if instr != "be terse" {
		t.Fatalf("instructions = %q", instr)
	}
	items, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(items, &arr); err != nil {
		t.Fatal(err)
	}
	joined := func() string { b, _ := json.Marshal(arr); return string(b) }
	var sawCall, sawOutput bool
	for i, it := range arr {
		if it["type"] == "function_call" && it["call_id"] == "call_9" {
			sawCall = true
			if it["name"] != "bash" || it["arguments"] != `{"cmd":"ls"}` {
				t.Fatalf("call body wrong: %v (all=%s)", it, joined())
			}
			// The narration rides the preceding assistant message, not the call.
			if i > 0 && arr[i-1]["role"] == "assistant" && arr[i-1]["content"] != "checking" {
				t.Fatalf("assistant narration lost: %v", arr[i-1])
			}
		}
		if it["type"] == "function_call_output" && it["call_id"] == "call_9" {
			sawOutput = true
			// The transcript's \n escape decodes to a real newline in output.
			if it["output"] != "main.go"+"\n"+"README.md" {
				t.Fatalf("tool result wrong: %v (all=%s)", it, joined())
			}
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("pair missing: call=%v output=%v all=%s", sawCall, sawOutput, joined())
	}
	// The final plain assistant turn keeps its shape for the next cache prefix.
	last := arr[len(arr)-1]
	if last["role"] != "assistant" || last["content"] != "two files" {
		t.Fatalf("plain turn damaged: %v", last)
	}
}

func TestChatCompletionToSSEEmitsToolCalls(t *testing.T) {
	folded, err := responsesToChatCompletion([]byte(`{
	  "id":"resp_sse","model":"m","created_at":1,
	  "output":[{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}],
	  "usage":{"input_tokens":1,"output_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	sse, err := chatCompletionToSSE(folded)
	if err != nil {
		t.Fatal(err)
	}
	var hasToolDelta, hasFinish bool
	for _, block := range strings.Split(string(sse), "\n\n") {
		block = strings.TrimPrefix(strings.TrimSpace(block)+strings.Repeat("", 0), "data: ")
		if block == "" || block == "data: " || strings.TrimSpace(block) == "[DONE]" {
			continue
		}
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(block)+"", ""))
		if !strings.HasPrefix(line, "data: ") {
			line = strings.TrimSpace(block)
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 && len(chunk.Choices[0].Delta.ToolCalls) > 0 &&
			chunk.Choices[0].Delta.ToolCalls[0].ID == "c1" {
			hasToolDelta = true
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil &&
			*chunk.Choices[0].FinishReason == "tool_calls" {
			hasFinish = true
		}
	}
	if !hasToolDelta {
		t.Fatalf("no tool_calls delta in SSE: %s", string(sse))
	}
	if !hasFinish {
		t.Fatalf("finish_reason tool_calls missing in SSE: %s", string(sse))
	}
}
