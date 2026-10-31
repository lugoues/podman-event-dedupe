// Package config parses flags and PED_* environment variables (§6).
package config

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeReadonly    Mode = "readonly"
	ModePassthrough Mode = "passthrough"
)

type Config struct {
	Listen                string
	SocketMode            fs.FileMode
	Upstream              string
	Mode                  Mode
	RepeatAfterTransition int
	ResyncInterval        time.Duration
	MaxEventBytes         int
	LogLevel              slog.Level
}

// Parse reads args (without the program name), using env vars as defaults.
// Flags take precedence over env vars.
func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	fset := flag.NewFlagSet("podman-event-dedupe", flag.ContinueOnError)
	fset.SetOutput(output)

	env := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	listen := fset.String("listen", env("PED_LISTEN", "unix:///run/ped/docker.sock"), "listen address (unix:// or tcp://) [PED_LISTEN]")
	socketMode := fset.String("listen-socket-mode", env("PED_SOCKET_MODE", "0660"), "permissions for the unix listen socket [PED_SOCKET_MODE]")
	upstream := fset.String("upstream", env("PED_UPSTREAM", "unix:///run/podman/podman.sock"), "Podman API socket [PED_UPSTREAM]")
	mode := fset.String("mode", env("PED_MODE", string(ModeReadonly)), "readonly or passthrough [PED_MODE]")
	repeat := fset.String("repeat-after-transition", env("PED_REPEAT", "1"), "same-status events forwarded after a change [PED_REPEAT]")
	resync := fset.String("resync-interval", env("PED_RESYNC", "0"), "forward at most one health event per stream per interval; 0 disables [PED_RESYNC]")
	maxEvent := fset.String("max-event-bytes", env("PED_MAX_EVENT", "4MiB"), "largest event to parse [PED_MAX_EVENT]")
	logLevel := fset.String("log-level", env("PED_LOG", "info"), "debug, info, warn or error [PED_LOG]")

	if err := fset.Parse(args); err != nil {
		return Config{}, err
	}

	cfg := Config{Listen: *listen, Upstream: *upstream, Mode: Mode(*mode)}
	var err error

	if !strings.HasPrefix(cfg.Listen, "unix://") && !strings.HasPrefix(cfg.Listen, "tcp://") {
		return Config{}, fmt.Errorf("listen: must start with unix:// or tcp://, got %q", cfg.Listen)
	}
	if !strings.HasPrefix(cfg.Upstream, "unix://") {
		return Config{}, fmt.Errorf("upstream: must start with unix://, got %q", cfg.Upstream)
	}
	if cfg.Mode != ModeReadonly && cfg.Mode != ModePassthrough {
		return Config{}, fmt.Errorf("mode: must be readonly or passthrough, got %q", cfg.Mode)
	}
	m, err := strconv.ParseUint(*socketMode, 8, 32)
	if err != nil {
		return Config{}, fmt.Errorf("listen-socket-mode: %w", err)
	}
	cfg.SocketMode = fs.FileMode(m)
	if cfg.RepeatAfterTransition, err = strconv.Atoi(*repeat); err != nil || cfg.RepeatAfterTransition < 0 {
		return Config{}, fmt.Errorf("repeat-after-transition: must be a non-negative integer, got %q", *repeat)
	}
	if *resync != "0" {
		if cfg.ResyncInterval, err = time.ParseDuration(*resync); err != nil || cfg.ResyncInterval < 0 {
			return Config{}, fmt.Errorf("resync-interval: must be 0 or a positive duration, got %q", *resync)
		}
	}
	if cfg.MaxEventBytes, err = parseBytes(*maxEvent); err != nil {
		return Config{}, fmt.Errorf("max-event-bytes: %w", err)
	}
	if err = cfg.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return Config{}, fmt.Errorf("log-level: %w", err)
	}
	return cfg, nil
}

// LoadFromOS parses os.Args and the process environment.
func LoadFromOS() (Config, error) {
	return Parse(os.Args[1:], os.Getenv, os.Stderr)
}

// maxEventLimit caps --max-event-bytes so the size can't overflow.
const maxEventLimit = 1 << 30

// parseBytes accepts a plain integer or one suffixed with KiB, MiB or GiB,
// up to maxEventLimit.
func parseBytes(s string) (int, error) {
	units := []struct {
		suffix string
		mult   int
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}}
	mult := 1
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = n, u.mult
			break
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > maxEventLimit/mult {
		return 0, fmt.Errorf("invalid size %q (must be 1 to 1GiB)", s)
	}
	return n * mult, nil
}
