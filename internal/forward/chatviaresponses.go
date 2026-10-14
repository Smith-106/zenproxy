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
	// Prompt-cache identity must survive the rewrite. session-sticky (and
	// OpenAI clients) put the stable conversation key on prompt_cache_key;
	// dropping it here made every muse turn look like a cold prefix to the
	// upstream even when account+exit+ses_* were already sticky — which is
	// exactly the "cache_read stuck at ~3–4k" pattern measured in production.
	for _, k := range []string{"prompt_cache_key", "prompt_cache_retention", "include"} {
		if v, ok := src[k]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			dst[k] = v
		}
	}
	// Chat clients (OpenCode / sub2api) send reasoning_effort; Responses wants
	// a `reasoning` object. Dropping either is exactly "丢失思考度": the
	// upstream falls back to a shallow default and OpenCode's thinking pane
	// stays empty because no summary was requested.
	attachReasoningForResponses(dst, src)
	// CPA lists muse free models with thinking.levels=["xhigh"], but OpenCode's
	// "auto" UI often sends no reasoning_effort at all (usage shows empty while
	// the console prints auto). Default the bridge to xhigh+summary so those
	// turns actually get the configured thinking depth and a foldable trace.
	if _, has := dst["reasoning"]; !has && needsChatViaResponses(model) {
		dst["reasoning"] = map[string]any{"effort": "xhigh", "summary": "auto"}
	}
	// Tools stay if present; prepareResponses will top them up and force flat shape.
	if v, ok := src["tools"]; ok && len(bytes.TrimSpace(v)) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) && !bytes.Equal(bytes.TrimSpace(v), []byte("[]")) {
		dst["tools"] = flattenChatTools(v)
	}

	// Structured output must survive the rewrite. Chat clients declare the
	// contract on response_format; Responses declares it on text.format — and
	// the free-tier upstream DOES enforce the schema (measured: a bad schema
	// is rejected with the structured-outputs linter's own message). Dropping
	// the field here left pi's sub-agents with a plain-text muse reply and a
	// "no structured result" fallback loop, which is exactly why "this model
	// behaves differently": only the bridge path lost the contract.
	if v, ok := src["response_format"]; ok && rawPresent(v) {
		if tf, ok2 := chatResponseFormatToTextFormat(v); ok2 {
			dst["text"] = tf
		}
	}

	out, err = json.Marshal(dst)
	return out, clientStream, err
}

// chatResponseFormatToTextFormat maps the chat surface's response_format onto
// the Responses text.format object. The schema is forwarded verbatim — it is
// the client's contract, and the upstream lints it far better than we could.
func chatResponseFormatToTextFormat(v json.RawMessage) (map[string]any, bool) {
	var rf struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(v), &rf); err != nil {
		return nil, false
	}
	switch rf.Type {
	case "json_object":
		return map[string]any{"format": map[string]any{"type": "json_object"}}, true
	case "json_schema":
		var js struct {
			Name        string          `json:"name"`
			Description json.RawMessage `json:"description"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict"`
		}
		if err := json.Unmarshal(rf.JSONSchema, &js); err != nil {
			return nil, false
		}
		name := js.Name
		if strings.TrimSpace(name) == "" {
			name = "response"
		}
		format := map[string]any{"type": "json_schema", "name": name}
		if js.Schema != nil && rawPresent(js.Schema) {
			format["schema"] = json.RawMessage(js.Schema)
		}
		if js.Description != nil && rawPresent(js.Description) {
			format["description"] = json.RawMessage(js.Description)
		}
		if js.Strict != nil {
			format["strict"] = *js.Strict
		}
		return map[string]any{"format": format}, true
	default:
		// "text" and unknown types: no enforcement contract to forward.
		return nil, false
	}
}

func rawPresent(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) > 0 && !bytes.Equal(v, []byte("null"))
}

// attachReasoningForResponses maps chat-shaped reasoning knobs onto the
// Responses `reasoning` object. summary=auto is filled in when absent so the
// upstream actually returns reasoning items we can fold back into
// reasoning_content for OpenCode.
func attachReasoningForResponses(dst map[string]any, src map[string]json.RawMessage) {
	if v, ok := src["reasoning"]; ok && rawPresent(v) {
		var obj map[string]any
		if err := json.Unmarshal(v, &obj); err == nil && obj != nil {
			if _, has := obj["summary"]; !has {
				obj["summary"] = "auto"
			}
			dst["reasoning"] = obj
			return
		}
		dst["reasoning"] = v
		return
	}
	if v, ok := src["reasoning_effort"]; ok && rawPresent(v) {
		var effort string
		if err := json.Unmarshal(v, &effort); err != nil || strings.TrimSpace(effort) == "" {
			return
		}
		dst["reasoning"] = map[string]any{
			"effort":  strings.TrimSpace(effort),
			"summary": "auto",
		}
	}
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
	// Always emit input as a message array, even for a single user turn.
	// A bare string is accepted by /responses and was convenient for probes,
	// but the second turn of a conversation then switches to an array: the
	// upstream prompt-cache prefix cannot grow across that shape change, so
	// cache_read stuck at the instructions+tools size (~3–4k) forever.
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
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	var text, reasoning strings.Builder
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			for _, s := range item.Summary {
				reasoning.WriteString(s.Text)
			}
			for _, c := range item.Content {
				if c.Type == "reasoning_text" || c.Type == "output_text" || c.Type == "text" || c.Type == "" {
					reasoning.WriteString(c.Text)
				}
			}
		case "message", "":
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "text" || c.Type == "" {
					text.WriteString(c.Text)
				}
			}
		}
	}
	out := foldedCompletion{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt,
		Model:   resp.Model,
		Choices: []foldedChoice{{
			Index: 0,
			Message: foldedMessage{
				Role:             "assistant",
				Content:          text.String(),
				ReasoningContent: reasoning.String(),
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
	type deltaBody struct {
		Role             string `json:"role,omitempty"`
		Content          string `json:"content,omitempty"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	type chunk struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int       `json:"index"`
			Delta        deltaBody `json:"delta"`
			FinishReason *string   `json:"finish_reason"`
		} `json:"choices"`
	}
	var buf bytes.Buffer
	role := chunk{ID: obj.ID, Object: "chat.completion.chunk", Created: obj.Created, Model: obj.Model}
	role.Choices = []struct {
		Index        int       `json:"index"`
		Delta        deltaBody `json:"delta"`
		FinishReason *string   `json:"finish_reason"`
	}{{Index: 0, Delta: deltaBody{Role: "assistant"}}}
	enc, _ := json.Marshal(role)
	buf.WriteString("data: ")
	buf.Write(enc)
	buf.WriteString("\n\n")

	reasoning := ""
	if len(obj.Choices) > 0 {
		reasoning = obj.Choices[0].Message.ReasoningContent
	}
	if reasoning != "" {
		delta := role
		delta.Choices[0].Delta = deltaBody{ReasoningContent: reasoning}
		enc, _ = json.Marshal(delta)
		buf.WriteString("data: ")
		buf.Write(enc)
		buf.WriteString("\n\n")
	}
	if content != "" {
		delta := role
		delta.Choices[0].Delta = deltaBody{Content: content}
		enc, _ = json.Marshal(delta)
		buf.WriteString("data: ")
		buf.Write(enc)
		buf.WriteString("\n\n")
	}
	stop := "stop"
	final := role
	final.Choices[0].Delta = deltaBody{}
	final.Choices[0].FinishReason = &stop
	enc, _ = json.Marshal(final)
	buf.WriteString("data: ")
	buf.Write(enc)
	buf.WriteString("\n\n")
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes(), nil
}
