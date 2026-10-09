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

func run() error {
	configPath := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	pool, err := proxypool.NewPool(proxypool.SpecsFromConfig(cfg.Proxies), cfg.Health, logger)
	if err != nil {
		return err
	}

	reg := registry.New(cfg.StateFile, pool, logger)
	if err := reg.Load(); err != nil {
		return err
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
	})
	adm := admin.New(reg, pool, cfg, engine.FreeTier(), logger)

	mux := http.NewServeMux()
	mux.Handle("/admin/", adm.Handler())
	mux.HandleFunc("/healthz", adm.Health)
	mux.HandleFunc("/metrics", adm.Metrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A forward-proxy request either asks for a tunnel or carries an
		// absolute URI. Everything else is reverse-mode traffic.
		if r.Method == http.MethodConnect || r.URL.IsAbs() {
			engine.ServeForward(w, r)
			return
		}
		account, rest := splitAccount(r.URL.Path)
		if account == "" {
			forward.WriteError(w, http.StatusNotFound,
				"usage: http://<host>:<port>/<account-id>/<upstream-path>, or use this listener as an HTTP proxy with http://<account-id>:x@<host>:<port>")
			return
		}
		engine.ServeReverse(w, r, account, rest)
	})

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           logRequests(logger, mux),
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
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
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

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admin and health traffic is noise in the request log.
		if strings.HasPrefix(r.URL.Path, "/admin") || r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds())
	})
}
