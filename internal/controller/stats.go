package controller

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/TwoD97/relay/internal/machinestats"
	"github.com/TwoD97/relay/internal/transport"
)

type statsEntry struct {
	link  *link
	cache machinestats.Cache
}

func collectMachineStats(ctx context.Context, connection *transport.Connection) (machinestats.Sample, error) {
	if connection == nil {
		return machinestats.Collect(ctx)
	}
	data, err := connection.Run(ctx, machinestats.LinuxScript, nil)
	if err != nil {
		return machinestats.Sample{}, err
	}
	return machinestats.ParseLinux(data)
}

func (s *Server) machineStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.RawQuery != "" {
		writeError(w, 400, "Machine statistics do not accept query parameters.")
		return
	}
	id := r.PathValue("id")
	if id == "local" {
		if !s.hasLocalMastermind() {
			writeError(w, 404, "Machine not found.")
			return
		}
	} else if _, ok := s.store.get(id); !ok {
		writeError(w, 404, "Machine not found.")
		return
	}
	s.mu.Lock()
	l := s.links[id]
	var connection *transport.Connection
	if l != nil {
		connection = l.conn
	}
	ready := id == "local" || connection != nil && connection.Ready()
	if !ready {
		s.mu.Unlock()
		writeError(w, 503, "Machine is disconnected. Reconnect to read its statistics.")
		return
	}
	entry := s.stats[id]
	if entry == nil || entry.link != l {
		entry = &statsEntry{link: l}
		s.stats[id] = entry
	}
	collector := s.statsCollector
	timeout := s.statsTimeout
	s.mu.Unlock()
	result, err := entry.cache.Read(r.Context(), s.ctx, 2*time.Second, timeout, func(ctx context.Context) (machinestats.Sample, error) {
		select {
		case s.statsSlots <- struct{}{}:
			defer func() { <-s.statsSlots }()
		case <-ctx.Done():
			return machinestats.Sample{}, ctx.Err()
		}
		return collector(ctx, connection)
	})
	s.mu.Lock()
	current := s.links[id] == l
	s.mu.Unlock()
	if !current {
		writeError(w, 503, "Machine connection changed. Read statistics after reconnecting.")
		return
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			writeError(w, 504, "Machine statistics timed out or were cancelled.")
		} else {
			writeError(w, 502, "Could not read machine statistics from this connection.")
		}
		return
	}
	writeJSON(w, 200, result)
}
