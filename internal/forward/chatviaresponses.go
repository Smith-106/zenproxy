package forward

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Models that refuse /chat/completions with ModelProtocolUnsupported and only
// answer on /responses. sub2api's opencode_go/zen mode has no native responses
// upstream URL, so it always forwards via raw chat completions — without this
// rewrite those models are unreachable through sub2api's account test and its
// /v1/responses → chat fallback path.
func needsChatViaResponses(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "muse-spark-") && strings.Contains(m, "contributor-free"):
		return true
	default:
		return false
	}
}

func rewritePathChatToResponses(path string) string {
	const chat = "/chat/completions"
	const resp = "/responses"
	if i := strings.LastIndex(path, chat); i >= 0 {
		return path[:i] + resp + path[i+len(chat):]
	}
	return path
}

// chatToResponsesBody rewrites a chat-completions body into a Responses body.
// The returned clientStream flag is whatever the client asked for; the free
// gate still forces upstream stream=true separately.
func chatToResponsesBody(raw []byte) (out []byte, clientStream bool, err error) {
	var src map[string]json.RawMessage
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, false, err
	}
	model := ""
	_ = json.Unmarshal(src["model"], &model)
	if model == "" {
		return nil, false, fmt.Errorf("chat body has no model")
	}
	clientStream = string(bytes.TrimSpace(src["stream"])) == "true"

	dst := map[string]any{
		"model":  model,
		"stream": clientStream,
	}

	if instr, input, ok := splitChatMessages(src["messages"]); ok {
		if instr != "" {
			dst["instructions"] = instr
		}
		dst["input"] = input
	} else {
		return nil, false, fmt.Errorf("chat body has no usable messages")
	}

	// Token budget field names differ across surfaces.
	if v, ok := src["max_tokens"]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		dst["max_output_tokens"] = v
	}
	if v, ok := src["max_output_tokens"]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		dst["max_output_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p", "metadata", "user"} {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
	// Tools stay if present; prepareResponses will top them up and force flat shape.
	if v, ok := src["tools"]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) && !bytes.Equal(bytes.TrimSpace(v), []byte("[]")) {
		dst["tools"] = flattenChatTools(v)
	}

	out, err = json.Marshal(dst)
	return out, clientStream, err
}

func splitChatMessages(raw json.RawMessage) (instructions string, input any, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil, false
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
		return "", nil, false
	}
	var sys []string
	var rest []map[string]any
	for _, m := range msgs {
		text := messageText(m.Content)
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			if text != "" {
				sys = append(sys, text)
			}
		default:
			rest = append(rest, map[string]any{
				"role":    m.Role,
				"content": text,
			})
		}
	}
	if len(rest) == 0 {
		return "", nil, false
	}
	instructions = strings.Join(sys, "\n\n")
	// One plain user turn → string input (matches how we probe muse-spark).
	if len(rest) == 1 && strings.EqualFold(fmt.Sprint(rest[0]["role"]), "user") {
		return instructions, rest[0]["content"], true
	}
	return instructions, rest, true
}

func messageText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return strings.Trim(string(raw), `"`)
}

// flattenChatTools rewrites nested chat tool entries into the flat Responses
// shape when needed. Already-flat entries pass through.
func flattenChatTools(raw json.RawMessage) json.RawMessage {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil || len(tools) == 0 {
		return raw
	}
	out := make([]map[string]any, 0, len(tools))
	changed := false
	for _, t := range tools {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(t, &obj); err != nil {
			continue
		}
		if _, hasName := obj["name"]; hasName {
			var m map[string]any
			_ = json.Unmarshal(t, &m)
			out = append(out, m)
			continue
		}
		var nested struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(t, &nested); err != nil || nested.Function.Name == "" {
			var m map[string]any
			_ = json.Unmarshal(t, &m)
			out = append(out, m)
			continue
		}
		changed = true
		entry := map[string]any{
			"type":        "function",
			"name":        nested.Function.Name,
			"description": nested.Function.Description,
		}
		if len(bytes.TrimSpace(nested.Function.Parameters)) > 0 {
			entry["parameters"] = nested.Function.Parameters
		}
		out = append(out, entry)
	}
	if !changed {
		return raw
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return enc
}

// responsesToChatCompletion turns a folded Responses object into a chat
// completion object that non-streaming chat clients (and sub2api's chat
// fallback) can read.
func responsesToChatCompletion(raw []byte) ([]byte, error) {
	var resp struct {
		ID        string `json:"id"`
		Model     string `json:"model"`
		CreatedAt int64  `json:"created_at"`
		Status    string `json:"status"`
		Output    []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, item := range resp.Output {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" || c.Type == "text" || c.Type == "" {
				text.WriteString(c.Text)
			}
		}
	}
	created := resp.CreatedAt
	if created == 0 {
		created = 0
	}
	out := foldedCompletion{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: created,
		Model:   resp.Model,
		Choices: []foldedChoice{{
			Index: 0,
			Message: foldedMessage{
				Role:    "assistant",
				Content: text.String(),
			},
			FinishReason: "stop",
		}},
		Usage: resp.Usage,
	}
	return json.Marshal(out)
}

// chatCompletionToSSE emits a minimal chat-completions SSE stream from a
// folded chat completion object, for clients that asked for stream=true.
func chatCompletionToSSE(chatJSON []byte) ([]byte, error) {
	var obj foldedCompletion
	if err := json.Unmarshal(chatJSON, &obj); err != nil {
		return nil, err
	}
	content := ""
	if len(obj.Choices) > 0 {
		content = obj.Choices[0].Message.Content
	}
	type chunk struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role    string `json:"role,omitempty"`
				Content string `json:"content,omitempty"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	var buf bytes.Buffer
	role := chunk{ID: obj.ID, Object: "chat.completion.chunk", Created: obj.Created, Model: obj.Model}
	role.Choices = []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	}{{Index: 0}}
	role.Choices[0].Delta.Role = "assistant"
	enc, _ := json.Marshal(role)
	buf.WriteString("data: ")
	buf.Write(enc)
	buf.WriteString("\n\n")

	if content != "" {
		delta := role
		delta.Choices[0].Delta.Role = ""
		delta.Choices[0].Delta.Content = content
		enc, _ = json.Marshal(delta)
		buf.WriteString("data: ")
		buf.Write(enc)
		buf.WriteString("\n\n")
	}
	stop := "stop"
	final := role
	final.Choices[0].Delta = struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	}{}
	final.Choices[0].FinishReason = &stop
	enc, _ = json.Marshal(final)
	buf.WriteString("data: ")
	buf.Write(enc)
	buf.WriteString("\n\n")
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes(), nil
}
