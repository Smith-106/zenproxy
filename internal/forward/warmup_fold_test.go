package forward

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

// The streaming-chat bridge folds a muse Responses SSE stream into one chat
// completion object. Inside the hold window NOTHING may reach the client: a
// stream that dies during muse's pre-text warmup (the measured 137k-context
// pattern: ~10s of reasoning deltas, then the edge reaps the tunnel) must
// surface as a plain fold error so the attempt loop re-rolls invisibly,
// instead of committing "ok, 20 tokens" fragments the harness reads as an
// idle model.
func TestFoldChatStreamInteractiveWarmupDeathStaysUncommitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Warmup only: a reasoning output item, no message deltas, no
		// response.completed. Then the body ends — an edge reap.
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n"))
	}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("probe upstream: %v", err)
	}
	defer resp.Body.Close()

	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}, log: testLogger()}
	rec := httptest.NewRecorder()
	chat, foldErr, hdrSent := e.foldChatStreamInteractive(rec, resp, &GatePlan{Surface: surfaceResponses, ReplyAsChat: true, Stream: true})
	if foldErr == "" {
		t.Fatalf("warmup death must fold-error, got chat=%d bytes", len(chat))
	}
	if hdrSent {
		t.Fatalf("no client byte may be committed inside the hold (body=%q)", rec.Body.String())
	}
	if len(chat) != 0 {
		t.Fatalf("no chat object may escape a dead fold: %q", string(chat))
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("recorder body must stay empty for the invisible re-roll: %q", rec.Body.String())
	}
}

func TestFoldChatStreamInteractiveFastCompleteStreamsOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"created_at\":1,\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"all done\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"))
	}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("probe upstream: %v", err)
	}
	defer resp.Body.Close()

	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}, log: testLogger()}
	rec := httptest.NewRecorder()
	chat, foldErr, hdrSent := e.foldChatStreamInteractive(rec, resp, &GatePlan{Surface: surfaceResponses, ReplyAsChat: true, Stream: true})
	if foldErr != "" {
		t.Fatalf("complete stream must not error: %v", foldErr)
	}
	if !hdrSent {
		t.Fatal("complete stream must commit and emit")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") || !strings.Contains(body, "all done") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("emitted SSE missing pieces: %q", body[:min(len(body), 300)])
	}
	if len(chat) == 0 {
		t.Fatal("the folded chat object must be returned for cache accounting")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
