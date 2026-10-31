package config

import (
	"io"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	env := map[string]string{"PED_MODE": "passthrough", "PED_MAX_EVENT": "64KiB", "PED_RESYNC": "5m"}
	cfg, err := Parse([]string{"--mode", "readonly"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeReadonly {
		t.Errorf("flag should override env: mode = %s", cfg.Mode)
	}
	if cfg.MaxEventBytes != 64<<10 || cfg.ResyncInterval != 5*time.Minute || cfg.SocketMode != 0o660 {
		t.Errorf("unexpected config %+v", cfg)
	}
}

func TestParseRejects(t *testing.T) {
	for _, args := range [][]string{
		{"--mode", "rw"},
		{"--listen", "/tmp/x.sock"},
		{"--upstream", "tcp://localhost:2375"},
		{"--repeat-after-transition", "-1"},
		{"--resync-interval", "-1m"},
		{"--max-event-bytes", "2GiB"},
		{"--max-event-bytes", "0"},
		{"--listen-socket-mode", "999"},
		{"--log-level", "loud"},
	} {
		if _, err := Parse(args, func(string) string { return "" }, io.Discard); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}
