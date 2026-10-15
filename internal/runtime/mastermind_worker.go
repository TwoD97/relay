//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const mastermindWorkerLimit = 16 << 10

type MastermindWorkerCapability struct {
	Protocol           int    `json:"protocol"`
	MastermindWorkerV1 bool   `json:"mastermindWorkerV1"`
	Executable         string `json:"executable"`
}

// A receipt records one attempted provider exec in one exact shell PTY. It is
// not a transcript, runtime session record, provider credential or task prompt.
type MastermindWorkerLaunch struct {
	Format           int       `json:"format"`
	LaunchID         string    `json:"launchId"`
	SessionID        string    `json:"sessionId"`
	SessionCreatedAt time.Time `json:"sessionCreatedAt"`
	Cwd              string    `json:"cwd"`
	Harness          string    `json:"harness"`
	AgentMode        string    `json:"agentMode"`
}

type MastermindWorkerStatus struct {
	Protocol int                     `json:"protocol"`
	Found    bool                    `json:"found"`
	Launch   *MastermindWorkerLaunch `json:"launch,omitempty"`
}

func mastermindWorkerServer(stateDir, harness, mode string) (*Server, string, error) {
	if (harness != "claude" && harness != "codex") || (mode != "manual" && mode != "auto") {
		return nil, "", errors.New("Mastermind requires claude or codex and explicit manual or auto mode")
	}
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	// Deliberately no New(), load(), daemon lock, session recovery or writes.
	s := &Server{stateDir: dir, home: home}
	provider, err := s.findCommand(harness)
	return s, provider, err
}

func validateMastermindWorker(ctx context.Context, s *Server, provider, harness, mode string) error {
	if err := s.validateMastermindAgent(ctx, harness, provider); err != nil {
		return err
	}
	if mode == "auto" {
		return s.validateAutoAgent(ctx, harness, provider)
	}
	return ctx.Err()
}

func ProbeMastermindWorker(ctx context.Context, stateDir, harness, mode string) (MastermindWorkerCapability, error) {
	s, provider, err := mastermindWorkerServer(stateDir, harness, mode)
	if err != nil {
		return MastermindWorkerCapability{}, err
	}
	if err := validateMastermindWorker(ctx, s, provider, harness, mode); err != nil {
		return MastermindWorkerCapability{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return MastermindWorkerCapability{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(executable) || len(executable) > 4096 {
		return MastermindWorkerCapability{}, errors.New("Could not verify the Mastermind worker executable")
	}
	return MastermindWorkerCapability{Protocol: 1, MastermindWorkerV1: true, Executable: executable}, nil
}

func validMastermindWorkerLaunch(launch MastermindWorkerLaunch) bool {
	return launch.Format == 1 && validMastermindID(launch.LaunchID) && validMastermindID(launch.SessionID) &&
		!launch.SessionCreatedAt.IsZero() && filepath.IsAbs(launch.Cwd) && len(launch.Cwd) <= 4096 &&
		!strings.ContainsAny(launch.Cwd, "\x00\r\n") && (launch.Harness == "claude" || launch.Harness == "codex") &&
		(launch.AgentMode == "manual" || launch.AgentMode == "auto")
}

func mastermindWorkerDirectory(stateDir string) (string, error) {
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mastermind-workers"), nil
}

func ReadMastermindWorker(stateDir, id string) (MastermindWorkerStatus, error) {
	status := MastermindWorkerStatus{Protocol: 1}
	if !validMastermindID(id) {
		return status, errors.New("Invalid Mastermind launch identity")
	}
	dir, err := mastermindWorkerDirectory(stateDir)
	if err != nil {
		return status, err
	}
	for _, parent := range []string{filepath.Dir(dir), dir} {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			return status, nil
		}
		if err != nil || !info.IsDir() {
			return status, errors.New("Could not verify the Mastermind receipt directory")
		}
	}
	fd, err := syscall.Open(filepath.Join(dir, id+".json"), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return status, errors.New("Could not read the Mastermind launch receipt")
	}
	f := os.NewFile(uintptr(fd), "mastermind-receipt")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > mastermindWorkerLimit {
		return status, errors.New("Invalid Mastermind launch receipt")
	}
	data, err := io.ReadAll(io.LimitReader(f, mastermindWorkerLimit+1))
	var launch MastermindWorkerLaunch
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err != nil || len(data) > mastermindWorkerLimit || decoder.Decode(&launch) != nil || decoder.Decode(new(any)) != io.EOF || !validMastermindWorkerLaunch(launch) || launch.LaunchID != id {
		return status, errors.New("Invalid Mastermind launch receipt; no startup will be repeated")
	}
	status.Found, status.Launch = true, &launch
	return status, nil
}

func claimMastermindWorker(stateDir string, launch MastermindWorkerLaunch) error {
	if !validMastermindWorkerLaunch(launch) {
		return errors.New("Invalid Mastermind launch identity")
	}
	dir, err := mastermindWorkerDirectory(stateDir)
	if err != nil {
		return err
	}
	for _, parent := range []string{filepath.Dir(dir), dir} {
		if err := privateDir(parent); err != nil {
			return err
		}
	}
	data, err := json.Marshal(launch)
	if err != nil || len(data) > mastermindWorkerLimit {
		return errors.New("Mastermind launch receipt exceeds its limit")
	}
	// O_EXCL is the durable one-launch boundary. A failed or partial write keeps
	// its marker so later attempts cannot replay work after uncertain storage.
	f, err := os.OpenFile(filepath.Join(dir, launch.LaunchID+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("Mastermind launch was already claimed or its receipt cannot be saved")
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return errors.New("Could not confirm the Mastermind launch receipt; no provider was started")
	}
	folder, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = folder.Sync()
	closeErr = folder.Close()
	return errors.Join(err, closeErr)
}

func verifyMastermindWorkerSession(ctx context.Context, stateDir string, launch MastermindWorkerLaunch) error {
	socket := filepath.Join(stateDir, "run", "daemon.sock")
	if os.Getenv("RELAY_SESSION_ID") != launch.SessionID || os.Getenv("RELAY_SOCKET") != socket {
		return errors.New("Mastermind must start inside its exact Relay shell session")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://relay/api/sessions", nil)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Could not verify the owning Relay session")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (512<<10)+1))
	var sessions []Session
	if response.StatusCode != http.StatusOK || err != nil || len(data) > 512<<10 || json.Unmarshal(data, &sessions) != nil {
		return errors.New("Could not verify the owning Relay session")
	}
	matches := 0
	for _, session := range sessions {
		if session.ID != launch.SessionID {
			continue
		}
		if session.Harness != "shell" || session.Purpose != "" || session.Status != "running" || session.AgentMode != "" ||
			!session.CreatedAt.Equal(launch.SessionCreatedAt) || session.Cwd != launch.Cwd {
			return errors.New("The owning Relay session identity changed; no provider was started")
		}
		matches++
	}
	if matches != 1 {
		return errors.New("The owning Relay session is missing or ambiguous")
	}
	return ctx.Err()
}

// RunMastermindWorker replaces its shell with the provider, retaining that
// shell's PID, PTY, process ownership, environment and existing provider login.
// Approval hooks cannot be attached to a legacy shell record, so prompts stay
// in the terminal. No provider/project policy or runtime metadata is rewritten.
func RunMastermindWorker(ctx context.Context, stateDir string, launch MastermindWorkerLaunch, out io.Writer) error {
	s, provider, err := mastermindWorkerServer(stateDir, launch.Harness, launch.AgentMode)
	if err != nil {
		return err
	}
	launch.Format = 1
	launch.Cwd, err = os.Getwd()
	if err != nil || !validMastermindWorkerLaunch(launch) {
		return errors.New("Invalid Mastermind worker launch")
	}
	if err := verifyMastermindWorkerSession(ctx, s.stateDir, launch); err != nil {
		return err
	}
	if err := validateMastermindWorker(ctx, s, provider, launch.Harness, launch.AgentMode); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := claimMastermindWorker(s.stateDir, launch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	env := setEnv(s.environment(), "RELAY_AGENT_MODE", launch.AgentMode)
	args := append([]string{provider}, sessionAgentArguments(launch.Harness, launch.AgentMode, true, nil)...)
	if out != nil {
		fmt.Fprintln(out, "Relay Mastermind: your coordinator is starting. Existing sessions stay running. Permission requests appear in this terminal.")
		if launch.AgentMode == "auto" {
			fmt.Fprintln(out, "Relay Auto: requesting full provider permissions. Provider and organization restrictions still apply.")
		}
	}
	return syscall.Exec(provider, args, env)
}
