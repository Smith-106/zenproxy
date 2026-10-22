// Command zenproxy is an egress daemon: it gives every account in a pool its
// own, health-checked way out to the internet, and rotates that exit when the
// failure is one an exit can fix.
//
// It fronts two ingress shapes on a single listener:
//
//	reverse   http://<host>:9000/<account-id>/<path>   for new-api channel Base URL
//	forward   http://<account-id>:x@<host>:9000        for new-api channel proxy field
//
// It exists because neither new-api nor LiteLLM can rotate a channel's
// outbound proxy: the setting is a single static value, so one dead exit takes
// the whole channel down with it.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"zenproxy/internal/admin"
	"zenproxy/internal/config"
	"zenproxy/internal/forward"
	"zenproxy/internal/proxypool"
	"zenproxy/internal/registry"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// poolStatePath puts the exit-health sidecar file next to the registry's
// state.json. The name includes the hostname so two replicas on the same
// volume do not take turns overwriting each other's retirements and
// admissions. A missing per-host file is seeded from the old shared
// exits-state.json once; see proxypool.loadState.
func poolStatePath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "local"
	}
	host = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ':
			return '-'
		default:
			return r
		}
	}, host)
	return filepath.Join(filepath.Dir(stateFile), "exits-state-"+host+".json")
}

// sessionFileSyncPath is the shared sticky-table file: a sibling of the
// registry state file, in the same writable bind mount.
func sessionFileSyncPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "sessions.json")
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	pool, err := proxypool.NewPool(proxypool.SpecsFromConfig(cfg.Proxies), cfg.Health, logger, poolStatePath(cfg.StateFile))
	if err != nil {
		return err
	}

	reg := registry.New(cfg.StateFile, pool, logger)
	if err := reg.Load(); err != nil {
		return err
	}
	if cfg.Policy.SessionExitTTL > 0 {
		c := registry.NewSessionExitCache(cfg.Policy.SessionExitTTL, 4096)
		reg.SetSessionExitCache(c)
		// Two replicas share one DNS name; without this file the session→exit
		// pins stay per-replica, every conversation hops exits every other
		// turn, and the upstream cache warmth collapses (measured 60% → 1.2%).
		c.StartFileSync(sessionFileSyncPath(cfg.StateFile), 5*time.Second, logger)
		logger.Info("session→exit stickiness enabled", "ttl", cfg.Policy.SessionExitTTL, "sync", sessionFileSyncPath(cfg.StateFile))
	}
	if n := reg.Seed(cfg.Accounts); n > 0 {
		logger.Info("seeded accounts from config", "count", n)
		if err := reg.Save(); err != nil {
			return err
		}
	}

	engine := forward.New(reg, pool, forward.Options{
		Upstreams:    cfg.Upstreams,
		Policy:       cfg.Policy,
		AllowedHosts: cfg.Policy.AllowedHosts,
		FreeTier:     cfg.FreeTier,
		Log:          logger,
		Gateway:      cfg.Gateway,
		Pools:        cfg.Pools,
		ModelList:    cfg.ModelList,
	})
	adm := admin.New(reg, pool, cfg, engine.FreeTier(), logger)
	mux := newMux(engine, adm)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           logRequests(logger, reg, mux),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       cfg.Server.IdleTimeout,
		// No write timeout: SSE responses are intentionally long-lived.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go pool.Run(ctx, cfg.Health.Interval)

	shutdown := make(chan struct{})
	stateSaved := make(chan struct{})
	go func() {
		defer close(stateSaved)
		reg.RunSaver(shutdown, 30*time.Second)
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("zenproxy listening",
			"addr", cfg.Server.Listen,
			"accounts", len(reg.List()),
			"exits", pool.Len(),
			"exits_healthy", pool.HealthyCount(),
			"state_file", cfg.StateFile,
			"allowlist", len(cfg.Policy.AllowedHosts))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		close(shutdown)
		<-stateSaved
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	close(shutdown)
	<-stateSaved
	return nil
}

// newMux builds the request router.
//
// It is separate from run() because the precedence it encodes is the part most
// likely to break silently: three ingress shapes share one listener, and
// getting the order wrong turns a working path into a puzzling 404 rather than
// an error anyone would notice at startup.
func newMux(engine *forward.Engine, adm *admin.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/admin/", adm.Handler())
	mux.HandleFunc("/healthz", adm.Health)
	mux.HandleFunc("/metrics", adm.Metrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A forward-proxy request either asks for a tunnel or carries an
		// absolute URI. Everything else is reverse-mode traffic.
		if r.URL.IsAbs() {
			engine.ServeForward(w, r)
			return
		}
		// The unified ingress claims /v1/* before the account split. Order
		// matters: splitAccount runs first and every gateway client would be
		// told "unknown or disabled account: v1".
		if p := r.URL.Path; p == "/v1" || strings.HasPrefix(p, "/v1/") {
			engine.ServeGateway(w, r, p)
			return
		}
		account, rest := splitAccount(r.URL.Path)
		if account == "" {
			forward.WriteError(w, http.StatusNotFound,
				"usage: POST /v1/chat/completions with an API key, http://<host>:<port>/<account-id>/<upstream-path>, or use this listener as an HTTP proxy with http://<account-id>:x@<host>:<port>")
			return
		}
		engine.ServeReverse(w, r, account, rest)
	})
	return tunnelFirst(engine, mux)
}

// tunnelFirst hands CONNECT to the forward ingress before ServeMux can answer it.
//
// ServeMux dispatches on r.URL.Path, and a CONNECT request has no path: the
// request line is "CONNECT host:port", so net/http parses a URL with an empty
// Path. Nothing matches, the mux's own "404 page not found" is written, and the
// branch that implements tunnelling is never reached.
//
// It failed silently in production for exactly that reason. curl reports
// "Received HTTP code 404 from proxy after CONNECT", the log line has no account
// in it and reads like any other unknown path, and the reverse ingress on the
// same listener works - so the reader looks at credentials, the allowlist or the
// target host. The tests could not catch it either: package forward attaches
// ServeForward to a server of its own, so nothing put a CONNECT through a mux.
//
// Only CONNECT is lifted here. An absolute-form request keeps its existing
// precedence, where /admin/, /healthz and /metrics still win over the proxy
// branch - that ordering predates this and is not worth changing to make the
// fix tidier.
func tunnelFirst(engine *forward.Engine, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			engine.ServeForward(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// splitAccount pulls the account id off the front of a reverse-mode path.
//
//	/zen-01/v1/chat/completions -> ("zen-01", "/v1/chat/completions")
//	/zen-01                     -> ("zen-01", "")
func splitAccount(p string) (account, rest string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i:]
	}
	return p, ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	start  time.Time
	first  int64
}

func (r *statusRecorder) mark() {
	if r.first != 0 || r.start.IsZero() {
		return
	}
	r.first = time.Since(r.start).Milliseconds()
	if r.first < 1 {
		r.first = 1
	}
}

func (r *statusRecorder) WriteHeader(code int) {
	r.mark()
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.mark()
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush keeps SSE working through the wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	return h.Hijack()
}

func logRequests(logger *slog.Logger, reg *registry.Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admin and health traffic is noise in the request log.
		if strings.HasPrefix(r.URL.Path, "/admin") || r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, start: start}
		r = r.WithContext(registry.WithCacheNote(r.Context()))
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		waited := time.Since(start).Milliseconds()
		if id, _ := splitAccount(r.URL.Path); id != "" && reg != nil {
			reg.NoteRequestDuration(id, waited)
			reg.NoteRequestHeader(id, rec.first)
			reg.NoteRequestCache(id, registry.CachePctFrom(r.Context()))
		}
		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", waited,
			"header_ms", rec.first)
	})
}
