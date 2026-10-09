//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var providerVersion = regexp.MustCompile(`(?:^|[^0-9])(\d+)\.(\d+)\.(\d+)(?:[^0-9]|$)`)

// These are conservative, verified baselines, not claims about when the feature
// was introduced. Older CLIs remain fully usable through the native terminal.
func verifiedPermissionVersion(id, output string) bool {
	match := providerVersion.FindStringSubmatch(output)
	if len(match) != 4 {
		return false
	}
	minimum := [3]int{2, 1, 209}
	if id == "codex" {
		minimum = [3]int{0, 153, 4}
	} else if id != "claude" {
		return false
	}
	for i := 0; i < 3; i++ {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return false
		}
		if value != minimum[i] {
			return value > minimum[i]
		}
	}
	return true
}
func (s *Server) permissionCapability(ctx context.Context, id, path string) *Permissions {
	terminal := &Permissions{Support: "terminal-only", Detail: "This CLI has not passed Relay's permission-hook compatibility checks. Use its terminal prompts; update the CLI and start a new session to enable approval integration."}
	version, err := s.probe(ctx, path, "--version")
	if err != nil || !verifiedPermissionVersion(id, version) {
		return terminal
	}
	if id == "codex" {
		features, err := s.probe(ctx, path, "features", "list")
		enabled := false
		for _, line := range strings.Split(features, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "hooks" && fields[2] == "true" {
				enabled = true
			}
		}
		if err != nil || !enabled {
			terminal.Detail = "Codex hooks are disabled or unavailable. Use terminal prompts; Relay does not override your hook or permission policy."
			return terminal
		}
		return &Permissions{Support: "configured", Detail: "Review and trust Relay's hook definitions in Codex /hooks. Until an authenticated hook connects, or if policy disables these hooks, approvals remain in the terminal."}
	}
	return &Permissions{Support: "configured", Detail: "Permission hooks are configured for this session. Until a hook connects, use the terminal. Provider policy and unsupported prompts still apply."}
}

func permissionHarnessArguments(id, executable string, enabled bool) []string {
	args := harnessArguments(id, executable)
	if !enabled {
		return args
	}
	hookCommand := func(event string) string {
		return shellQuote(executable) + " hook --provider " + id + " --event " + event
	}
	if id == "codex" {
		// -c accepts TOML. Inline arrays add this invocation's hooks; Codex merges
		// hook definitions across configuration layers and applies its trust checks.
		for _, event := range []string{"PermissionRequest", "SessionStart", "UserPromptSubmit"} {
			kind, timeout := "observe", 3
			if event == "PermissionRequest" {
				kind, timeout = "permission", 620
			}
			command, _ := json.Marshal(hookCommand(kind))
			definition := fmt.Sprintf("hooks.%s=[{hooks=[{type=\"command\",command=%s,timeout=%d}]}]", event, command, timeout)
			args = append(args, "-c", definition)
		}
		return args
	}
	if id == "claude" {
		var settings map[string]any
		_ = json.Unmarshal([]byte(args[1]), &settings)
		hooks := settings["hooks"].(map[string]any)
		entry := func(kind string, timeout int) any {
			return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCommand(kind), "timeout": timeout}}}
		}
		hooks["PermissionRequest"] = []any{entry("permission", 620)}
		hooks["SessionStart"] = []any{entry("observe", 3)}
		hooks["UserPromptSubmit"] = append(hooks["UserPromptSubmit"].([]any), entry("observe", 3))
		data, _ := json.Marshal(settings)
		return []string{"--settings", string(data)}
	}
	return args
}
