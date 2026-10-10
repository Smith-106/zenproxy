// Free-tier envelope: how a request that was not sent by the official OpenCode
// client can still use opencode.ai's free models.
//
// The upstream decides "is this OpenCode?" from the request itself. Measured
// against the live endpoint (2026), the matcher is a pure structural gate — it
// does NOT require a verbatim system prompt. Four independent things must line
// up; each was established by replaying captured requests, and each one on its
// own is not enough:
//
//  1. User-Agent. Removing it is enough to be refused, even with everything
//     else in place.
//  2. The x-opencode-* identity headers: `x-opencode-client: cli` plus the
//     request and session ids. The values are not stateful — ids the server has
//     never seen are accepted — only their shape is, so the envelope mints
//     fresh ones: `msg_`/`ses_` + 12 lowercase hex + 14 base62 characters.
//  3. `"stream": true` in the body. A non-streaming request is refused even
//     with a perfect header set, which is why the engine asks the upstream for
//     a stream and folds the answer back for a non-streaming client.
//  4. The request must advertise the tool names `bash` and `read`. Schemas,
//     descriptions, order and extra client tools are ignored — only the names
//     matter. Two is enough. Prompt injection is deliberately NOT used: the
//     old title/agent chunks polluted weaker free models into answering as a
//     title generator ("Greeting", "Short joke request").
//
// Tool-choice policy differs by surface:
//
//   - /chat/completions accepts `tool_choice: "none"`. When the client sent no
//     tools of its own, the envelope sets `"none"` so the model never calls the
//     injected stubs. When the client already had tools, its tool_choice is
//     left untouched.
//   - /responses only accepts `tool_choice: "auto"` whenever tools are present;
//     `"none"` returns HTTP 400. The envelope therefore forces `"auto"`.
//
// Conditions 1-3 are shared by every surface; 4 is re-stated per surface
// because the field shapes differ (chat nests tool names under `function`,
// Responses puts them at the top level). Two surfaces are enveloped, and they
// are not interchangeable: a model can answer on one and refuse the other with
// ModelProtocolUnsupported, which is exactly how both muse-spark variants
// behave. Enveloping only chat therefore makes working models look broken —
// the client sees a FreeTierError that is really "you were not enveloped".
// /messages remains un-enveloped: it carries a third shape that has not been
// characterised.
//
// Two things that do NOT matter, and are therefore deliberately not
// implemented: the TLS fingerprint (a plain Go or Node fetch passes once 1-4
// hold) and the egress IP. That is what makes this a request-envelope problem
// rather than a transport one — no exit in the pool can fix a tier refusal, and
// equally no exit is needed to use the free tier.
package forward

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"zenproxy/internal/config"
)

// defaultClientUA mirrors the User-Agent of the client that is allowed through.
const defaultClientUA = "opencode/1.18.35 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

// FreeTier turns an inbound request into the envelope the free tier accepts.
type FreeTier struct {
	enabled bool
	ua      string
	// models are the model ids expected to be free-tier gated. Anything else
	// travels unmodified so that a paid model is never charged the extra
	// prompt — until the upstream refuses it, which is what learned records.
	models  map[string]bool
	learned sync.Map // model id -> struct{}
}

// NewFreeTier builds the envelope writer. A zero-valued enabled flag disables
// the whole mechanism, which is the escape hatch for a deployment that only
// ever talks to paid models.
func NewFreeTier(cfg config.FreeTier) *FreeTier {
	f := &FreeTier{
		enabled: cfg.Enabled,
		ua:      strings.TrimSpace(cfg.UserAgent),
		models:  make(map[string]bool, len(cfg.Models)),
	}
	if f.ua == "" {
		f.ua = defaultClientUA
	}
	for _, m := range cfg.Models {
		if m = strings.TrimSpace(m); m != "" {
			f.models[m] = true
		}
	}
	return f
}

// Enabled reports whether the envelope is active.
func (f *FreeTier) Enabled() bool { return f != nil && f.enabled }

// GatePlan is one prepared request, together with what the response path has to
// undo afterwards.
type GatePlan struct {
	// Body is the rewritten request body.
	Body []byte
	// Model is the model id the body asked for.
	Model string
	// Template names how the envelope was built: "tools" for chat, or
	// "responses-tools" for the Responses surface. Kept for operator logs.
	Template string
	// Stream reports whether the *client* asked for a stream. When it did not,
	// the upstream stream has to be folded back into a single JSON response.
	Stream bool
	// Surface is which request shape was enveloped. It is carried out of here
	// because the fold that undoes a forced stream is surface-specific: the two
	// surfaces stream entirely different events, and folding Responses events
	// with the chat folder would hand the client a valid-looking empty answer.
	Surface surface
}

// Prepare returns the envelope for a request, or nil when it should travel
// untouched.
//
// forced skips the "is this model expected to be free?" test and is used to
// recover from a refusal the envelope was supposed to prevent.
func (f *FreeTier) Prepare(path string, body []byte, forced bool) *GatePlan {
	if !f.Enabled() || len(body) == 0 {
		return nil
	}
	switch surfaceFor(path) {
	case surfaceChat:
		return f.prepareChat(body, forced)
	case surfaceResponses:
		return f.prepareResponses(body, forced)
	default:
		// The Anthropic /messages shape carries a different prompt template and
		// has not been characterised, so it is left alone rather than half
		// enveloped. A refusal there is reported as a tier gate and nothing more.
		return nil
	}
}

// surface is which request shape an envelope knows how to build.
//
// The two surfaces are not interchangeable, and that is not a detail: a model
// can answer on one and refuse the other with ModelProtocolUnsupported. Both
// muse-spark variants are exactly that case — /chat/completions refuses them
// while /responses serves them — so a proxy that envelops only chat makes two
// working models look unobtainable.
type surface int

const (
	surfaceNone surface = iota
	surfaceChat
	surfaceResponses
)

func surfaceFor(path string) surface {
	p := strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(p, "/chat/completions"):
		return surfaceChat
	case strings.HasSuffix(p, "/responses"):
		return surfaceResponses
	default:
		return surfaceNone
	}
}

func (f *FreeTier) prepareChat(body []byte, forced bool) *GatePlan {
	var req chatEnvelope
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	if !forced && !f.wants(req.Model) {
		return nil
	}
	if len(req.Messages) == 0 {
		return nil
	}

	// The body is rebuilt from a decoded map, so unrelated fields (temperature,
	// max_tokens, provider options, the caller's own messages) survive untouched
	// and only key order changes — which the upstream does not look at.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}

	clientTools := req.toolList()
	hadClientTools := len(clientTools) > 0
	// Chat always uses the nested tool shape. When the client already sent
	// tools, detectToolShape preserves whatever they used; when they sent
	// none, default to nested so the stubs match this surface.
	shape := toolShapeNested
	if hadClientTools {
		shape = detectToolShape(clientTools)
	}
	rawTools, err := topUpToolNames(clientTools, shape)
	if err != nil {
		return nil
	}
	obj["tools"] = rawTools

	// When the client sent no tools of its own, force tool_choice=none so the
	// model never calls the injected bash/read stubs. When the client already
	// had tools, leave its tool_choice alone (or absent).
	if !hadClientTools {
		obj["tool_choice"] = json.RawMessage(`"none"`)
	}

	obj["stream"] = json.RawMessage("true")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return &GatePlan{
		Body:     out,
		Model:    req.Model,
		Template: "tools",
		Stream:   req.streams(),
		Surface:  surfaceChat,
	}
}

// responsesEnvelope is the part of a Responses body the envelope cares about.
//
// The field names differ from chat in a way that matters: there is no system
// message, `instructions` carries what chat carries as one, and `input`
// replaces `messages`. Everything is raw JSON for the same reason as chat — the
// rewrite must not drop fields it does not model.
type responsesEnvelope struct {
	Model        string          `json:"model"`
	Instructions json.RawMessage `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        json.RawMessage `json:"tools"`
	Stream       json.RawMessage `json:"stream"`
}

func (r responsesEnvelope) toolList() []json.RawMessage {
	raw := bytes.TrimSpace(r.Tools)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("[]")) {
		return nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	return tools
}

func (r responsesEnvelope) streams() bool {
	return string(bytes.TrimSpace(r.Stream)) == "true"
}

// prepareResponses is the envelope for the native Responses surface.
//
// Measured, not assumed: the gate guards this surface too. Headers alone are
// not enough — a request carrying a perfect identity and no tools is still
// refused with FreeTierError. The fix is structural: append bash+read and set
// tool_choice to "auto". Prompt injection into `instructions` is deliberately
// not used.
func (f *FreeTier) prepareResponses(body []byte, forced bool) *GatePlan {
	var req responsesEnvelope
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	if !forced && !f.wants(req.Model) {
		return nil
	}
	// Without input there is no request to carry, and `input` is where this
	// surface keeps the caller's content.
	if len(bytes.TrimSpace(req.Input)) == 0 || bytes.Equal(bytes.TrimSpace(req.Input), []byte("null")) {
		return nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}

	clientTools := req.toolList()
	// Responses always uses the flat tool shape. Client tools stay in front;
	// any missing bash/read stubs are appended flat so the surface stays
	// consistent even if a client somehow sent nested entries.
	rawTools, err := topUpToolNames(clientTools, toolShapeFlat)
	if err != nil {
		return nil
	}
	obj["tools"] = rawTools

	// Responses rejects tool_choice values other than "auto" whenever tools are
	// present (HTTP 400: only `"auto"` is supported). Force it unconditionally.
	obj["tool_choice"] = json.RawMessage(`"auto"`)

	obj["stream"] = json.RawMessage("true")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return &GatePlan{
		Body:     out,
		Model:    req.Model,
		Template: "responses-tools",
		Stream:   req.streams(),
		Surface:  surfaceResponses,
	}
}

// ApplyHeaders stamps the official client's identity onto an upstream request.
// The caller runs it before the operator's own header overrides, so an explicit
// account or upstream header still wins.
func (f *FreeTier) ApplyHeaders(h http.Header) {
	h.Set("User-Agent", f.ua)
	h.Set("x-opencode-client", "cli")
	h.Set("x-opencode-project", "global")
	h.Set("x-opencode-request", newClientID("msg_"))
	session := newClientID("ses_")
	h.Set("x-opencode-session", session)
	h.Set("x-opencode-session-id", session)
}

// Learn remembers a model the upstream refused, so the next request for it is
// enveloped on the first attempt instead of after a wasted 403.
func (f *FreeTier) Learn(model string) {
	if f == nil || model == "" {
		return
	}
	f.learned.Store(model, struct{}{})
}

// Learned lists the models that were enveloped only after the upstream refused
// them. It is the operator's hint that the configured free-model list is stale.
func (f *FreeTier) Learned() []string {
	if f == nil {
		return nil
	}
	var out []string
	f.learned.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

func (f *FreeTier) wants(model string) bool {
	if model == "" {
		return false
	}
	if f.models[model] {
		return true
	}
	_, ok := f.learned.Load(model)
	return ok
}

// chatEnvelope is the part of a chat-completions body the envelope cares about.
// Messages and tools stay as raw JSON so that each entry is re-emitted byte for
// byte. The scalars are raw too: a provider that writes `"stream": 1` must not
// make the whole rewrite fall through and cost the request its envelope.
type chatEnvelope struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Tools    json.RawMessage   `json:"tools"`
	Stream   json.RawMessage   `json:"stream"`
}

// toolList decodes the tools array, treating absent, null and an empty array as
// "no tools".
func (c chatEnvelope) toolList() []json.RawMessage {
	raw := bytes.TrimSpace(c.Tools)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || bytes.Equal(raw, []byte("[]")) {
		return nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	return tools
}

// streams reports whether the caller asked for a stream. An absent flag means
// "no": that is the OpenAI default, and getting it wrong hands a non-streaming
// client an event stream it cannot read.
func (c chatEnvelope) streams() bool {
	return string(bytes.TrimSpace(c.Stream)) == "true"
}

// toolNameOf reads a tool's function name, whichever shape names it.
//
// The two surfaces disagree here and neither is wrong: chat nests the name
// under `function`, Responses puts it at the top level. The gate only counts
// names, so counting has to read both.
func toolNameOf(raw json.RawMessage) string {
	var t struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return ""
	}
	if t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

// toolShape is how a tools array names its functions, so that a top-up entry
// matches the entries already there. The upstream ignores the schemas, but it
// does parse the array, and a mixed-shape array is not something a real client
// would ever send.
type toolShape int

const (
	toolShapeNested toolShape = iota // chat:      {"type":"function","function":{"name":…}}
	toolShapeFlat                    // responses: {"type":"function","name":…}
)

func detectToolShape(tools []json.RawMessage) toolShape {
	for _, raw := range tools {
		var nested struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &nested) == nil && nested.Function.Name != "" {
			return toolShapeNested
		}
		var flat struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &flat) == nil && flat.Name != "" {
			return toolShapeFlat
		}
	}
	return toolShapeNested
}

// toolTopUpStub is the wording of the tools this proxy adds purely to satisfy
// the name check. It says so, because the model reads these descriptions.
const toolTopUpStub = "Reserved by the proxied runtime. Do not call this tool."

// topUpToolNames guarantees the request advertises bash and read. The caller's
// own tools stay in front, so its tool calling keeps working; the extra entries
// exist only to satisfy the gate, and their descriptions say so.
func topUpToolNames(tools []json.RawMessage, shape toolShape) ([]byte, error) {
	if tools == nil {
		tools = []json.RawMessage{}
	}
	present := make(map[string]bool, len(tools))
	for _, raw := range tools {
		if name := toolNameOf(raw); name != "" {
			present[name] = true
		}
	}
	hits := 0
	for _, name := range gateToolNames {
		if present[name] {
			hits++
		}
	}
	if hits >= gateMinToolNames {
		return json.Marshal(tools)
	}
	for _, name := range gateToolNames {
		if hits >= gateMinToolNames {
			break
		}
		if present[name] {
			continue
		}
		params := map[string]any{"type": "object", "properties": map[string]any{}}
		entry := map[string]any{"type": "function", "description": toolTopUpStub, "parameters": params, "name": name}
		if shape == toolShapeNested {
			entry = map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": toolTopUpStub,
					"parameters":  params,
				},
			}
		}
		stub, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		tools = append(tools, stub)
		hits++
	}
	return json.Marshal(tools)
}

// idAlphabet is the character set the client draws its random id suffix from.
const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// newClientID mints an id shaped like the client's own: a prefix, 12 lowercase
// hex characters, then 14 characters of base62. The upstream accepts ids it has
// never seen before, so this is a shape to match, not a value to learn.
func newClientID(prefix string) string {
	var buf [20]byte // 6 bytes -> 12 hex characters, 14 bytes -> 14 base62 characters
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on the platforms this ships to. Degrade to a
		// timestamp rather than failing a proxy request over an id.
		binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UnixNano()))
	}
	id := prefix + hex.EncodeToString(buf[:6])
	var sb strings.Builder
	sb.Grow(len(id) + 14)
	sb.WriteString(id)
	for _, b := range buf[6:] {
		sb.WriteByte(idAlphabet[int(b)%len(idAlphabet)])
	}
	return sb.String()
}
