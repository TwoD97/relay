package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *Server) hostedClient(ctx context.Context) (*http.Client, *CoordinatorBinding, error) {
	if s.migration == nil {
		return nil, nil, errors.New("No hosted coordinator")
	}
	rec, problem := s.migration.snapshot()
	if problem != "" {
		return nil, rec.Binding, errors.New(problem)
	}
	if rec.Binding == nil {
		return nil, nil, errors.New("No hosted coordinator")
	}
	host, l, err := s.migrationHost(rec.Binding.HostID)
	if err != nil {
		return nil, rec.Binding, err
	}
	if host.Target != rec.Host.Target || host.Port != rec.Host.Port || !host.CreatedAt.Equal(rec.Host.CreatedAt) {
		return nil, rec.Binding, errors.New("Hosted coordinator machine identity changed")
	}
	client, err := s.coordinatorClient(ctx, host, l, rec.CoordinatorID, false)
	return client, rec.Binding, err
}
func mapCoordinatorHosts(value any, from, to string) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "hostId" || key == "controllerHostId" {
				if child == from {
					item[key] = to
				}
			} else {
				mapCoordinatorHosts(child, from, to)
			}
		}
	case []any:
		for _, child := range item {
			mapCoordinatorHosts(child, from, to)
		}
	}
}
func cloneHosted(state MastermindState) MastermindState {
	copy := cloneMastermind(mastermindRecord{MastermindState: state}).MastermindState
	if state.Local != nil {
		local := *state.Local
		copy.Local = &local
	}
	return copy
}
func (s *Server) rememberHosted(state MastermindState) {
	store := s.migration
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.record.Binding == nil {
		return
	}
	next := store.record
	prior := next.LastHosted
	copy := cloneHosted(state)
	next.LastHosted = &copy
	changed := prior == nil || state.Revision > prior.Revision || (state.Session != nil && (prior.Session == nil || state.Session.ID != prior.Session.ID || !state.Session.CreatedAt.Equal(prior.Session.CreatedAt)))
	if changed {
		_ = store.saveLocked(next)
	} else {
		store.record.LastHosted = &copy
	}
}
func (s *Server) hostedMastermindState() (MastermindState, bool) {
	if s.migration == nil {
		return MastermindState{}, false
	}
	rec, problem := s.migration.snapshot()
	if rec.Binding == nil {
		if problem != "" {
			state := s.withMastermindLocal(s.mastermind.current().MastermindState)
			state.Status = "uncertain"
			state.Stage = "Migration state needs recovery"
			state.Error = problem
			state.UpdatedAt = time.Now().UTC()
			return state, true
		}
		return MastermindState{}, false
	}
	state := MastermindState{Revision: rec.Source.Revision + 2, Enabled: true, Status: "waiting", Stage: "Reconnect the hosted coordinator machine", ControllerHostID: rec.Binding.HostID}
	if rec.Source.Config != nil {
		copy := *rec.Source.Config
		copy.HostID = rec.Binding.HostID
		state.Config = &copy
	}
	if rec.LastHosted != nil {
		state = cloneHosted(*rec.LastHosted)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	client, _, err := s.hostedClient(ctx)
	if err == nil {
		var data []byte
		data, err = peerRequest(ctx, client, "GET", "/coordinator/api/mastermind", nil, 64<<10)
		if err == nil {
			err = json.Unmarshal(data, &state)
		}
	}
	if err != nil {
		state.Status = "waiting"
		state.Stage = "Hosted coordinator unavailable; no local coordinator was started"
		state.Error = "Reconnect its machine to inspect the authoritative workspace."
	}
	state.ControllerHostID = rec.Binding.HostID
	state.UpdatedAt = time.Now().UTC()
	if state.Config != nil {
		state.Config.HostID = rec.Binding.HostID
	}
	if state.Local != nil {
		state.Local.HostID = rec.Binding.HostID
	}
	if err == nil {
		s.rememberHosted(state)
	}
	return state, true
}
func (s *Server) routeCoordinator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mastermind" && !strings.HasPrefix(r.URL.Path, "/api/mastermind/") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/mastermind/migration") {
			next.ServeHTTP(w, r)
			return
		}
		rec, problem := s.migration.snapshot()
		if problem != "" {
			writeError(w, 503, problem)
			return
		}
		if rec.Binding == nil {
			if rec.SourceFrozen && r.Method != "GET" {
				writeError(w, 409, "The source coordinator is paused for handoff. Review migration status; no local edit or launch was dispatched.")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if problem != "" {
			writeError(w, 503, problem)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		client, binding, err := s.hostedClient(ctx)
		if err != nil {
			writeError(w, 503, "Hosted coordinator is unavailable. Reconnect its machine; no local operation was substituted.")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			writeError(w, 413, "Hosted request exceeds its bound")
			return
		}
		if len(body) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.UseNumber()
			var value any
			if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
				writeError(w, 400, "Invalid hosted JSON request")
				return
			}
			if r.URL.Path == "/api/mastermind/config" {
				object, ok := value.(map[string]any)
				if !ok || object["hostId"] != binding.HostID {
					writeError(w, 409, "Hosted coordinator settings must keep their current machine. Use an explicit migration to move it.")
					return
				}
			}
			mapCoordinatorHosts(value, binding.HostID, "local")
			body, _ = json.Marshal(value)
		}
		request, err := http.NewRequestWithContext(ctx, r.Method, "http://coordinator/coordinator"+r.URL.EscapedPath(), bytes.NewReader(body))
		if err != nil {
			writeError(w, 400, "Invalid hosted route")
			return
		}
		request.URL.RawQuery = r.URL.RawQuery
		request.GetBody = nil
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			writeError(w, 502, "Hosted operation was not confirmed. Mutations are not replayed; inspect the destination before another action.")
			return
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, (7<<20)+1))
		if err != nil || len(data) > 7<<20 {
			writeError(w, 502, "Hosted response could not be verified; inspect its current state before repeating an action")
			return
		}
		if len(data) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			var value any
			if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
				writeError(w, 502, "Hosted response was not bounded JSON")
				return
			}
			mapCoordinatorHosts(value, "local", binding.HostID)
			if r.URL.Path == "/api/mastermind" || r.URL.Path == "/api/mastermind/config" || r.URL.Path == "/api/mastermind/start" || r.URL.Path == "/api/mastermind/disable" {
				if object, ok := value.(map[string]any); ok {
					object["controllerHostId"] = binding.HostID
					object["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
					if response.StatusCode >= 200 && response.StatusCode < 300 {
						encoded, _ := json.Marshal(object)
						var state MastermindState
						if json.Unmarshal(encoded, &state) == nil && state.Config != nil {
							s.rememberHosted(state)
						}
					}
				}
			}
			data, _ = json.Marshal(value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
	})
}
