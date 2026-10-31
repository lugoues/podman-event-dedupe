// Package proxy routes requests to the events handler or a reverse proxy (§5, §8, §9).
package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"

	"github.com/lugoues/podman-compat-proxy/internal/config"
	"github.com/lugoues/podman-compat-proxy/internal/events"
)

var eventsPath = regexp.MustCompile(`^(/v[0-9.]+)?/events$`)

// UpstreamTransport dials the Podman unix socket for every request.
func UpstreamTransport(socketPath string) *http.Transport {
	return &http.Transport{
		// Pass bodies through as-is; never request compression ourselves.
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
}

// New builds the top-level handler.
func New(cfg config.Config, stats *events.Stats, log *slog.Logger) http.Handler {
	transport := UpstreamTransport(strings.TrimPrefix(cfg.Upstream, "unix://"))

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "podman"})
			pr.Out.Host = pr.In.Host
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	ev := &events.Handler{
		Upstream:      &http.Client{Transport: transport},
		Repeat:        cfg.RepeatAfterTransition,
		Resync:        cfg.ResyncInterval,
		MaxEventBytes: cfg.MaxEventBytes,
		Stats:         stats,
		Log:           log,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ped/stats" && r.Method == http.MethodGet {
			writeStats(w, stats)
			return
		}
		if cfg.Mode == config.ModeReadonly && !Allowed(r.Method, r.URL.Path) {
			log.Warn("blocked request", "method", r.Method, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"blocked by podman-event-dedupe"}`))
			return
		}
		if r.Method == http.MethodGet && eventsPath.MatchString(r.URL.Path) {
			ev.ServeHTTP(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

func writeStats(w http.ResponseWriter, s *events.Stats) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{
		"events_in":        s.EventsIn.Load(),
		"events_forwarded": s.EventsForwarded.Load(),
		"health_dropped":   s.HealthDropped.Load(),
		"streams_active":   s.StreamsActive.Load(),
	})
}
