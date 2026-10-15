//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Auto is Relay's explicit full-permission launch mode, not the providers'
// similarly named classifier/workspace-only modes. This text supplements the
// provider's normal instructions; it never replaces project or user config.
const autoAgentInstruction = `Relay session mode: Auto, explicitly selected by the user for this new session.
The user authorizes you to complete their requested task end-to-end using the tools and full permissions available in this session, without pausing for routine tool approvals. Stay within the user's task and follow applicable project instructions and organization policy. Do not invent a task, expand its scope, or treat tool access as authorization for unrelated work. If no task has been provided, wait for the user's task.
When the user's task refers to a named Relay machine, discover the available machines with: relay_cli="${RELAY_BIN:-$HOME/.local/share/relay/bin/relay}"; "$relay_cli" fleet list
Use the returned machine names with Relay session commands, for example: "$relay_cli" session list --host NAME. Resolve names through Relay; do not guess SSH destinations or copy credentials between machines. New delegated sessions remain Manual unless the user requests autonomous delegation; only then pass --mode auto when starting that delegated agent session.`

func normalizedAgentMode(harness, mode string) (string, error) {
	if mode != "" && mode != "manual" && mode != "auto" {
		return "", errors.New("agentMode must be manual or auto")
	}
	if harness == "shell" {
		if mode == "auto" {
			return "", errors.New("Auto mode is available only for Claude Code or Codex sessions")
		}
		return "", nil
	}
	if mode == "" {
		return "manual", nil
	}
	return mode, nil
}

func agentModeArguments(harness, mode string, base []string) []string {
	args := append([]string(nil), base...)
	if mode != "auto" {
		return args
	}
	if harness == "claude" {
		return append(args, "--dangerously-skip-permissions", "--append-system-prompt", autoAgentInstruction)
	}
	// A literal initial user prompt preserves Codex's configured developer
	// instructions and normal AGENTS.md discovery. No shell evaluates the text.
	return append(args, "--dangerously-bypass-approvals-and-sandbox", "--", autoAgentInstruction)
}

func (s *Server) validateAutoAgent(ctx context.Context, harness, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--help")
	cmd.Dir, cmd.Env = s.home, s.environment()
	output := &boundedOutput{limit: 64 << 10}
	stderr := &boundedOutput{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := runIsolatedCommand(ctx, cmd); err != nil || output.buffer.Len() >= output.limit || stderr.buffer.Len() >= stderr.limit {
		return errors.New("Could not verify this CLI's Auto mode flags. Update or repair the harness, then try again; no session was started")
	}
	help := output.buffer.String()
	flags := []string{"--dangerously-bypass-approvals-and-sandbox"}
	if harness == "claude" {
		flags = []string{"--dangerously-skip-permissions", "--append-system-prompt"}
	}
	for _, flag := range flags {
		if !observerHelpFlag([]byte(help), flag) {
			return fmt.Errorf("This %s CLI does not support Relay Auto mode. Update or repair it and start a new session; no session was started", harnessNames[harness])
		}
	}
	if harness == "codex" && !strings.Contains(help, "[PROMPT]") {
		return errors.New("This Codex CLI does not advertise initial prompt support required for Auto mode. Update or repair it; no session was started")
	}
	return nil
}
