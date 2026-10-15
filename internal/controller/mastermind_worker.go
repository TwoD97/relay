package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TwoD97/relay/internal/transport"
)

const mastermindWorkerOutputLimit = 16 << 10

type mastermindWorkerReceipt struct {
	LaunchID         string    `json:"launchId"`
	SessionID        string    `json:"sessionId"`
	SessionCreatedAt time.Time `json:"sessionCreatedAt"`
	Cwd              string    `json:"cwd"`
	Harness          string    `json:"harness"`
	AgentMode        string    `json:"agentMode"`
}

// Every argument is fixed, enum-validated, or obtained from the trusted worker.
// Quotes preserve spaces and punctuation; control characters are rejected before
// a path is allowed into a PTY command.
func mastermindShellArguments(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}

func mastermindWorkerArgs(rec mastermindRecord, operation string) []string {
	args := []string{"mastermind-worker", operation}
	if operation == "probe" || operation == "run" {
		args = append(args, "--harness", rec.Config.Harness, "--mode", rec.Config.AgentMode)
	}
	if operation == "status" || operation == "run" {
		args = append(args, "--launch-id", rec.Token)
	}
	if operation == "run" {
		args = append(args, "--session-id", rec.Session.ID, "--session-created-at", rec.Session.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	if rec.WorkerStateDir != "" {
		args = append(args, "--state-dir", rec.WorkerStateDir)
	}
	return args
}

func (s *Server) prepareMastermindWorker(ctx context.Context, rec mastermindRecord, connection *transport.Connection) (string, string, error) {
	s.mu.Lock()
	l := s.links[rec.Config.HostID]
	if l == nil || l.conn != connection || l.repairing || s.ctx.Err() != nil {
		s.mu.Unlock()
		return "", "", errors.New("The machine connection changed or runtime files are already being prepared. Check setup again when it is ready.")
	}
	select {
	case s.repairSlots <- struct{}{}:
	default:
		s.mu.Unlock()
		return "", "", errors.New("Other runtime files are being prepared. Check setup again shortly.")
	}
	l.repairing = true
	s.mu.Unlock()
	defer func() { <-s.repairSlots; s.mu.Lock(); l.repairing = false; s.mu.Unlock() }()

	var output []byte
	var err error
	if rec.Config.HostID == "local" {
		rec.WorkerStateDir = filepath.Dir(filepath.Dir(s.config.LocalSocket))
		if !validMastermindWorkerPath(rec.WorkerStateDir) {
			return "", "", errors.New("The local runtime directory cannot be used safely by the Mastermind worker.")
		}
		executable, exeErr := os.Executable()
		if exeErr != nil {
			return "", "", errors.New("Could not locate the installed Relay CLI.")
		}
		output, err = runLocalMastermindWorker(ctx, executable, mastermindWorkerArgs(rec, "probe"))
	} else if connection != nil {
		_, err = connection.Repair(ctx, s.config.BinaryDir, s.config.Version, func(stage string) {
			s.mastermind.notice(rec.Revision, "waiting", "Preparing a compatible Mastermind worker: "+stage, "Existing terminal sessions remain running.")
		})
		if err == nil {
			output, err = connection.Run(ctx, `exec "$HOME/.local/share/relay/bin/relay" `+mastermindShellArguments(mastermindWorkerArgs(rec, "probe")), nil)
		}
	} else {
		err = errors.New("SSH connection is unavailable")
	}
	if err != nil {
		return "", "", errors.New("Could not prepare the Mastermind worker. Check the machine connection, update Relay, and verify the selected agent supports this mode; existing sessions were preserved.")
	}
	s.mu.Lock()
	current := s.links[rec.Config.HostID] == l && s.ctx.Err() == nil
	s.mu.Unlock()
	if !current {
		return "", "", errors.New("The machine connection changed while preparing Mastermind. Reconnect and check setup; no agent was started.")
	}
	var probe struct {
		Protocol           int    `json:"protocol"`
		MastermindWorkerV1 bool   `json:"mastermindWorkerV1"`
		Executable         string `json:"executable"`
	}
	if len(output) > mastermindWorkerOutputLimit || json.Unmarshal(output, &probe) != nil || probe.Protocol != 1 || !probe.MastermindWorkerV1 || !validMastermindWorkerPath(probe.Executable) {
		return "", "", errors.New("The installed Relay CLI does not support the Mastermind worker. Update Relay and check setup again; existing sessions were preserved.")
	}
	return probe.Executable, rec.WorkerStateDir, nil
}

func readMastermindWorkerStatus(ctx context.Context, rec mastermindRecord, connection *transport.Connection) ([]byte, error) {
	if !validMastermindWorkerPath(rec.WorkerPath) || !validMastermindID(rec.Token) {
		return nil, errors.New("invalid worker identity")
	}
	args := mastermindWorkerArgs(rec, "status")
	if rec.Config.HostID == "local" {
		return runLocalMastermindWorker(ctx, rec.WorkerPath, args)
	}
	if connection == nil {
		return nil, errors.New("SSH connection is unavailable")
	}
	return connection.Run(ctx, "exec "+mastermindShellArguments(append([]string{rec.WorkerPath}, args...)), nil)
}

func verifyMastermindWorkerReceipt(rec mastermindRecord, data []byte) error {
	var response struct {
		Protocol int                      `json:"protocol"`
		Found    bool                     `json:"found"`
		Launch   *mastermindWorkerReceipt `json:"launch"`
	}
	if len(data) > mastermindWorkerOutputLimit || json.Unmarshal(data, &response) != nil || response.Protocol != 1 {
		return errors.New("The Mastermind worker returned an invalid launch receipt. Nothing will be replayed.")
	}
	if !response.Found || response.Launch == nil {
		return errors.New("The Mastermind worker launch has not been confirmed. Inspect the machine's sessions; no command will be repeated.")
	}
	l := response.Launch
	if rec.Session == nil || l.LaunchID != rec.Token || l.SessionID != rec.Session.ID || !l.SessionCreatedAt.Equal(rec.Session.CreatedAt) || l.Cwd != rec.CanonicalCwd || l.Harness != rec.Config.Harness || l.AgentMode != rec.Config.AgentMode {
		return errors.New("The Mastermind worker receipt does not match this session and configuration. Existing sessions were preserved; nothing will be replayed.")
	}
	return nil
}
