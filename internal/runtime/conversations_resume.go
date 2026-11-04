//go:build linux

package runtime

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/TwoD97/relay/internal/conversations"
)

func (s *Server) prepareConversationResume(ctx context.Context, harness, path, cwd, id string) error {
	if !conversations.ValidID(id) || (harness != "claude" && harness != "codex") {
		return errors.New("Resume requires a Claude Code or Codex conversation UUID")
	}
	if _, err := s.conversationLookup(ctx, harness, cwd, id); err != nil {
		return err
	}
	args := []string{"--help"}
	if harness == "codex" {
		args = []string{"resume", "--help"}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env, cmd.Dir = s.environment(), cwd
	out := &boundedOutput{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = &boundedOutput{limit: 8 << 10}
	if err := runIsolatedCommand(ctx, cmd); err != nil || out.buffer.Len() >= out.limit {
		return errors.New("Could not verify this CLI's resume support. Update or repair the harness; no session was started")
	}
	help := out.buffer.String()
	if (harness == "claude" && !observerHelpFlag([]byte(help), "--resume")) || (harness == "codex" && !strings.Contains(help, "SESSION_ID")) {
		return errors.New("This CLI does not advertise UUID resume support. Update or repair it; no session was started")
	}
	return nil
}

func conversationResumeArguments(harness, id string, args []string) []string {
	if id == "" {
		return args
	}
	if harness == "claude" {
		return append([]string{"--resume", id}, args...)
	}
	return append([]string{"resume", id}, args...)
}
