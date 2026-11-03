//go:build linux

package runtime

import (
	"bytes"
	"encoding/json"
	agentskill "github.com/TwoD97/relay/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalMastermindGetsWorkspaceRoleAndPrivateEnvironmentOnlyAtLaunch(t *testing.T) {
	s := testServer(t)
	capture := filepath.Join(s.stateDir, "environment")
	_, argv := modeStub(t, s, "codex", "[PROMPT] --dangerously-bypass-approvals-and-sandbox", `printf '%s\0' "$RELAY_CONTROLLER_STATE" "$RELAY_AGENT_CAPABILITY_FILE" "$RELAY_BIN" > `+shellQuote(capture)+"\nexit 0")
	dir := t.TempDir()
	environment := map[string]string{"RELAY_CONTROLLER_STATE": dir, "RELAY_AGENT_CAPABILITY_FILE": filepath.Join(dir, "agent-capabilities", testMastermindID+".json"), "RELAY_BIN": "/tmp/relay fixture/bin"}
	meta := modeSession(t, s, modeRequest(t, s, map[string]any{"harness": "codex", "cwd": s.home, "agentMode": "manual", "mastermindId": testMastermindID, "mastermindEnvironment": environment})).snapshot()
	args := capturedArguments(t, argv)
	if !strings.Contains(args[len(args)-1], agentskill.MastermindInstruction) {
		t.Fatal("local workspace instructions missing")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := environment["RELAY_CONTROLLER_STATE"] + "\x00" + environment["RELAY_AGENT_CAPABILITY_FILE"] + "\x00" + environment["RELAY_BIN"] + "\x00"
	if string(data) != want {
		t.Fatal("private launch environment was changed")
	}
	encoded, _ := json.Marshal(meta)
	if bytes.Contains(encoded, []byte("RELAY_")) || bytes.Contains(encoded, []byte("agent-capabilities")) {
		t.Fatal("private launch configuration entered public session metadata")
	}
	for _, bad := range []map[string]string{{"PATH": "/tmp/attacker"}, {"RELAY_CONTROLLER_STATE": dir, "RELAY_AGENT_CAPABILITY_FILE": filepath.Join(dir, "other.json"), "RELAY_BIN": "/tmp/relay"}} {
		if reply := modeRequest(t, s, map[string]any{"harness": "codex", "cwd": s.home, "mastermindId": testMastermindID, "mastermindEnvironment": bad}); reply.Code != 400 {
			t.Fatal("unscoped launch environment accepted")
		}
	}
	if reply := modeRequest(t, s, map[string]any{"harness": "codex", "cwd": s.home, "mastermindEnvironment": environment}); reply.Code != 400 {
		t.Fatal("ordinary child received controller capability")
	}
}
