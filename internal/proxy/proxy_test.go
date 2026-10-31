package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lugoues/podman-compat-proxy/internal/config"
	"github.com/lugoues/podman-compat-proxy/internal/events"
)

// setup starts a fake Podman on a temp unix socket and the proxy in front of it.
func setup(t *testing.T, upstream http.HandlerFunc, mutate func(*config.Config)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "podman.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewUnstartedServer(upstream)
	up.Listener = ln
	up.Start()
	t.Cleanup(up.Close)

	cfg := config.Config{
		Upstream:              "unix://" + sock,
		Mode:                  config.ModeReadonly,
		RepeatAfterTransition: 1,
		MaxEventBytes:         4 << 20,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	down := httptest.NewServer(New(cfg, &events.Stats{}, log))
	t.Cleanup(down.Close)
	return down.URL
}

// replay serves lines as an event stream, flushing after each, then closes.
func replay(lines []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		for _, l := range lines {
			io.WriteString(w, l+"\n")
			w.(http.Flusher).Flush()
		}
	}
}

func readLines(t *testing.T, url string) []string {
	t.Helper()
	return readPath(t, url+"/v1.41/events")
}

func readPath(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEventsFixtureReplay(t *testing.T) {
	tests := []struct {
		file    string
		dropped []int // zero-based line indexes expected to be dropped (repeat=1)
	}{
		// Third consecutive "healthy" for hc (line 7) and hc2 (line 8).
		{"podman-5.4.2-compat-events.jsonl", []int{7, 8}},
		// Every health event is a first sighting, a change, or the one repeat after it.
		{"podman-5.8.1-compat-events.jsonl", nil},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile("../events/testdata/" + tt.file)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			url := setup(t, replay(lines), nil)

			var want []string
			for i, l := range lines {
				if !slices.Contains(tt.dropped, i) {
					want = append(want, l)
				}
			}
			if got := readLines(t, url); !slices.Equal(got, want) {
				t.Errorf("got %d lines, want %d\ngot:  %q\nwant: %q", len(got), len(want), got, want)
			}
		})
	}
}

// healthLine builds a health event for id, padded with an attribute to size bytes.
func healthLine(id, status string, size int) string {
	head := `{"Type":"container","Action":"health_status","Actor":{"ID":"` + id + `","Attributes":{"pad":"`
	tail := `"}},"HealthStatus":"` + status + `"}`
	return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
}

func TestEventsRawBytesAndSizeLimits(t *testing.T) {
	const limit = 8000
	lines := []string{
		healthLine("a", "healthy", 200),
		`{"Type":"container","Action":`,                  // malformed: forwarded raw
		healthLine("o", "healthy", 3*limit),              // oversized, spans several reads: forwarded raw
		healthLine("o", "healthy", 3*limit),              // oversized repeat: still forwarded (never parsed)
		healthLine("m", "healthy", 5000),                 // larger than the read buffer, under the limit
		healthLine("b", "healthy", limit),                // exactly at the limit: parsed
		healthLine("b", "healthy", limit),                // so this duplicate is dropped
		"  " + healthLine("a", "unhealthy", 200) + " \r", // whitespace and CR preserved
	}
	url := setup(t, replay(lines), func(c *config.Config) {
		c.MaxEventBytes = limit
		c.RepeatAfterTransition = 0
	})

	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	forwarded := slices.Delete(slices.Clone(lines), 6, 7)
	want := strings.Join(forwarded, "\n") + "\n"
	if string(got) != want {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(want))
		for i, l := range strings.SplitAfter(string(got), "\n") {
			t.Logf("got line %d: %d bytes", i, len(l))
		}
	}
}

func TestEventsFlushedPromptly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	url := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"Type":"container","Action":"start","Actor":{"ID":"a"}}`+"\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, nil)

	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, `"start"`) {
			t.Errorf("unexpected line %q", line)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("event not flushed within 100ms")
	}
}

func TestEventsUpstreamCloseEndsDownstream(t *testing.T) {
	url := setup(t, replay(nil), nil)
	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream still open after upstream closed")
	}
}

func TestEventsClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamDone := make(chan struct{})
	url := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}, nil)

	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request not cancelled after client disconnect")
	}
}

func TestLibpodEventsNotDeduped(t *testing.T) {
	line := healthLine("a", "healthy", 200)
	lines := []string{line, line, line}
	url := setup(t, replay(lines), func(c *config.Config) { c.Mode = config.ModePassthrough })

	if got := readPath(t, url+"/v5.0.0/libpod/events"); !slices.Equal(got, lines) {
		t.Errorf("got %d lines, want %d", len(got), len(lines))
	}
}

func TestEventsNon200PassedThrough(t *testing.T) {
	url := setup(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"bad filter"}`, http.StatusBadRequest)
	}, nil)
	resp, err := http.Get(url + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "bad filter") {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
}

func TestReadonlyAndPassthrough(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.41")
		io.WriteString(w, "OK")
	}

	ro := setup(t, upstream, nil)
	resp, err := http.Post(ro+"/v1.41/containers/a/stop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || string(body) != `{"message":"blocked by podman-event-dedupe"}` {
		t.Errorf("readonly POST: got %d %q", resp.StatusCode, body)
	}

	resp, err = http.Get(ro + "/_ping")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Api-Version") != "1.41" {
		t.Errorf("readonly ping: got %d, Api-Version %q", resp.StatusCode, resp.Header.Get("Api-Version"))
	}

	pt := setup(t, upstream, func(c *config.Config) { c.Mode = config.ModePassthrough })
	resp, err = http.Post(pt+"/v1.41/containers/a/stop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("passthrough POST: got %d", resp.StatusCode)
	}
}
