// Free-tier structural gate constants.
//
// Measured against https://opencode.ai/zen/v1: the free-tier matcher no longer
// requires a verbatim client system prompt. A request that carries the official
// client identity headers, `"stream": true`, and the two tool names below is
// accepted on both /chat/completions and /responses. Prompt injection is
// deliberately not used — the old title/agent chunks polluted weaker models
// (e.g. step-5-preview-free answering "Greeting" / "Short joke request").
package forward

// gateToolNames are the tool names the free-tier matcher looks for. Schemas,
// descriptions, order, and any extra client tools are ignored — only these
// names matter. Two is enough: bash + read alone returns HTTP 200.
var gateToolNames = []string{"bash", "read"}

// gateMinToolNames is how many of gateToolNames a request must carry.
const gateMinToolNames = 2
