package forward

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"zenproxy/internal/registry"
)

// AccountFromProxyAuth extracts the account id from a forward-proxy request.
//
// Clients address an account through the proxy credentials:
//
//	proxy = http://zen-01:anything@zenproxy:9000
//
// A plain header is accepted as well, which keeps ad-hoc curl testing
// readable:
//
//	curl -H 'X-Zen-Account: zen-01' -x http://zenproxy:9000 https://...
func AccountFromProxyAuth(r *http.Request) string {
	if h := r.Header.Get("Proxy-Authorization"); h != "" {
		const prefix = "Basic "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			if raw, err := base64.StdEncoding.DecodeString(h[len(prefix):]); err == nil {
				user, _, _ := strings.Cut(string(raw), ":")
				if user = strings.TrimSpace(user); user != "" {
					return user
				}
			}
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Zen-Account"))
}

// hostAllowed enforces the forward-mode allowlist. Without one, an exposed
// listener would happily relay traffic to anywhere on the internet.
func (e *Engine) hostAllowed(host string) bool {
	if e.allow == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return e.allow[host]
}

// ServeForward handles the HTTP forward-proxy protocol, covering both the
// CONNECT tunnel used for https upstreams and plain absolute-form requests.
//
// This is the ingress new-api uses when a channel's "代理" field is set. Note
// the structural limit: an https upstream arrives as an opaque tunnel, so an
// upstream-side 403/429 is invisible here and only dial failures can trigger a
// rotation. Prefer the Base URL (reverse) ingress when rotation matters.
func (e *Engine) ServeForward(w http.ResponseWriter, r *http.Request) {
	accountID := AccountFromProxyAuth(r)
	if accountID == "" {
		w.Header().Set("Proxy-Authenticate", `Basic realm="zenproxy"`)
		writeJSONError(w, http.StatusProxyAuthRequired,
			"missing proxy credentials; address an account as http://<account-id>:x@<host>:<port>")
		return
	}
	ent, ok := e.reg.Get(accountID)
	if !ok || !ent.Enabled {
		writeJSONError(w, http.StatusForbidden, "unknown or disabled account: "+accountID)
		return
	}

	if r.Method == http.MethodConnect {
		e.tunnel(w, r, ent)
		return
	}
	e.forwardAbsolute(w, r, ent)
}

// tunnel relays a CONNECT request: it picks an exit, opens the TCP tunnel
// through it, and then copies bytes in both directions.
func (e *Engine) tunnel(w http.ResponseWriter, r *http.Request, ent *registry.Entry) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	if target == "" {
		writeJSONError(w, http.StatusBadRequest, "CONNECT without a target host")
		return
	}
	if !strings.Contains(target, ":") {
		target += ":443"
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil || !e.hostAllowed(host) {
		writeJSONError(w, http.StatusForbidden, "target host not in allowed_hosts: "+host)
		return
	}

	attempts := e.pol.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var (
		conn   net.Conn
		lastEr error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		exitID := ""
		proxy := e.reg.ExitFor(ent.ID)
		if proxy != nil {
			exitID = proxy.ID
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		var c net.Conn
		if proxy == nil {
			var d net.Dialer
			c, err = d.DialContext(ctx, "tcp", target)
		} else {
			c, err = proxy.DialContext(ctx, "tcp", target)
		}
		cancel()
		if err == nil {
			if exitID != "" {
				e.pool.MarkSuccess(exitID, 0)
			}
			conn = c
			break
		}
		lastEr = err
		if exitID != "" {
			e.pool.MarkFailure(exitID, err.Error())
		}
		e.log.Warn("CONNECT tunnel failed",
			"account", ent.ID, "exit", exitID, "target", target, "attempt", attempt, "err", err)
		if attempt < attempts {
			e.reg.Rotate(ent.ID, exitID)
		}
	}
	if conn == nil {
		code, why := clientFacingFailure("")
		if lastEr != nil {
			code, why = clientFacingFailure(lastEr.Error())
		}
		writeJSONErrorCode(w, http.StatusBadGateway, code, "no exit could reach "+target+": "+why)
		return
	}
	defer conn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "connection hijacking unavailable")
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "hijack failed: "+err.Error())
		return
	}
	defer client.Close()

	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		// rw.Reader may already hold bytes the client sent with the CONNECT.
		_, _ = io.Copy(conn, rw)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, conn)
		done <- struct{}{}
	}()
	<-done
}

// forwardAbsolute relays an absolute-form request to its own target without
// touching the caller's credentials. Upstream status codes are passed through
// untouched: in this mode the client owns the retry semantics, so only
// transport-level failures trigger an exit rotation.
func (e *Engine) forwardAbsolute(w http.ResponseWriter, r *http.Request, ent *registry.Entry) {
	if !r.URL.IsAbs() {
		writeJSONError(w, http.StatusBadRequest,
			"forward-proxy mode expects an absolute request URI, e.g. GET http://host/path HTTP/1.1")
		return
	}
	if !e.hostAllowed(r.URL.Hostname()) {
		writeJSONError(w, http.StatusForbidden, "target host not in allowed_hosts: "+r.URL.Hostname())
		return
	}
	body, truncated, err := readBody(r, e.pol.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}

	attempts := e.pol.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	if truncated {
		attempts = 1
	}
	target := r.URL.String()

	var lastMsg string
	for attempt := 1; attempt <= attempts; attempt++ {
		exitID := ""
		proxy := e.reg.ExitFor(ent.ID)
		if proxy != nil {
			exitID = proxy.ID
		}

		var reader io.Reader
		length := int64(len(body))
		switch {
		case truncated:
			reader = io.MultiReader(bytes.NewReader(body), r.Body)
			length = -1
		case body != nil:
			reader = bytes.NewReader(body)
		}

		ctx, cancel := context.WithTimeout(r.Context(), e.pol.UpstreamTimeout)
		req, err := http.NewRequestWithContext(ctx, r.Method, target, reader)
		if err != nil {
			cancel()
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.ContentLength = length
		for k, vals := range r.Header {
			lk := strings.ToLower(k)
			if hopHeaders[lk] || lk == "host" || lk == "content-length" {
				continue
			}
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}

		resp, err := e.clientFor(proxy, false).Do(req)
		if err != nil {
			cancel()
			lastMsg = err.Error()
			if exitID != "" {
				e.pool.MarkFailure(exitID, err.Error())
			}
			e.log.Warn("forward transport failure",
				"account", ent.ID, "exit", exitID, "target", target, "attempt", attempt, "err", err)
			if attempt < attempts {
				e.reg.Rotate(ent.ID, exitID)
				continue
			}
			break
		}
		e.writeStream(w, resp)
		cancel()
		return
	}
	code, why := clientFacingFailure(lastMsg)
	writeJSONErrorCode(w, http.StatusBadGateway, code,
		fmt.Sprintf("all %d attempt(s) failed for account %s: %s", attempts, ent.ID, why))
}
