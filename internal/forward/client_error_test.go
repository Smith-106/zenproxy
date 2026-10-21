package forward

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zenproxy/internal/model"
)

func TestClientFacingFailureHidesTheExit(t *testing.T) {
	raw := `Post "https://upstream.example/v1/responses": proxyconnect tcp: dial tcp 10.1.2.3:3129: i/o timeout (proxy http://user:s3cret@10.1.2.3:3129)`
	code, text := clientFacingFailure(raw)
	if code != "exit_unreachable" {
		t.Fatalf("code = %q", code)
	}
	if strings.Contains(text, "10.1.2.3") || strings.Contains(text, "s3cret") || strings.Contains(text, "user:") {
		t.Fatalf("client text still names the exit: %q", text)
	}
	if text != "the exit could not reach the upstream" {
		t.Fatalf("text = %q", text)
	}
}

func TestScrubClientTextDropsAddressesWhenTheClassIsUnknown(t *testing.T) {
	got := scrubClientText("dial tcp 203.0.113.8:443: read from http://alice:hunter2@203.0.113.8:3128 failed")
	if strings.Contains(got, "203.0.113.8") || strings.Contains(got, "hunter2") || strings.Contains(got, "alice:") {
		t.Fatalf("scrubbed = %q", got)
	}
	if !strings.Contains(got, "<exit>") {
		t.Fatalf("scrubbed = %q, want the address replaced", got)
	}
}

func TestWriteClientErrorHidesAURLAndKeepsAPlainCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeClientError(rec, http.StatusInternalServerError, "bad_upstream",
		`bad upstream base_url "https://alice:hunter2@proxy.example:3128/v1": parse error`)
	if rec.Header().Get("X-Zen-Error") != "bad_upstream" {
		t.Fatalf("header = %q", rec.Header().Get("X-Zen-Error"))
	}
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") || strings.Contains(body, "proxy.example") || strings.Contains(body, "https://") {
		t.Fatalf("body still names the upstream: %s", body)
	}

	dial := httptest.NewRecorder()
	writeClientError(dial, http.StatusBadRequest, "bad_request",
		`Post "https://upstream.example/v1": proxyconnect tcp: dial tcp 10.1.2.3:3129: i/o timeout`)
	if dial.Header().Get("X-Zen-Error") != "exit_unreachable" {
		t.Fatalf("dial header = %q", dial.Header().Get("X-Zen-Error"))
	}
	if strings.Contains(dial.Body.String(), "10.1.2.3") || strings.Contains(dial.Body.String(), "upstream.example") {
		t.Fatalf("dial body = %s", dial.Body.String())
	}
}

func TestWriteJSONErrorCodeIsWhatTheNextHopPrints(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONErrorCode(rec, http.StatusBadGateway, "exit_unreachable", "all 3 attempt(s) failed for account zen-03: the exit could not reach the upstream")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Zen-Error") != "exit_unreachable" {
		t.Fatalf("header = %q", rec.Header().Get("X-Zen-Error"))
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Type != "zenproxy_error" || env.Error.Code != "exit_unreachable" {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if strings.Contains(env.Error.Message, "dial tcp") {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

func TestWriteUpstreamResponsePutsTheClassWhereTheNextHopKeepsIt(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Zen-Error":  []string{"spoofed"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":"slow down"}`)),
	}
	writeUpstreamResponse(rec, resp, []byte(`{"error":"slow down"}`), model.ClassRateLimit)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("X-Zen-Error") != "rate_limit" {
		t.Fatalf("header = %q", rec.Header().Get("X-Zen-Error"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "slow down") || !strings.Contains(body, `"code":"rate_limit"`) {
		t.Fatalf("body = %s", body)
	}

	kept := stampUpstreamClass([]byte(`{"error":{"message":"nope","code":"vendor_code"}}`), model.ClassRateLimit)
	if strings.Contains(string(kept), "rate_limit") || !strings.Contains(string(kept), "vendor_code") {
		t.Fatalf("vendor code was replaced: %s", kept)
	}
	plain := stampUpstreamClass([]byte("error code: 1010"), model.ClassAuth)
	if !strings.Contains(string(plain), `"code":"auth"`) || !strings.Contains(string(plain), "1010") {
		t.Fatalf("plain = %s", plain)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestWriteSSEErrorNamesTheFold(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEError(rec, nil, "upstream_fold", "dial tcp 203.0.113.9:443: EOF")
	if strings.Contains(rec.Body.String(), "203.0.113.9") {
		t.Fatalf("sse = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"upstream_fold"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("sse = %s", rec.Body.String())
	}
}
