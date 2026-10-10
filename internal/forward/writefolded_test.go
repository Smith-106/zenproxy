package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenproxy/internal/config"
)

// A fold that could not produce an answer is an upstream failure, so the status
// code has to say so.
//
// This is the regression the two-result contract exists for. The fold used to
// return an error envelope as an ordinary payload, writeFolded wrote it under an
// unconditional 200, and new-api relayed that 200 verbatim: the caller saw
// success, then found no `choices` to read. Reporting the problem only in the
// body is not enough when something else is in the path.
func TestWriteFoldedReportsAFailedFoldAsBadGateway(t *testing.T) {
	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}}

	for _, s := range []surface{surfaceChat, surfaceResponses} {
		rec := httptest.NewRecorder()
		resp := &http.Response{Body: io.NopCloser(strings.NewReader("<html>502 from a gateway</html>"))}
		e.writeFolded(rec, resp, s)

		if rec.Code != http.StatusBadGateway {
			t.Errorf("surface %d: status = %d, want %d; body = %s",
				s, rec.Code, http.StatusBadGateway, rec.Body.String())
		}
		var env struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("surface %d: body is not JSON: %v\n%s", s, err, rec.Body.String())
		}
		if env.Error.Type != "zenproxy_error" || env.Error.Message == "" {
			t.Errorf("surface %d: envelope = %+v", s, env.Error)
		}
	}
}

// The happy path must be untouched: a fold that worked still answers 200 with
// the reply and still claims it is JSON.
func TestWriteFoldedStillAnswersTwoHundredOnSuccess(t *testing.T) {
	e := &Engine{pol: config.Policy{MaxBodyBytes: 1 << 20}}
	rec := httptest.NewRecorder()
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(sampleStream))}
	e.writeFolded(rec, resp, surfaceChat)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Paris is the capital.") {
		t.Errorf("body = %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}
