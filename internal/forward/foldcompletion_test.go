package forward

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

var errFoldForTest = errors.New("boom")

// foldCompletion is the pre-commit fold: everything the plain completion path
// needs, nothing written to the client. A truncation becomes an ordinary error
// string the caller can rotate on.
func TestFoldCompletionFoldsAndConverts(t *testing.T) {
	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}}
	sse := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed"}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	out, ferr := e.foldCompletion(resp, &GatePlan{Surface: surfaceResponses})
	if ferr != "" {
		t.Fatalf("fold error: %s", ferr)
	}
	if !strings.Contains(string(out), "hello") {
		t.Fatalf("folded body lost the text: %s", out)
	}

	// A read failure must surface as a fold error too — nothing written, so
	// the caller still owns the whole rotation budget.
	respFail := &http.Response{Body: io.NopCloser(errReaderForFold{})}
	if _, ferr := e.foldCompletion(respFail, &GatePlan{Surface: surfaceResponses}); ferr == "" {
		t.Fatal("a read failure must be a fold error")
	}

	// Truncation semantics (deliberate, from the mid-stream-death work): a
	// stream that died AFTER delivering some text folds into a completed
	// object holding the partial answer — better than a 502 — while events
	// with NO text at all fold into an honest fold error.
	resp2 := &http.Response{Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"par"}` + "\n\n"))}
	out2, _ := e.foldCompletion(resp2, &GatePlan{Surface: surfaceResponses})
	if !strings.Contains(string(out2), "par") {
		t.Fatalf("a truncated stream with text must yield the partial answer: %s", out2)
	}
	resp3 := &http.Response{Body: io.NopCloser(strings.NewReader("event: response.created\n" +
		`data: {"type":"response.created"}` + "\n\n"))}
	if _, ferr := e.foldCompletion(resp3, &GatePlan{Surface: surfaceResponses}); ferr == "" {
		t.Fatal("a stream with no text at all must be a fold error")
	}
}

// The ReplyAsChat plain case (client asked for non-streaming chat) comes back
// as chat JSON, ready for writeCompleted — with usage intact for the
// cache-accounting line.
func TestFoldCompletionReplyAsChatPlain(t *testing.T) {
	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}}
	sse := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hey"}]}],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sse))}
	out, ferr := e.foldCompletion(resp, &GatePlan{Surface: surfaceResponses, ReplyAsChat: true})
	if ferr != "" {
		t.Fatalf("fold error: %s", ferr)
	}
	if !strings.Contains(string(out), `"content"`) || !strings.Contains(string(out), `"hey"`) {
		t.Fatalf("converted chat completion missing content: %s", out)
	}
	if !strings.Contains(string(out), `"usage"`) {
		t.Fatalf("usage must survive the muse conversion: %s", out)
	}
}

type errReaderForFold struct{}

func (errReaderForFold) Read([]byte) (int, error) { return 0, errFoldForTest }
func (errReaderForFold) Close() error             { return nil }
