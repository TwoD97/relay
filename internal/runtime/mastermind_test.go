//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testMastermindID = "6c8a54c20eea41aa9756618510c2a7f5"

func TestInstalledMastermindFlags(t *testing.T) {
	if os.Getenv("RELAY_LIVE_HARNESS_SMOKE") != "1" {
		t.Skip("set RELAY_LIVE_HARNESS_SMOKE=1 for installed CLI help-only validation")
	}
	s := testServer(t)
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			path, err := s.findCommand(harness)
			if err != nil {
				t.Skip("provider is not installed")
			}
			if err := s.validateMastermindAgent(context.Background(), harness, path); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"manual", "auto"} {
				args := sessionAgentArguments(harness, mode, true, []string{"--help"})
				if _, err := s.probe(context.Background(), path, args...); err != nil {
					t.Fatalf("installed CLI rejected Mastermind %s arguments: %T", mode, err)
				}
			}
		})
	}
}

func TestMastermindLaunchRolesModesAndDurableIdentity(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, mode := range []string{"manual", "auto"} {
			t.Run(harness+"/"+mode, func(t *testing.T) {
				s := testServer(t)
				_, capture := modeStub(t, s, harness, "--append-system-prompt --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox [PROMPT]", "exit 0")
				instructions := filepath.Join(s.home, "AGENTS.md")
				original := []byte("Existing project policy must be preserved.\n")
				if err := os.WriteFile(instructions, original, 0600); err != nil {
					t.Fatal(err)
				}
				meta := modeSession(t, s, modeRequest(t, s, map[string]any{"harness": harness, "cwd": s.home, "agentMode": mode, "mastermindId": testMastermindID})).snapshot()
				if meta.MastermindID != testMastermindID || meta.AgentMode != mode || meta.Purpose != "" {
					t.Fatalf("wrong coordinator metadata: %+v", meta)
				}
				args := capturedArguments(t, capture)
				prompt := args[len(args)-1]
				if !strings.Contains(prompt, mastermindInstruction) || strings.Count(prompt, mastermindInstruction) != 1 || !strings.Contains(prompt, `"$relay_cli" fleet list`) {
					t.Fatal("missing literal coordinator role/fleet discovery")
				}
				if strings.Contains(prompt, autoAgentInstruction) != (mode == "auto") {
					t.Fatal("permission mode instruction changed")
				}
				bypass := 0
				for _, arg := range args {
					if arg == "--dangerously-skip-permissions" || arg == "--dangerously-bypass-approvals-and-sandbox" {
						bypass++
					}
					if arg == "--system-prompt" || strings.HasPrefix(arg, "developer_instructions=") {
						t.Fatal("project/provider instruction override")
					}
				}
				if (mode == "auto" && bypass != 1) || (mode == "manual" && bypass != 0) {
					t.Fatal("Mastermind implicitly changed permissions")
				}
				if got, _ := os.ReadFile(instructions); !bytes.Equal(got, original) {
					t.Fatal("project policy modified")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				restored, err := New(s.stateDir, "test")
				if err != nil {
					t.Fatal(err)
				}
				defer restored.Close()
				live, err := restored.lookup(meta.ID)
				if err != nil || live.snapshot().MastermindID != testMastermindID || !live.snapshot().CreatedAt.Equal(meta.CreatedAt) {
					t.Fatal("coordinator identity did not survive runtime reload")
				}
			})
		}
	}
}

func TestMastermindRejectsInvalidOrUnsupportedLaunchBeforeStarting(t *testing.T) {
	for _, tc := range []struct {
		name, harness, id, help string
		status                  int
	}{
		{"shell", "shell", testMastermindID, "", 400},
		{"short", "claude", "bad", "--append-system-prompt", 400},
		{"uppercase", "claude", strings.ToUpper(testMastermindID), "--append-system-prompt", 400},
		{"unsupported Claude", "claude", testMastermindID, "old CLI help", 422},
		{"unsupported Codex", "codex", testMastermindID, "old CLI help", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t)
			_, capture := modeStub(t, s, tc.harness, tc.help, "exit 0")
			r := modeRequest(t, s, map[string]any{"harness": tc.harness, "cwd": s.home, "mastermindId": tc.id})
			if r.Code != tc.status {
				t.Fatalf("got %d: %s", r.Code, r.Body.String())
			}
			if _, err := os.Stat(capture + "-starts"); !os.IsNotExist(err) {
				t.Fatal("rejected request started an agent")
			}
			if len(s.sessions) != 0 {
				t.Fatal("rejected request created metadata")
			}
		})
	}
}

func TestMastermindConcurrentLaunchTokenCannotCreateTwoSessions(t *testing.T) {
	s := testServer(t)
	_, capture := modeStub(t, s, "codex", "[PROMPT]", "exit 0")
	var group sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- modeRequest(t, s, map[string]any{"harness": "codex", "cwd": s.home, "mastermindId": testMastermindID})
		}()
	}
	group.Wait()
	close(results)
	created, rejected := 0, 0
	for response := range results {
		switch response.Code {
		case http.StatusCreated:
			created++
			modeSession(t, s, response)
		case http.StatusConflict:
			rejected++
		default:
			t.Fatalf("unexpected response %d: %s", response.Code, response.Body.String())
		}
	}
	starts, err := os.ReadFile(capture + "-starts")
	if err != nil || created != 1 || rejected != 1 || string(starts) != "started\n" {
		t.Fatalf("duplicate startup: %d/%d %q (%v)", created, rejected, starts, err)
	}
	reply := httptest.NewRecorder()
	s.Handler().ServeHTTP(reply, httptest.NewRequest("GET", "/api/health", nil))
	var health struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err = json.Unmarshal(reply.Body.Bytes(), &health); err != nil || !health.Capabilities["mastermindV1"] {
		t.Fatal("runtime capability not advertised")
	}
}
