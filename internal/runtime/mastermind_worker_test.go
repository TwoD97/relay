//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func workerLaunch(t *testing.T) MastermindWorkerLaunch {
	t.Helper()
	return MastermindWorkerLaunch{Format: 1, LaunchID: testMastermindID, SessionID: "7934857619204abcaed5038327409b1a", SessionCreatedAt: time.Now().UTC(), Cwd: t.TempDir(), Harness: "claude", AgentMode: "manual"}
}

func TestMastermindWorkerClaimIsExclusiveAndDoesNotTouchRuntimeState(t *testing.T) {
	dir := t.TempDir()
	state := []byte("deliberately invalid runtime state owned by the live daemon\n")
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	launch := workerLaunch(t)
	var succeeded atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if claimMastermindWorker(dir, launch) == nil {
				succeeded.Add(1)
			}
		}()
	}
	group.Wait()
	if succeeded.Load() != 1 {
		t.Fatalf("got %d launch claims", succeeded.Load())
	}
	status, err := ReadMastermindWorker(dir, launch.LaunchID)
	if err != nil || !status.Found || status.Launch == nil || *status.Launch != launch {
		t.Fatalf("receipt did not retain exact identity: %+v %v", status, err)
	}
	info, err := os.Stat(filepath.Join(dir, "mastermind-workers", launch.LaunchID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt is not private")
	}
	got, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil || !bytes.Equal(got, state) {
		t.Fatal("worker touched the daemon's session state")
	}
}

func TestMastermindWorkerReceiptRejectsCorruptionLinksAndReplay(t *testing.T) {
	for _, scenario := range []string{"empty", "oversized", "wrong-token", "unknown-field", "symlink", "directory-link"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			launch := workerLaunch(t)
			if status, err := ReadMastermindWorker(dir, launch.LaunchID); err != nil || status.Found {
				t.Fatal("missing receipt was not reported as absent")
			}
			if err := claimMastermindWorker(dir, launch); err != nil {
				t.Fatal(err)
			}
			folder := filepath.Join(dir, "mastermind-workers")
			path := filepath.Join(folder, launch.LaunchID+".json")
			data := []byte{}
			switch scenario {
			case "oversized":
				data = bytes.Repeat([]byte("x"), mastermindWorkerLimit+1)
			case "wrong-token":
				launch.LaunchID = strings.Repeat("a", 32)
				data, _ = json.Marshal(launch)
			case "unknown-field":
				data, _ = json.Marshal(launch)
				data = append(data[:len(data)-1], []byte(`,"credential":"must-not-be-returned"}`)...)
			case "symlink", "directory-link":
				target := path
				if scenario == "directory-link" {
					target = folder
				}
				if err := os.Rename(target, target+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target+"-saved", target); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "symlink" && scenario != "directory-link" {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if result, err := ReadMastermindWorker(dir, testMastermindID); err == nil || result.Found || result.Launch != nil {
				t.Fatal("unverified receipt accepted")
			}
			launch.LaunchID = testMastermindID
			if err := claimMastermindWorker(dir, launch); err == nil {
				t.Fatal("uncertain receipt was replayed")
			}
		})
	}
}

func TestMastermindWorkerProbeIsReadOnlyAndRequiresModeSupport(t *testing.T) {
	s := testServer(t)
	_, capture := modeStub(t, s, "claude", "--append-system-prompt", "exit 0")
	before, err := os.ReadFile(filepath.Join(s.stateDir, "sessions.json"))
	if os.IsNotExist(err) {
		before = []byte("old state belongs to the daemon")
		if err = os.WriteFile(filepath.Join(s.stateDir, "sessions.json"), before, 0600); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	probe, err := ProbeMastermindWorker(context.Background(), s.stateDir, "claude", "manual")
	if err != nil || probe.Protocol != 1 || !probe.MastermindWorkerV1 || !filepath.IsAbs(probe.Executable) {
		t.Fatalf("manual probe failed: %+v %v", probe, err)
	}
	for _, input := range [][2]string{{"claude", "auto"}, {"shell", "manual"}, {"claude", ""}, {"claude", "unrestricted"}} {
		if _, err := ProbeMastermindWorker(context.Background(), s.stateDir, input[0], input[1]); err == nil {
			t.Fatal("unsupported or implicit mode accepted")
		}
	}
	if _, err := os.Stat(capture + "-starts"); !os.IsNotExist(err) {
		t.Fatal("probe launched a provider conversation")
	}
	if got, _ := os.ReadFile(filepath.Join(s.stateDir, "sessions.json")); !bytes.Equal(got, before) {
		t.Fatal("probe changed runtime session state")
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "mastermind-workers")); !os.IsNotExist(err) {
		t.Fatal("probe created launch state")
	}
}

func TestMastermindWorkerExecHelper(t *testing.T) {
	if os.Getenv("RELAY_TEST_WORKER_EXEC") != "1" {
		return
	}
	var launch MastermindWorkerLaunch
	if json.Unmarshal([]byte(os.Getenv("RELAY_TEST_WORKER_LAUNCH")), &launch) != nil {
		os.Exit(91)
	}
	if err := RunMastermindWorker(context.Background(), os.Getenv("RELAY_TEST_WORKER_DIR"), launch, os.Stdout); err != nil {
		os.Exit(92)
	}
	os.Exit(93) // a successful exec never returns
}

func TestMastermindWorkerRequiresExactLiveSessionEvidence(t *testing.T) {
	dir, err := os.MkdirTemp("", "rmw-proof-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "run", "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	launch := workerLaunch(t)
	correct := Session{ID: launch.SessionID, CreatedAt: launch.SessionCreatedAt, Cwd: launch.Cwd, Harness: "shell", Status: "running"}
	var payload atomic.Value
	set := func(sessions ...Session) { data, _ := json.Marshal(sessions); payload.Store(data) }
	set(correct)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions" {
			t.Error("verification attempted a mutation")
		}
		_, _ = w.Write(payload.Load().([]byte))
	})}
	go server.Serve(listener)
	defer server.Close()
	t.Setenv("RELAY_SESSION_ID", launch.SessionID)
	t.Setenv("RELAY_SOCKET", socket)
	if err := verifyMastermindWorkerSession(context.Background(), dir, launch); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"missing", "duplicate", "time", "cwd", "harness", "finished", "purpose", "mode", "invalid", "oversized", "other-socket", "other-session"} {
		t.Run(scenario, func(t *testing.T) {
			changed := correct
			switch scenario {
			case "time":
				changed.CreatedAt = changed.CreatedAt.Add(time.Nanosecond)
			case "cwd":
				changed.Cwd = filepath.Dir(changed.Cwd)
			case "harness":
				changed.Harness = "claude"
			case "finished":
				changed.Status = "exited"
			case "purpose":
				changed.Purpose = "repair"
			case "mode":
				changed.AgentMode = "auto"
			case "other-socket":
				t.Setenv("RELAY_SOCKET", socket+"-other")
			case "other-session":
				t.Setenv("RELAY_SESSION_ID", strings.Repeat("f", 32))
			}
			set(changed)
			switch scenario {
			case "missing":
				set()
			case "duplicate":
				set(correct, correct)
			case "invalid":
				payload.Store([]byte("not JSON"))
			case "oversized":
				payload.Store(bytes.Repeat([]byte("x"), (512<<10)+1))
			}
			if err := verifyMastermindWorkerSession(context.Background(), dir, launch); err == nil {
				t.Fatal("unverified owning session was accepted")
			}
		})
	}
}

func TestMastermindWorkerExecPreservesSessionAndUsesLiteralRole(t *testing.T) {
	dir, err := os.MkdirTemp("", "rmw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for _, sub := range []string{"run", "bin"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "run", "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	var current Session
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sessions" {
			t.Error("worker attempted a runtime mutation")
			http.Error(w, "unexpected request", 500)
			return
		}
		mu.RLock()
		defer mu.RUnlock()
		_ = json.NewEncoder(w).Encode([]Session{current})
	})}
	go server.Serve(listener)
	defer server.Close()
	fixture := `#!/usr/bin/env python3
import json, os, sys
if sys.argv[1:] == ["--help"]:
    print("--append-system-prompt --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox [PROMPT]")
else:
    with open(os.environ["RELAY_TEST_WORKER_CAPTURE"], "w") as f:
        json.dump({"pid":os.getpid(), "args":sys.argv[1:], "mode":os.environ["RELAY_AGENT_MODE"], "session":os.environ["RELAY_SESSION_ID"], "socket":os.environ["RELAY_SOCKET"], "binary":os.environ["RELAY_BIN"], "cwd":os.getcwd()}, f)
`
	for _, provider := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, "bin", provider), []byte(fixture), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, provider := range []string{"claude", "codex"} {
		for _, mode := range []string{"manual", "auto"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				launch := workerLaunch(t)
				launch.LaunchID, err = newID()
				if err != nil {
					t.Fatal(err)
				}
				launch.Harness, launch.AgentMode = provider, mode
				mu.Lock()
				current = Session{ID: launch.SessionID, CreatedAt: launch.SessionCreatedAt, Cwd: launch.Cwd, Harness: "shell", Status: "running"}
				mu.Unlock()
				encoded, _ := json.Marshal(launch)
				capture := filepath.Join(dir, provider+"-"+mode+"-capture.json")
				child := exec.Command(os.Args[0], "-test.run=^TestMastermindWorkerExecHelper$")
				child.Dir = launch.Cwd
				child.Env = append(os.Environ(), "RELAY_TEST_WORKER_EXEC=1", "RELAY_TEST_WORKER_DIR="+dir, "RELAY_TEST_WORKER_LAUNCH="+string(encoded), "RELAY_TEST_WORKER_CAPTURE="+capture, "RELAY_SESSION_ID="+launch.SessionID, "RELAY_SOCKET="+filepath.Join(dir, "run", "daemon.sock"), "RELAY_AGENT_MODE=stale-parent-mode")
				if output, err := child.CombinedOutput(); err != nil {
					t.Fatalf("worker failed: %v %s", err, output)
				}
				var got struct {
					PID                                int      `json:"pid"`
					Args                               []string `json:"args"`
					Mode, Session, Socket, Binary, Cwd string
				}
				data, err := os.ReadFile(capture)
				if err != nil || json.Unmarshal(data, &got) != nil {
					t.Fatal("missing provider capture")
				}
				if got.PID != child.Process.Pid || got.Mode != mode || got.Session != launch.SessionID || got.Socket != filepath.Join(dir, "run", "daemon.sock") || got.Cwd != launch.Cwd || !filepath.IsAbs(got.Binary) {
					t.Fatalf("worker lost session ownership/environment: %+v", got)
				}
				prompt := got.Args[len(got.Args)-1]
				if !strings.Contains(prompt, mastermindInstruction) || strings.Contains(prompt, autoAgentInstruction) != (mode == "auto") || strings.Contains(strings.Join(got.Args, " "), "--settings") {
					t.Fatal("worker changed role, mode or permission-hook policy")
				}
				status, err := ReadMastermindWorker(dir, launch.LaunchID)
				if err != nil || !status.Found || status.Launch == nil || *status.Launch != launch {
					t.Fatal("exec did not retain its durable launch receipt")
				}
			})
		}
	}
}
