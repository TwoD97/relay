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
	"reflect"
	"strings"
	"testing"
	"time"
)

func modeStub(t *testing.T, s *Server, harness, help, result string) (string, string) {
	t.Helper()
	s.home = t.TempDir()
	dir := filepath.Join(s.stateDir, "bin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, harness)
	capture := filepath.Join(s.stateDir, harness+"-argv")
	script := "#!/bin/sh\ncase \"$1\" in\n--help) printf '%s\\n' " + shellQuote(help) + "; exit 0;;\n--version) printf '0.0.0\\n'; exit 0;;\nesac\nprintf 'started\\n' >> " + shellQuote(capture+"-starts") + "\nprintf '%s\\0' \"$@\" > " + shellQuote(capture) + "\nprintf '%s' \"${RELAY_AGENT_MODE-unset}\" > " + shellQuote(capture+"-mode") + "\n" + result + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path, capture
}

func modeRequest(t *testing.T, s *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(data)))
	return response
}

func modeSession(t *testing.T, s *Server, response *httptest.ResponseRecorder) *liveSession {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", response.Code, response.Body.String())
	}
	var session Session
	if err := json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	live, err := s.lookup(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stub did not exit")
	}
	return live
}

func capturedArguments(t *testing.T, capture string) []string {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func TestAgentModeAutoUsesExplicitFlagsAndLiteralPrompt(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			s := testServer(t)
			help := "--dangerously-skip-permissions --append-system-prompt --dangerously-bypass-approvals-and-sandbox [PROMPT]"
			_, capture := modeStub(t, s, harness, help, "printf 'stub completed\\n'")
			instructions := filepath.Join(s.home, "AGENTS.md")
			original := []byte("User project instructions must remain byte-identical.\n")
			if err := os.WriteFile(instructions, original, 0600); err != nil {
				t.Fatal(err)
			}
			live := modeSession(t, s, modeRequest(t, s, map[string]any{"harness": harness, "agentMode": "auto", "cwd": s.home}))
			meta := live.snapshot()
			if meta.AgentMode != "auto" || meta.Status != "exited" || meta.ExitCode == nil || *meta.ExitCode != 0 {
				t.Fatalf("unexpected metadata: %+v", meta)
			}
			args := capturedArguments(t, capture)
			var suffix []string
			if harness == "claude" {
				suffix = []string{"--dangerously-skip-permissions", "--append-system-prompt", autoAgentInstruction}
			} else {
				suffix = []string{"--dangerously-bypass-approvals-and-sandbox", "--", autoAgentInstruction}
			}
			if len(args) < len(suffix) || !reflect.DeepEqual(args[len(args)-len(suffix):], suffix) {
				t.Fatalf("wrong literal argument suffix: %q", args)
			}
			for _, arg := range args {
				if arg == "--system-prompt" || strings.HasPrefix(arg, "developer_instructions=") || strings.HasPrefix(arg, "model_instructions_file=") || arg == "--dangerously-bypass-hook-trust" {
					t.Fatalf("overrides user instructions or hook policy: %s", arg)
				}
			}
			if got, _ := os.ReadFile(capture + "-mode"); string(got) != "auto" {
				t.Fatalf("wrong child mode: %q", got)
			}
			if got, _ := os.ReadFile(instructions); !bytes.Equal(got, original) {
				t.Fatal("project instructions changed")
			}
			if !strings.Contains(historyOf(live), "organization restrictions still apply") {
				t.Fatal("permission scope is missing from session output")
			}
			stored, err := os.ReadFile(filepath.Join(s.stateDir, "sessions.json"))
			if err != nil || !bytes.Contains(stored, []byte(`"agentMode":"auto"`)) {
				t.Fatalf("mode not persisted: %v", err)
			}
		})
	}
}

func TestAgentModeManualDefaultDoesNotInheritAuto(t *testing.T) {
	t.Setenv("RELAY_AGENT_MODE", "auto")
	for _, mode := range []string{"", "manual"} {
		t.Run("request_"+mode, func(t *testing.T) {
			s := testServer(t)
			_, capture := modeStub(t, s, "codex", "unsupported older help", "exit 0")
			request := map[string]any{"harness": "codex", "cwd": s.home}
			if mode != "" {
				request["agentMode"] = mode
			}
			live := modeSession(t, s, modeRequest(t, s, request))
			if live.snapshot().AgentMode != "manual" {
				t.Fatal("new session did not default to Manual")
			}
			for _, arg := range capturedArguments(t, capture) {
				if strings.Contains(arg, "dangerously") || arg == autoAgentInstruction {
					t.Fatalf("Manual inherited Auto settings: %q", arg)
				}
			}
			if got, _ := os.ReadFile(capture + "-mode"); string(got) != "manual" {
				t.Fatal("parent mode leaked into new session")
			}
		})
	}
}

func TestAgentModeInvalidAndUnsupportedFailBeforeCreation(t *testing.T) {
	for _, tc := range []struct {
		harness, mode, help string
		status              int
	}{
		{"shell", "auto", "", 400},
		{"codex", "typo", "", 400},
		{"claude", "auto", "--allow-dangerously-skip-permissions --append-system-prompt", 422},
		{"claude", "auto", "--dangerously-skip-permissions", 422},
		{"codex", "auto", "--full-auto [PROMPT]", 422},
		{"codex", "auto", "--dangerously-bypass-approvals-and-sandbox", 422},
	} {
		t.Run(tc.harness+"_"+tc.mode+"_"+tc.help, func(t *testing.T) {
			s := testServer(t)
			_, capture := modeStub(t, s, tc.harness, tc.help, "exit 0")
			response := modeRequest(t, s, map[string]any{"harness": tc.harness, "agentMode": tc.mode, "cwd": s.home})
			if response.Code != tc.status || len(s.sessions) != 0 {
				t.Fatalf("unexpected creation result: %d %s", response.Code, response.Body.String())
			}
			if _, err := os.Stat(capture); !os.IsNotExist(err) {
				t.Fatal("provider started despite rejected mode")
			}
		})
	}
}

func TestAgentModeProviderPolicyFailureDoesNotRetryOrDowngrade(t *testing.T) {
	s := testServer(t)
	_, capture := modeStub(t, s, "claude", "--dangerously-skip-permissions --append-system-prompt", "printf 'Policy prohibits bypass mode\\n' >&2; exit 78")
	live := modeSession(t, s, modeRequest(t, s, map[string]any{"harness": "claude", "agentMode": "auto", "cwd": s.home}))
	meta := live.snapshot()
	if meta.ExitCode == nil || *meta.ExitCode != 78 || meta.AgentMode != "auto" || len(s.sessions) != 1 {
		t.Fatalf("policy rejection was hidden: %+v", meta)
	}
	if !strings.Contains(historyOf(live), "Policy prohibits bypass mode") {
		t.Fatal("provider startup diagnostic was hidden")
	}
	if got := capturedArguments(t, capture); !reflect.DeepEqual(got[len(got)-3:], []string{"--dangerously-skip-permissions", "--append-system-prompt", autoAgentInstruction}) {
		t.Fatal("policy rejection retried with reduced permissions")
	}
	if starts, _ := os.ReadFile(capture + "-starts"); string(starts) != "started\n" {
		t.Fatal("provider launch was replayed")
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+meta.ID, strings.NewReader(`{"agentMode":"manual"}`)))
	if response.Code != 400 || live.snapshot().AgentMode != "auto" {
		t.Fatal("launch mode was mutable after session creation")
	}
}

func TestAgentModeProbeCancellationAndBounds(t *testing.T) {
	s := testServer(t)
	path, capture := modeStub(t, s, "claude", strings.Repeat("x", 70<<10)+" --dangerously-skip-permissions --append-system-prompt", "exit 0")
	if err := s.validateAutoAgent(context.Background(), "claude", path); err == nil {
		t.Fatal("oversized help was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.validateAutoAgent(ctx, "claude", path); err == nil {
		t.Fatal("cancelled capability check succeeded")
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("capability check started a model session")
	}
}

func TestAgentModeCancelledPermissionProbeDoesNotLaunch(t *testing.T) {
	s := testServer(t)
	path, capture := modeStub(t, s, "claude", "--dangerously-skip-permissions --append-system-prompt", "exit 0")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("--version) printf '0.0.0\\n'; exit 0;;"), []byte("--version) exec /bin/sleep 30;;"), 1)
	if err := os.WriteFile(path, script, 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"harness": "claude", "agentMode": "auto", "cwd": s.home})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body)).WithContext(ctx))
	if response.Code != http.StatusRequestTimeout || len(s.sessions) != 0 {
		t.Fatalf("cancelled preparation created a session: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatal("provider started after preparation was cancelled")
	}
}

func TestAgentModeDiscoveryAndLegacySavedMetadata(t *testing.T) {
	s := testServer(t)
	for _, id := range []string{"claude", "codex"} {
		modeStub(t, s, id, "", "exit 1")
		if got := s.inspectHarness(context.Background(), id).SessionModes; !reflect.DeepEqual(got, []string{"manual", "auto"}) {
			t.Fatalf("new runtime omitted API capability: %v", got)
		}
	}
	legacy := Session{ID: strings.Repeat("a", 32), Title: "Existing", Workspace: "Default", Cwd: s.stateDir, Harness: "claude", Status: "exited"}
	state, _ := json.Marshal(storedState{Protocol: 1, Sessions: []Session{legacy}})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if got := reloaded.sessions[legacy.ID].snapshot().AgentMode; got != "" {
		t.Fatal("legacy record was relabeled as a known launch mode")
	}
}

// --help exits during argument parsing. This gate never opens a conversation,
// changes provider settings, requests login, or invokes a model.
func TestInstalledAgentModeFlags(t *testing.T) {
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
			if err := s.validateAutoAgent(context.Background(), harness, path); err != nil {
				t.Fatal(err)
			}
			// Put --help before Codex's positional delimiter so it remains a
			// CLI option, regardless of the literal instruction contents.
			args := agentModeArguments(harness, "auto", []string{"--help"})
			if _, err := s.probe(context.Background(), path, args...); err != nil {
				t.Fatalf("installed CLI rejected Auto arguments: %T", err)
			}
		})
	}
}
