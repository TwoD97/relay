package controller

import (
	"errors"
	"os"
	"path/filepath"

	agentskill "github.com/TwoD97/relay/skills"
)

const localMastermindInstruction = agentskill.MastermindInstruction

func (s *Server) hasLocalMastermind() bool {
	return s.config.LocalSocket != "" || s.config.LocalAgentDir != ""
}
func (s *Server) localMastermindIdentity() string {
	if s.config.LocalAgentDir != "" {
		return s.config.LocalAgentDir
	}
	return s.config.LocalSocket
}
func (s *Server) mastermindDirectory() string {
	return filepath.Join(s.config.StateDir, "mastermind-project")
}
func (s *Server) initializeMastermindProject() error {
	for _, name := range []string{"mastermind-project", "mastermind-project/.agents", "mastermind-project/.agents/skills", "mastermind-project/.agents/skills/relay-mastermind", "mastermind-project/.claude", "mastermind-project/.claude/skills", "mastermind-project/.claude/skills/relay-mastermind"} {
		dir := filepath.Join(s.config.StateDir, filepath.FromSlash(name))
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Mastermind project directories must not be links")
		}
	}
	for _, parent := range []string{".agents", ".claude"} {
		file, err := os.OpenFile(filepath.Join(s.mastermindDirectory(), parent, "skills", "relay-mastermind", "SKILL.md"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString(agentskill.MastermindContent)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
func (s *Server) mastermindState() MastermindState {
	if state, hosted := s.hostedMastermindState(); hosted {
		return state
	}
	return s.withMastermindLocal(s.mastermind.current().MastermindState)
}
func (s *Server) withMastermindLocal(state MastermindState) MastermindState {
	if s.hasLocalMastermind() {
		state.Local = &MastermindLocal{HostID: "local", Cwd: s.mastermindDirectory()}
	}
	return state
}

// Only the app-owned working folder receives generated project guidance.
// Selecting a user's existing project never overwrites that project's skills.
func (s *Server) prepareLocalMastermind(rec mastermindRecord) (map[string]string, error) {
	capability, err := s.PrepareWorkspaceAgentCapability(rec.Token)
	if err != nil {
		return nil, errors.New("Could not prepare the local Mastermind connection; no agent was started")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := s.initializeMastermindProject(); err != nil {
		return nil, err
	}
	return map[string]string{"RELAY_CONTROLLER_STATE": s.config.StateDir, "RELAY_AGENT_CAPABILITY_FILE": capability, "RELAY_BIN": executable}, nil
}
