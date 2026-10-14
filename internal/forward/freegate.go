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
//     request and session ids. The *shape* must match
//     (`msg_`/`ses_` + 12 lowercase hex + 14 base62). Fresh request ids are
//     fine every turn, but the session id is sticky: minting a new `ses_*`
//     per attempt makes the upstream treat every turn as a new conversation
//     and drops prompt-cache hits. When the inbound request already carries a
//     session / conversation / prompt-cache key, it is reused (normalized into
//     `ses_*` shape when needed).
//  3. `"stream": true` in the body. A non-streaming request is refused even
//     with a perfect header set, which is why the engine asks the upstream for
//     a stream and folds the answer back for a non-streaming client.
//  4. The tool definitions for `bash` and `read` must match the official
//     client's verbatim — description and parameter schema. This is new as of
//     2026-10-14: the matcher used to accept the names alone, and minimal
//     stub synonyms passed for months; the tightened build now hashes the
//     checked shapes, so the envelope stamps the captured definitions
//     (freetier_tools.go) in place of anything a client sent under those
//     names. Extra client tools are still ignored by the gate and may ride
//     along after the pinned pair. Prompt injection is deliberately NOT used:
//     the old title/agent chunks polluted weaker free models into answering as
//     a title generator ("Greeting", "Short joke request").
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
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"zenproxy/internal/config"
)

// defaultClientUA mirrors the User-Agent of the client that is allowed through.
// Re-captured 2026-10-13 evening from opencode 1.19.0 itself (mitm capture):
// the CLI stamps its requests with the ai-sdk / runtime suffixes on the wire —
// `/responses` carries ai-sdk/provider-utils/4.0.40, and the short form only
// shows up in places like the models fetch. Keep in sync with the envelope
// spec docs (docs/FREE-TIER.md).
const defaultClientUA = "opencode/1.19.0 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

// freeTierUpstreamCredential is the bearer token the opencode app itself uses
// on the free conversation surface (captured: `Authorization: Bearer public`).
// Account keys belong to the paid /zen surface and are rejected by the free
// gate with FreeTierError.
const freeTierUpstreamCredential = "Bearer public"

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
	// ReplyAsChat means the client spoke chat/completions but the upstream was
	// rewritten to /responses (muse-spark and friends). The response path folds
	// Responses events, then converts the object back into a chat completion
	// (JSON or a minimal chat SSE) before answering.
	ReplyAsChat bool
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
	// none, default to nested so the stamped gate pair matches this surface.
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

	// Reasoning-heavy free models often burn a tiny max_tokens budget on
	// hidden reasoning and return finish_reason=length with empty content.
	// Raise only when the client set a budget below the floor; omit/absent
	// stays omitted so upstream defaults remain.
	ensureMinTokenBudget(obj, "max_tokens", gateMinMaxTokens)

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
	// Responses always uses the flat tool shape. Gate tools are pinned first
	// for prompt-cache stability; any missing bash/read stubs are synthesized
	// flat so the surface stays consistent even if a client sent nested entries.
	rawTools, err := topUpToolNames(clientTools, toolShapeFlat)
	if err != nil {
		return nil
	}
	obj["tools"] = rawTools

	// Responses rejects tool_choice values other than "auto" whenever tools are
	// present (HTTP 400: only `"auto"` is supported). Force it unconditionally.
	obj["tool_choice"] = json.RawMessage(`"auto"`)

	ensureMinTokenBudget(obj, "max_output_tokens", gateMinMaxTokens)

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
//
// Request ids stay per-attempt. Session ids stay sticky across turns of the
// same conversation: if the inbound request already carried a session /
// conversation identifier (copied onto h before this runs), that value is
// reused — reshaped into the upstream's `ses_*` form when needed — instead of
// minting a fresh one that would bust prompt cache.
func (f *FreeTier) ApplyHeaders(h http.Header) {
	h.Set("User-Agent", f.ua)
	h.Set("x-opencode-client", "cli")
	h.Set("x-opencode-project", "global")
	// 1.19+ always attaches the workspace directory (URI-encoded). Upstream
	// helpers strip/re-apply it around some hops; omitting it is a visible
	// difference from the real client, so stamp a stable synthetic path.
	if strings.TrimSpace(h.Get("x-opencode-directory")) == "" {
		h.Set("x-opencode-directory", "%2Fworkspace")
	}
	h.Set("x-opencode-request", newRequestID())
	session := stableClientSessionID(h)
	h.Set("x-opencode-session", session)
	h.Set("x-opencode-session-id", session)
}

// inboundSessionHeaderNames are the conversation identifiers the gateway and
// upstream schedulers already understand. Order is preference order.
var inboundSessionHeaderNames = []string{
	"x-opencode-session",
	"x-opencode-session-id",
	"session_id",
	"session-id",
	"conversation_id",
	"x-session-id",
	"x-session-affinity",
}

// stableClientSessionID returns a session id the free-tier gate will accept.
// Prefer an inbound conversation key already present on h; only mint a fresh
// one when the request carries no stable identity at all.
func stableClientSessionID(h http.Header) string {
	for _, name := range inboundSessionHeaderNames {
		raw := strings.TrimSpace(h.Get(name))
		if raw == "" {
			continue
		}
		if looksLikeClientSessionID(raw) {
			return raw
		}
		return deriveClientSessionID(raw)
	}
	return newSessionID()
}

// looksLikeClientSessionID reports whether s already has a session shape the
// gate accepts. Two are now in the wild: the pre-1.19 `ses_` + 12 hex + 14
// base62 short id, and the 1.19-erа affinity id — `ses_` + 64 lowercase hex
// (re-read 2026-10-14 from the official client's request builder: the gate
// validates the session VALUE, and the real client stamps x-opencode-session
// with this 64-hex affinity — a short legacy id got refused the moment the new
// build shipped, taking every exit down with it).
func looksLikeClientSessionID(s string) bool {
	const prefix = "ses_"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	switch rest := s[len(prefix):]; {
	case len(rest) == 64:
		for _, r := range rest {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
		return true
	case len(rest) == 26:
		mid := rest[:12]
		for _, r := range mid {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
		for _, r := range rest[12:] {
			if !strings.ContainsRune(idAlphabet, r) {
				return false
			}
		}
		return true
	}
	return false
}

// deriveClientSessionID maps an arbitrary inbound conversation key onto a
// session the gate accepts. Two constraints bind: the session must be STICKY
// (the same seed always yields the same id — a fresh ses_* per turn is what
// wiped prompt-cache hits), and the 12 hex clock characters must satisfy the
// counter gate. Those are at odds for a purely derived id — a digest hex blob
// fails the gate ~98% of the time — so the id is MINTED once per seed and
// memoized (FIFO-evicted; the clock segment is the real wall clock at mint
// time, the seed never collides into the tail of another conversation because
// each seed owns its own memo slot).
var stickySessions struct {
	sync.Mutex
	ids   map[string]string
	order []string
}

const stickySessionLimit = 4096

func deriveClientSessionID(seed string) string {
	stickySessions.Lock()
	id, ok := stickySessions.ids[seed]
	stickySessions.Unlock()
	if ok {
		return id
	}
	id = newSessionID()
	stickySessions.Lock()
	defer stickySessions.Unlock()
	if stickySessions.ids == nil {
		stickySessions.ids = make(map[string]string, stickySessionLimit)
	}
	if len(stickySessions.ids) >= stickySessionLimit {
		evict := stickySessions.order[0]
		stickySessions.order = stickySessions.order[1:]
		delete(stickySessions.ids, evict)
	}
	stickySessions.ids[seed] = id
	stickySessions.order = append(stickySessions.order, seed)
	return id
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

// ensureMinTokenBudget raises a numeric token-budget field when the client set
// one below gateMinMaxTokens. Absent fields are left absent.
func ensureMinTokenBudget(obj map[string]json.RawMessage, field string, floor int) {
	raw, ok := obj[field]
	if !ok {
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return
	}
	if n >= float64(floor) {
		return
	}
	bumped, err := json.Marshal(floor)
	if err != nil {
		return
	}
	obj[field] = bumped
}

// gateToolDefs maps each gate tool name to its verbatim definition, captured
// from the official client (freetier_tools.go). The 2026-10-14 gate validates
// the two definitions themselves — description and parameter schema — so a
// client that advertises its own bash/read under a different shape must still
// be presented with the official pair: the client's entries are replaced, not
// reused. Anything the client genuinely uses keeps working because the model
// calls the pinned pair by name and the client executes its own tool.

// topUpToolNames guarantees the request advertises the official bash+read
// definitions, pinned first.
//
// Order keeps the caller's tools after the stable gate pair for prompt-cache
// stability: agent clients churn their own tool list every few turns, and a
// changing first tool entry busts the entire tools prefix. The 2026-10-14
// matcher validates the gate pair's definitions verbatim, so client-supplied
// bash/read entries (whose schemas differ) are dropped in favour of the
// captured pair — a client entry that merely carried the name used to pass the
// old name-only matcher and would now poison the whole request. The gate
// definitions are rendered in the surface's own shape (chat nests the fields
// under "function", Responses keeps them flat); the checked content is
// identical either way.
func topUpToolNames(tools []json.RawMessage, shape toolShape) ([]byte, error) {
	if tools == nil {
		tools = []json.RawMessage{}
	}
	rest := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		if isGateToolName(toolNameOf(raw)) {
			// Dropped: the gate pair is re-stamped below verbatim.
			continue
		}
		rest = append(rest, raw)
	}
	out := make([]json.RawMessage, 0, len(gateToolNames)+len(rest))
	for _, name := range gateToolNames {
		def, ok := gateToolDefs[name]
		if !ok {
			// Unreachable while gateToolNames and gateToolDefs agree.
			return nil, fmt.Errorf("no verbatim definition for gate tool %q", name)
		}
		entry, err := def.render(shape)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	out = append(out, rest...)
	return json.Marshal(out)
}

func isGateToolName(name string) bool {
	for _, g := range gateToolNames {
		if name == g {
			return true
		}
	}
	return false
}

// idAlphabet is the character set the client draws its random id suffix from.
const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// idClock serializes the per-millisecond sequence the upstream expects inside
// a minted identifier. Live-probed 2026-10-13 (and independently by
// FishBottle7/opencode2dsh, issue #62): the free gate decodes the 12 hex
// characters back as a wall clock bitwise-inverted with a per-millisecond
// counter in the low 12 bits, and accepts only counter values in [0, 63]. A
// digest- or random-derived hex blob lands in that window with probability
// ~64/4096 ≈ 1.5% — which is what the earlier random mint was doing, and why
// the envelope was 403-shaped almost every time while the real CLI sailed
// through. The high clock bits are not checked (an id decoding to 1970
// passes); the counter is.
var idClock struct {
	sync.Mutex
	ms      int64
	counter uint32
}

// nextClockRaw yields now_ms*0x1000 + counter with the counter in [1, 63],
// mint-once per millisecond. If a single millisecond somehow sees 63 mints
// (a pathological ~63k ids/second) the caller yields until the clock ticks so
// ids never repeat: uniqueness beats latency here.
func nextClockRaw() int64 {
	for {
		idClock.Lock()
		now := time.Now().UnixMilli()
		if idClock.ms != now {
			idClock.ms = now
			idClock.counter = 0
		}
		if idClock.counter < 63 {
			idClock.counter++
			raw := now*0x1000 + int64(idClock.counter)
			idClock.Unlock()
			return raw
		}
		idClock.Unlock()
		runtime.Gosched()
	}
}

// clockHex renders the 12 hex characters of one identifier: the raw clock value
// masked to 48 bits, bitwise-inverted first for the descending (session) order.
// JS `~x` is -(x+1), and upstream's own generator inverts before masking, so
// the inversion happens on the raw value — matching that is the whole point.
func clockHex(raw int64, inverted bool) string {
	mask := uint64(1)<<48 - 1
	v := uint64(raw) & mask
	if inverted {
		v = ^uint64(raw) & mask
	}
	return fmt.Sprintf("%012x", v)
}

// randomBase62 draws n characters of the id tail exactly the way the client
// does: one random byte per character, modulo the base62 alphabet.
func randomBase62(n int) string {
	var buf [20]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on the platforms this ships to. A
		// timestamp-derived tail is worse than crashing a request over an id.
		binary.BigEndian.PutUint64(buf[:8], uint64(time.Now().UnixNano()))
	}
	var sb strings.Builder
	sb.Grow(n)
	for _, b := range buf[:n] {
		sb.WriteByte(idAlphabet[int(b)%len(idAlphabet)])
	}
	return sb.String()
}

// newRequestID mints an ascending identifier (`msg_…`): the plain clock plus
// counter, then the random tail — the order the real CLI emits for requests.
func newRequestID() string {
	return "msg_" + clockHex(nextClockRaw(), false) + randomBase62(14)
}

// newSessionID mints a descending identifier (`ses_…`): the clock inverted, so
// ids sort newest-first — the shape the real CLI emits for sessions.
func newSessionID() string {
	return "ses_" + clockHex(nextClockRaw(), true) + randomBase62(14)
}
