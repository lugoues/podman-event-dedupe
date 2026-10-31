package main

import (
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

	"github.com/lugoues/podman-compat-proxy/internal/config"
	"github.com/lugoues/podman-compat-proxy/internal/events"
	"github.com/lugoues/podman-compat-proxy/internal/proxy"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, err := config.LoadFromOS()
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	log.Info("starting", "version", version,
		"listen", cfg.Listen, "upstream", cfg.Upstream, "mode", cfg.Mode,
		"repeat", cfg.RepeatAfterTransition, "resync", cfg.ResyncInterval,
		"max_event_bytes", cfg.MaxEventBytes)

	ln, err := listen(cfg)
	if err != nil {
		return err
	}
	// Closing a unix listener also removes its socket file.
	defer ln.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:     proxy.New(cfg, &events.Stats{}, log),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Event streams watch the request context (derived from ctx), so they
	// end now; Shutdown then waits for in-flight requests (§11).
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return srv.Close()
	}
	return nil
}

// listen opens the listener. For unix sockets it first removes a stale
// socket file, refusing if the path is a live socket or not a socket.
func listen(cfg config.Config) (net.Listener, error) {
	if addr, ok := strings.CutPrefix(cfg.Listen, "tcp://"); ok {
		return net.Listen("tcp", addr)
	}
	path := strings.TrimPrefix(cfg.Listen, "unix://")
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, cfg.SocketMode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("%s is in use by another process", path)
	}
	// Only "connection refused" proves nothing is listening.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("%s: cannot tell whether socket is stale: %w", path, err)
	}
	return os.Remove(path)
}
