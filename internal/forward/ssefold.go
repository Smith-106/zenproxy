// Folding an upstream SSE stream back into a single completion.
//
// The free tier refuses non-streaming requests, so whenever the proxy speaks to
// it on behalf of a client that did not ask for a stream, it asks for one anyway
// and reassembles the answer here. Without this the client would receive
// `text/event-stream` for a request that promised a JSON object.
package forward

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// foldStream reassembles a buffered chat-completions SSE stream into the single
// JSON object a non-streaming client expects. It always returns valid JSON: an
// empty or unreadable stream becomes an error envelope rather than a silently
// empty answer.
func foldStream(raw []byte) []byte {
	var (
		id      string
		model   string
		created int64
		content strings.Builder
		finish  string
		usage   json.RawMessage
		tools   = map[int]*foldedToolCall{}
		order   []int
		seen    bool
	)

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		seen = true
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, ch := range chunk.Choices {
			content.WriteString(ch.Delta.text())
			for _, tc := range ch.Delta.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &foldedToolCall{Type: "function"}
					tools[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.ID = tc.ID
				}
				if tc.Type != "" {
					acc.Type = tc.Type
				}
				acc.Function.Name += tc.Function.Name
				acc.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
		}
	}

	if !seen {
		return []byte(`{"error":{"type":"zenproxy_error","message":"upstream returned an unreadable event stream"}}`)
	}
	msg := foldedMessage{Role: "assistant", Content: content.String()}
	for _, i := range order {
		tc := tools[i]
		tc.Index = i
		msg.ToolCalls = append(msg.ToolCalls, *tc)
	}
	if finish == "" {
		finish = "stop"
	}
	if created == 0 {
		created = time.Now().Unix()
	}

	out := foldedCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []foldedChoice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   usage,
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"error":{"type":"zenproxy_error","message":"failed to reassemble the streamed reply"}}`)
	}
	return enc
}

type streamChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Created int64           `json:"created"`
	Usage   json.RawMessage `json:"usage"`
	Choices []struct {
		Index        int         `json:"index"`
		Delta        streamDelta `json:"delta"`
		FinishReason *string     `json:"finish_reason"`
	} `json:"choices"`
}

// streamDelta keeps content as raw JSON: a provider that streams something
// other than a plain string must not break the reassembly.
type streamDelta struct {
	Content   json.RawMessage  `json:"content"`
	ToolCalls []streamToolCall `json:"tool_calls"`
}

func (d streamDelta) text() string {
	if len(d.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(d.Content, &s); err != nil {
		return ""
	}
	return s
}

type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type foldedCompletion struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []foldedChoice  `json:"choices"`
	Usage   json.RawMessage `json:"usage,omitempty"`
}

type foldedChoice struct {
	Index        int           `json:"index"`
	Message      foldedMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type foldedMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []foldedToolCall `json:"tool_calls,omitempty"`
}

type foldedToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
