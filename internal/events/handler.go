package events

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Stats are the counters served at /_ped/stats (§10).
type Stats struct {
	EventsIn        atomic.Int64
	EventsForwarded atomic.Int64
	HealthDropped   atomic.Int64
	StreamsActive   atomic.Int64
}

// Handler serves the compat /events stream (§7.2).
type Handler struct {
	Upstream      *http.Client // must not have a Timeout; streams are long-lived
	Repeat        int
	Resync        time.Duration
	MaxEventBytes int
	Stats         *Stats
	Log           *slog.Logger
}

// hopHeaders are connection-level headers that must not be copied across.
var hopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := h.Log.With("path", r.URL.Path, "remote", r.RemoteAddr)

	req, err := http.NewRequestWithContext(ctx, r.Method, "http://podman"+r.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Host = r.Host
	req.Header = r.Header.Clone()
	for _, k := range hopHeaders {
		req.Header.Del(k)
	}
	// The stream must arrive uncompressed so it can be parsed line by line.
	// The upstream transport also has DisableCompression set.
	req.Header.Del("Accept-Encoding")

	resp, err := h.Upstream.Do(req)
	if err != nil {
		log.Error("upstream request failed", "err", err)
		http.Error(w, `{"message":"upstream unavailable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		w.Header()[k] = vs
	}
	for _, k := range append(hopHeaders, "Content-Length") {
		w.Header().Del(k)
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(w, resp.Body)
		return
	}

	h.Stats.StreamsActive.Add(1)
	defer h.Stats.StreamsActive.Add(-1)
	log.Info("event stream opened")

	s := &stream{h: h, w: w, rc: rc, log: log, dedupe: NewDeduper(h.Repeat, h.Resync, time.Now)}
	err = s.run(resp.Body)
	switch {
	case ctx.Err() != nil:
		log.Info("event stream closed by client")
	case err != nil:
		log.Info("event stream closed", "err", err)
	default:
		log.Info("event stream closed by upstream")
	}
}

// stream is the per-connection state of one /events response.
type stream struct {
	h      *Handler
	w      io.Writer
	rc     *http.ResponseController
	log    *slog.Logger
	dedupe *Deduper
}

// run copies events from body to the client until either side ends.
// It returns nil when the upstream ends cleanly.
func (s *stream) run(body io.Reader) error {
	br := bufio.NewReader(body)
	var line []byte
	oversized := false // the current line exceeded MaxEventBytes and is streamed raw

	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case oversized:
			if werr := s.write(chunk); werr != nil {
				return werr
			}
		case len(line)+len(bytes.TrimSuffix(chunk, []byte{'\n'})) > s.h.MaxEventBytes:
			s.log.Debug("oversized event forwarded unparsed", "limit", s.h.MaxEventBytes)
			s.h.Stats.EventsIn.Add(1)
			s.h.Stats.EventsForwarded.Add(1)
			oversized = true
			if werr := s.write(line, chunk); werr != nil {
				return werr
			}
			line = line[:0]
		default:
			line = append(line, chunk...)
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !oversized && len(line) > 0 {
			if perr := s.process(line); perr != nil {
				return perr
			}
			line = line[:0]
		}
		if oversized {
			if !bytes.HasSuffix(chunk, []byte{'\n'}) {
				// Upstream ended mid-line; terminate it like any other event.
				if werr := s.write([]byte{'\n'}); werr != nil {
					return werr
				}
			}
			if ferr := s.rc.Flush(); ferr != nil {
				return ferr
			}
			oversized = false
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// process handles one complete line (with or without its trailing newline).
// Forwarded lines keep their original bytes; a missing final newline is added.
func (s *stream) process(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	s.h.Stats.EventsIn.Add(1)

	ev, err := Classify(trimmed)
	if err != nil {
		s.log.Debug("unparseable event forwarded", "err", err)
		return s.forward(line)
	}

	d := s.dedupe.Decide(ev)
	s.log.Debug("event", "forward", d.Forward, "reason", d.Reason,
		"id", ev.ContainerID, "name", ev.Name, "health", ev.Health)
	if !d.Forward {
		s.h.Stats.HealthDropped.Add(1)
		return nil
	}
	return s.forward(line)
}

func (s *stream) forward(line []byte) error {
	s.h.Stats.EventsForwarded.Add(1)
	err := s.write(line)
	if err == nil && !bytes.HasSuffix(line, []byte{'\n'}) {
		err = s.write([]byte{'\n'})
	}
	if err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *stream) write(parts ...[]byte) error {
	for _, p := range parts {
		if _, err := s.w.Write(p); err != nil {
			return err
		}
	}
	return nil
}
