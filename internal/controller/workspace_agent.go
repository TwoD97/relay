package controller

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type workspaceAgentCapability struct {
	Format   int    `json:"format"`
	LaunchID string `json:"launchId"`
	Token    string `json:"token"`
}

func workspaceCapabilityPath(stateDir, launchID string) string {
	return filepath.Join(stateDir, "agent-capabilities", launchID+".json")
}

func validWorkspaceCapability(value workspaceAgentCapability, launchID string) bool {
	decoded, err := hex.DecodeString(value.Token)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value.Token && value.Format == 1 && value.LaunchID == launchID && validMastermindID(launchID)
}

func readWorkspaceCapability(stateDir, launchID string) (workspaceAgentCapability, error) {
	var value workspaceAgentCapability
	info, err := os.Lstat(filepath.Join(stateDir, "agent-capabilities"))
	if err != nil || !info.IsDir() {
		return value, errors.New("Agent capability directory is unavailable")
	}
	data, err := readWorkspaceFile(workspaceCapabilityPath(stateDir, launchID), 4096)
	if err != nil || strictWorkspaceJSON(data, &value) != nil || !validWorkspaceCapability(value, launchID) {
		return value, errors.New("Agent capability is unavailable or invalid")
	}
	return value, nil
}

// PrepareWorkspaceAgentCapability returns a private, per-launch credential file.
// Its contents must never enter session JSON, fleet snapshots, URLs or prompts.
// The credential survives controller handover, while authorization below also
// requires that this launch is still the enabled Mastermind configuration.
func (s *Server) PrepareWorkspaceAgentCapability(launchID string) (string, error) {
	if !validMastermindID(launchID) || s.workspace == nil {
		return "", errors.New("Invalid coordinator launch identity")
	}
	s.workspace.capabilityMu.Lock()
	defer s.workspace.capabilityMu.Unlock()
	dir := filepath.Join(s.config.StateDir, "agent-capabilities")
	if err := makeWorkspaceCapabilityDirectory(dir); err != nil && !errors.Is(err, os.ErrExist) {
		return "", errors.New("Could not create the private agent capability directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("Agent capability directory must not be a link")
	}
	folder, err := os.Open(dir)
	if err != nil {
		return "", errors.New("Could not open the private agent capability directory")
	}
	opened, statErr := folder.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		folder.Close()
		return "", errors.New("Agent capability directory identity changed")
	}
	chmodErr := folder.Chmod(0700)
	closeErr := folder.Close()
	if errors.Join(chmodErr, closeErr) != nil {
		return "", errors.New("Could not protect the agent capability directory")
	}
	path := workspaceCapabilityPath(s.config.StateDir, launchID)
	if _, err := os.Lstat(path); err == nil {
		if _, err := readWorkspaceCapability(s.config.StateDir, launchID); err != nil {
			return "", err
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("Could not inspect the agent capability")
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", errors.New("Could not create an agent capability")
	}
	data, err := json.Marshal(workspaceAgentCapability{Format: 1, LaunchID: launchID, Token: hex.EncodeToString(secret[:])})
	if err != nil {
		return "", err
	}
	f, err := createWorkspaceCapability(path)
	if err != nil {
		return "", errors.New("Could not exclusively create the agent capability")
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = syncStoreDirectory(dir)
	}
	if err != nil {
		return "", errors.New("Agent capability save could not be confirmed; existing bytes were preserved")
	}
	return path, nil
}

// WorkspaceAgentAuthorization is for the private controller IPC listener only.
// It grants no browser login, native window action, harness installation, or host
// lifecycle permission. The caller must mount only its explicit agent allowlist.
func (s *Server) WorkspaceAgentAuthorization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !strings.HasPrefix(r.URL.Path, "/agent/") {
			writeError(w, 404, "Unknown agent endpoint")
			return
		}
		for name := range r.Header {
			lower := strings.ToLower(name)
			if lower == "origin" || lower == "cookie" || strings.HasPrefix(lower, "sec-fetch-") {
				writeError(w, 403, "Browser credentials are not accepted by the agent interface")
				return
			}
		}
		authorization := r.Header.Values("Authorization")
		if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") || len(authorization[0]) != len("Bearer ")+64 || s.mastermind == nil {
			writeError(w, 401, "An active Mastermind capability is required")
			return
		}
		record := s.mastermind.current()
		if !record.Enabled || !validMastermindID(record.Token) {
			writeError(w, 401, "An active Mastermind capability is required")
			return
		}
		capability, err := readWorkspaceCapability(s.config.StateDir, record.Token)
		if err != nil || subtle.ConstantTimeCompare([]byte(capability.Token), []byte(strings.TrimPrefix(authorization[0], "Bearer "))) != 1 {
			writeError(w, 401, "An active Mastermind capability is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
