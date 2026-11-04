package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/peers"
	"github.com/TwoD97/relay/internal/transport"
)

type coordinatorStandbyRequest struct {
	SourceID  string `json:"sourceId"`
	Harness   string `json:"harness"`
	AgentMode string `json:"agentMode"`
	Hosts     []Host `json:"hosts"`
}
type coordinatorStandby struct {
	Protocol int      `json:"protocol"`
	Cwd      string   `json:"cwd"`
	Blockers []string `json:"blockers"`
	Warnings []string `json:"warnings"`
}
type coordinatorStatus struct {
	Receipt    coordinatorReceipt `json:"receipt"`
	Mastermind MastermindState    `json:"mastermind"`
	Error      string             `json:"error,omitempty"`
}

// CoordinatorHandler is mounted ONLY on the user-private controller IPC socket.
// The desktop reaches it through its existing authenticated SSH connection.
// Browser credentials and arbitrary forwarding destinations are never accepted.
func (s *Server) CoordinatorHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /coordinator/standby", s.coordinatorStandby)
	mux.HandleFunc("POST /coordinator/seed", s.coordinatorSeed)
	mux.HandleFunc("POST /coordinator/activate", s.coordinatorActivate)
	mux.HandleFunc("GET /coordinator/status", s.coordinatorStatus)
	api := http.NewServeMux()
	api.HandleFunc("GET /api/mastermind", s.getMastermind)
	api.HandleFunc("POST /api/mastermind/config", s.configureMastermind)
	api.HandleFunc("POST /api/mastermind/start", s.startMastermind)
	api.HandleFunc("POST /api/mastermind/disable", s.disableMastermind)
	s.RegisterWorkspaceRoutes(api)
	s.RegisterControlRoutes(api)
	mux.Handle("/coordinator/api/", http.StripPrefix("/coordinator", api))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		for name := range r.Header {
			lower := strings.ToLower(name)
			if lower == "origin" || lower == "cookie" || lower == "authorization" || strings.HasPrefix(lower, "sec-fetch-") {
				writeError(w, 403, "Only the private coordinator IPC channel is accepted")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) coordinatorStandby(w http.ResponseWriter, r *http.Request) {
	var req coordinatorStandbyRequest
	if err := decodeJSON(w, r, &req); err != nil || !peers.ValidSource(req.SourceID) || (req.Harness != "claude" && req.Harness != "codex") || (req.AgentMode != "manual" && req.AgentMode != "auto") || len(req.Hosts) > 64 {
		writeError(w, 400, "Invalid destination preparation request")
		return
	}
	result := coordinatorStandby{Protocol: 1, Cwd: s.mastermindDirectory(), Blockers: []string{}, Warnings: []string{}}
	if runtime.GOOS != "linux" || s.config.LocalSocket == "" {
		result.Blockers = append(result.Blockers, "Hosted coordinators require a persistent Linux runtime")
	}
	s.coordinator.mu.Lock()
	receipt := s.coordinator.receipt
	receiptErr := s.coordinator.err
	s.coordinator.mu.Unlock()
	if receiptErr != "" {
		result.Blockers = append(result.Blockers, receiptErr)
	}
	if receipt.ID != "" {
		result.Blockers = append(result.Blockers, "This dedicated destination already has a handoff receipt; inspect it before another move")
	}
	current := s.mastermind.current()
	if current.Enabled || current.Session != nil {
		result.Blockers = append(result.Blockers, "This destination already has a coordinator session")
	}
	s.mu.Lock()
	l := s.links["local"]
	s.mu.Unlock()
	if l == nil || l.httpTransport == nil {
		result.Blockers = append(result.Blockers, "The destination local runtime is unavailable")
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		data, err := runtimeJSON(ctx, &http.Client{Transport: l.httpTransport}, "GET", "/api/harnesses", nil)
		var harnesses []struct {
			ID            string   `json:"id"`
			Installed     bool     `json:"installed"`
			Authenticated *bool    `json:"authenticated"`
			SessionModes  []string `json:"sessionModes"`
		}
		if err != nil || json.Unmarshal(data, &harnesses) != nil {
			result.Blockers = append(result.Blockers, "The destination harness sign-in could not be verified")
		} else {
			found := false
			for _, h := range harnesses {
				if h.ID != req.Harness {
					continue
				}
				found = true
				if !h.Installed {
					result.Blockers = append(result.Blockers, "Install the selected provider on the destination first")
				} else if h.Authenticated == nil || !*h.Authenticated {
					result.Blockers = append(result.Blockers, "Sign in to the selected provider on the destination first")
				}
			}
			if !found {
				result.Blockers = append(result.Blockers, "The selected provider is unavailable on the destination")
			}
		}
	}
	if len(result.Blockers) == 0 {
		executable, err := os.Executable()
		probe := mastermindRecord{MastermindState: MastermindState{Config: &MastermindConfig{HostID: "local", Harness: req.Harness, AgentMode: req.AgentMode}}, WorkerStateDir: filepath.Dir(filepath.Dir(s.config.LocalSocket))}
		if err == nil {
			_, err = runLocalMastermindWorker(r.Context(), executable, mastermindWorkerArgs(probe, "probe"))
		}
		if err != nil {
			result.Blockers = append(result.Blockers, "The destination provider does not support the requested permission mode; no coordinator was started")
		}
	}
	// Verify access using destination-owned SSH settings. No password, private key,
	// provider token or source-desktop credential is transferred.
	var wg sync.WaitGroup
	defer wg.Wait()
	var mu sync.Mutex
	slots := make(chan struct{}, 4)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, host := range req.Hosts {
		if host.ID == "" || host.ID == "local" || seen[host.ID] || transport.ValidateTarget(host.Target) != nil || transport.ValidatePort(host.Port) != nil {
			writeError(w, 400, "Invalid destination fleet mapping")
			return
		}
		seen[host.ID] = true
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			result.Blockers = append(result.Blockers, "Destination fleet access check timed out")
			wg.Wait()
			writeJSON(w, 200, result)
			return
		}
		wg.Add(1)
		go func(host Host) {
			defer wg.Done()
			defer func() { <-slots }()
			probe, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			args := []string{"-oBatchMode=yes", "-oStrictHostKeyChecking=yes", "-oConnectTimeout=6", "-oNumberOfPasswordPrompts=0", "-oForwardAgent=no", "-p", strconv.Itoa(host.Port), "--", host.Target, "true"}
			cmd := exec.CommandContext(probe, "ssh", args...)
			cmd.Stdin = nil
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			cmd.WaitDelay = time.Second
			if err := cmd.Run(); err != nil {
				mu.Lock()
				result.Blockers = append(result.Blockers, "Destination SSH access is not ready for "+host.Name+". Configure its own key and host trust first.")
				mu.Unlock()
			}
		}(host)
	}
	wg.Wait()
	writeJSON(w, 200, result)
}
func seedDigest(seed coordinatorSeed) string {
	data, _ := json.Marshal(seed)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validateCoordinatorSeed(seed coordinatorSeed) error {
	if seed.SourceRevision > maxMastermindRevision-3 || seed.Workspace.Revision > maxMastermindRevision-3 || seed.Control.Revision > maxMastermindRevision-3 || !validMastermindID(seed.ID) || !peers.ValidSource(seed.SourceID) || (seed.Harness != "claude" && seed.Harness != "codex") || (seed.AgentMode != "manual" && seed.AgentMode != "auto") || len(seed.Hosts) > 64 || seed.Workspace.FrozenBy != "" || seed.Control.FrozenBy != "" {
		return errors.New("invalid handoff identity")
	}
	if err := validateWorkspaceRecord(seed.Workspace); err != nil {
		return err
	}
	if err := validateControlRecord(seed.Control); err != nil {
		return err
	}
	if len(seed.Control.Messages) > 200 || len(seed.Control.Lessons) > 100 {
		return errors.New("handoff inbox is too large")
	}
	seen := map[string]bool{}
	for _, h := range seed.Hosts {
		if h.ID == "" || h.ID == "local" || seen[h.ID] || transport.ValidateTarget(h.Target) != nil || transport.ValidatePort(h.Port) != nil {
			return errors.New("invalid handoff fleet")
		}
		seen[h.ID] = true
	}
	for _, message := range seed.Control.Messages {
		if message.Status == "processing" || message.Key != "" {
			return errors.New("in-flight instructions cannot migrate")
		}
	}
	return nil
}
func (s *Server) coordinatorSeed(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 7<<20))
	var seed coordinatorSeed
	if err != nil || strictWorkspaceJSON(data, &seed) != nil || validateCoordinatorSeed(seed) != nil {
		writeError(w, 400, "Invalid bounded coordinator handoff")
		return
	}
	digest := seedDigest(seed)
	store := s.coordinator
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.err != "" {
		writeError(w, 503, store.err)
		return
	}
	if store.receipt.ID != "" {
		writeError(w, 409, "This destination already accepted a seed. Inspect its exact receipt; the write will not be repeated.")
		return
	}
	s.mastermind.mu.Lock()
	defer s.mastermind.mu.Unlock()
	if s.mastermind.record.Enabled || s.mastermind.record.Config != nil {
		writeError(w, 409, "The destination coordinator is already configured")
		return
	}
	receipt := coordinatorReceipt{SourceRevision: seed.SourceRevision, Format: 1, ID: seed.ID, SourceID: seed.SourceID, Digest: digest, Phase: "seeding", Harness: seed.Harness, AgentMode: seed.AgentMode}
	if err = store.saveLocked(receipt); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.workspace.mu.Lock()
	seed.Workspace.Revision += 2
	for i := range seed.Workspace.Pages {
		seed.Workspace.Pages[i].Revision = seed.Workspace.Revision
	}
	seed.Workspace.UpdatedAt = time.Now().UTC()
	err = s.workspace.saveLocked(seed.Workspace)
	s.workspace.mu.Unlock()
	if err == nil {
		s.control.mu.Lock()
		seed.Control.Revision += 2
		err = saveControlFile(s.control.path, seed.Control)
		if err == nil {
			s.control.record = seed.Control
		} else {
			s.control.err = errors.New("Handoff inbox write was not confirmed")
		}
		s.control.mu.Unlock()
	}
	if err == nil {
		s.store.mu.Lock()
		if len(s.store.hosts) != 0 {
			err = errors.New("Destination fleet is not empty")
		} else {
			for _, host := range seed.Hosts {
				host.Status = "disconnected"
				host.Stage = "Ready to connect"
				host.Error = ""
				host.RuntimeOperation = nil
				host.SetupWarning = ""
				s.store.hosts = append(s.store.hosts, host)
			}
			err = s.store.saveLocked()
		}
		s.store.mu.Unlock()
	}
	if err != nil {
		receipt.Phase = "uncertain"
		receipt.Error = "Destination seed was not fully confirmed. No coordinator was activated."
		_ = store.saveLocked(receipt)
		writeError(w, 500, receipt.Error)
		return
	}
	receipt.Phase = "ready"
	if err = store.saveLocked(receipt); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.ReconnectSaved()
	writeJSON(w, 201, receipt)
}
func (s *Server) coordinatorActivate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Digest string `json:"digest"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	store := s.coordinator
	store.mu.Lock()
	defer store.mu.Unlock()
	rec := store.receipt
	if store.err != "" || rec.ID != req.ID || rec.Digest != req.Digest || rec.Phase != "ready" {
		writeError(w, 409, "Destination activation needs the exact prepared receipt; it will not be repeated")
		return
	}
	s.mastermind.mu.Lock()
	defer s.mastermind.mu.Unlock()
	if s.mastermind.record.Config != nil || s.mastermind.record.Enabled {
		writeError(w, 409, "The destination coordinator was already configured")
		return
	}
	rec.Phase = "activating"
	if err := store.saveLocked(rec); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	next := mastermindRecord{Format: 1, Phase: "queued", HostTarget: s.localMastermindIdentity(), MastermindState: MastermindState{Revision: max(s.mastermind.record.Revision, rec.SourceRevision) + 2, Enabled: true, Config: &MastermindConfig{HostID: "local", Harness: rec.Harness, AgentMode: rec.AgentMode, Cwd: s.mastermindDirectory()}, Status: "waiting", Stage: "Starting the handed-off coordinator", UpdatedAt: time.Now().UTC()}}
	if err := s.mastermind.saveLocked(next); err != nil {
		writeError(w, 500, "Destination activation was not confirmed; no request will be retried")
		return
	}
	rec.Phase = "activated"
	if err := store.saveLocked(rec); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.wakeMastermind()
	writeJSON(w, 202, rec)
}
func (s *Server) coordinatorStatus(w http.ResponseWriter, r *http.Request) {
	s.coordinator.mu.Lock()
	receipt, problem := s.coordinator.receipt, s.coordinator.err
	s.coordinator.mu.Unlock()
	writeJSON(w, 200, coordinatorStatus{Receipt: receipt, Mastermind: s.mastermindState(), Error: problem})
}
