//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This subprocess owns real PTYs and runtime state, then the parent kills it
// with SIGKILL. No graceful Close is used to model the crash.
func TestRuntimeCrashProcessHelper(t *testing.T) {
	dir := os.Getenv("RELAY_TEST_CRASH_RUNTIME")
	if dir == "" {
		return
	}
	server, err := New(dir, "crash-helper")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	listener, err := net.Listen("unix", filepath.Join(dir, "test.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(filepath.Join(dir, "test.sock"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := http.Serve(listener, server.Handler()); err != nil {
		t.Fatal(err)
	}
}

func processAlive(pid int) bool {
	facts, err := inspectProcess(pid)
	return err == nil && facts.state != 'Z' && facts.state != 'X'
}

func startRecoverySentinel(t *testing.T, separateSession bool) (*exec.Cmd, processFacts) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if separateSession {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	facts, err := inspectProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return cmd, facts
}

func TestInterruptedRecoveryNeverSignalsUnprovenProcessIdentity(t *testing.T) {
	_, facts := startRecoverySentinel(t, true)
	_, nonLeader := startRecoverySentinel(t, false)
	boot, err := bootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name     string
		identity *ProcessIdentity
		status   string
	}{
		{"legacy metadata", nil, "unverified"},
		{"unsafe PID", &ProcessIdentity{PID: 1, StartTime: facts.startTime, BootID: boot}, "unverified"},
		{"reused PID", &ProcessIdentity{PID: facts.pid, StartTime: facts.startTime + 1, BootID: boot}, "unverified"},
		{"different boot", &ProcessIdentity{PID: facts.pid, StartTime: facts.startTime, BootID: "00000000-0000-0000-0000-000000000000"}, "host-restarted"},
		{"not the session leader", &ProcessIdentity{PID: nonLeader.pid, StartTime: nonLeader.startTime, BootID: boot}, "unverified"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			recovery := recoverInterruptedProcess(scenario.identity)
			if recovery.Status != scenario.status || recovery.Detail == "" {
				t.Fatal("incorrect recovery outcome", recovery)
			}
			for _, sentinel := range []processFacts{facts, nonLeader} {
				current, err := inspectProcess(sentinel.pid)
				if err != nil || current.startTime != sentinel.startTime || current.state == 'Z' || current.state == 'T' || current.state == 't' {
					t.Fatal("unrelated sentinel was killed or stopped", current, err)
				}
			}
		})
	}
}

func TestRuntimeSIGKILLRecoveryStopsVerifiedOrphansAndPreservesOtherSessions(t *testing.T) {
	dir, err := os.MkdirTemp("", "relay-crash-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	shell := filepath.Join(dir, "controlled-shell")
	script := `#!/bin/sh
trap '' HUP TERM
/bin/sh -c 'trap "" HUP TERM; printf "CHILD_PID:%s\n" "$$"; while :; do /bin/sleep 1; done' &
printf 'LEADER_PID:%s\n' "$$"
wait
`
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(executable, "-test.run=^TestRuntimeCrashProcessHelper$")
	owner.Env = append(os.Environ(), "RELAY_TEST_CRASH_RUNTIME="+dir, "SHELL="+shell)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ownerReaped := false
	t.Cleanup(func() {
		if !ownerReaped {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "test.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	eventually(t, func() bool {
		response, err := client.Get("http://runtime/api/health")
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == 200
	})
	body, _ := json.Marshal(createRequest{Title: "Disposable crash test", Workspace: "Tests", Cwd: dir, Harness: "shell"})
	response, err := client.Post("http://runtime/api/sessions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var created Session
	err = json.NewDecoder(response.Body).Decode(&created)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 {
		t.Fatal("create session", response.StatusCode, err)
	}
	var leaderPID, childPID int
	eventually(t, func() bool {
		response, err := client.Get("http://runtime/api/sessions/" + created.ID + "/history")
		if err != nil {
			return false
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		for _, line := range strings.Fields(string(data)) {
			if strings.HasPrefix(line, "LEADER_PID:") {
				leaderPID, _ = strconv.Atoi(strings.TrimPrefix(line, "LEADER_PID:"))
			}
			if strings.HasPrefix(line, "CHILD_PID:") {
				childPID, _ = strconv.Atoi(strings.TrimPrefix(line, "CHILD_PID:"))
			}
		}
		return leaderPID > 1 && childPID > 1
	})
	// Pin only our fixture's exact identities for failure cleanup as well.
	for _, pid := range []int{leaderPID, childPID} {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
	}
	_, sentinel := startRecoverySentinel(t, true)
	deadline := time.Now().Add(7 * time.Second)
	durable := false
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "history", created.ID+".bin"))
		if strings.Contains(string(data), "LEADER_PID:") && strings.Contains(string(data), "CHILD_PID:") {
			durable = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !durable {
		t.Fatal("history did not reach its periodic durable checkpoint")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	ownerReaped = true
	if !processAlive(leaderPID) || !processAlive(childPID) {
		t.Fatal("fixture did not reproduce HUP-ignoring orphan processes")
	}
	restored, err := New(dir, "new-runtime")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	p, err := restored.lookup(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta := p.snapshot()
	if meta.Status != "interrupted" || meta.Recovery == nil || meta.Recovery.Status != "stopped" || meta.ProcessIdentity == nil || meta.ProcessIdentity.PID != leaderPID {
		t.Fatalf("incorrect crash recovery result: %+v / %+v", meta, meta.Recovery)
	}
	if processAlive(leaderPID) || processAlive(childPID) {
		t.Fatal("verified crashed-session processes survived recovery")
	}
	if current, err := inspectProcess(sentinel.pid); err != nil || current.startTime != sentinel.startTime || !processAlive(sentinel.pid) || current.state == 'T' {
		t.Fatal("unrelated live session was affected", current, err)
	}
	if err := p.writeInput("must not restart"); err == nil {
		t.Fatal("interrupted session accepted input")
	}
	if !strings.Contains(historyOf(p), "LEADER_PID:") || strings.Count(historyOf(p), meta.Recovery.Detail) != 1 {
		t.Fatal("durable history or recovery notice missing")
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(dir, "next-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	replayed, _ := again.lookup(created.ID)
	if strings.Count(historyOf(replayed), meta.Recovery.Detail) != 1 {
		t.Fatal("opening interrupted history duplicated the recovery notice")
	}
}

func TestLegacyInterruptedSessionReportsUnverifiedCleanup(t *testing.T) {
	dir := t.TempDir()
	meta := Session{ID: strings.Repeat("a", 32), Title: "Legacy", Workspace: "Tests", Cwd: dir, Harness: "shell", Status: "running"}
	data, _ := json.Marshal(storedState{Protocol: 1, Sessions: []Session{meta}})
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, "new")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, _ := s.lookup(meta.ID)
	if recovery := p.snapshot().Recovery; recovery == nil || recovery.Status != "unverified" || !strings.Contains(historyOf(p), "inspect this host") {
		t.Fatal(fmt.Sprintf("legacy metadata silently claimed cleanup: %+v", recovery))
	}
}

func TestSessionRejectsInputUntilProcessIdentityIsDurable(t *testing.T) {
	s := testServer(t)
	start := make(chan struct{})
	meta, err := s.start(Session{Title: "Durability barrier", Workspace: "Tests", Cwd: s.stateDir, Harness: "shell"}, 80, 24, func(context.Context, io.Writer) (*exec.Cmd, error) {
		<-start
		return exec.Command("/bin/sh", "-c", `IFS= read -r line; printf '<%s>\n' "$line"`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.lookup(meta.ID)
	// Let the child start while holding the metadata writer: its PTY exists,
	// but the new process identity cannot have reached durable storage yet.
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	close(start)
	eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.pty != nil && p.meta.ProcessIdentity != nil
	})
	if err := p.writeInput("before-save\n"); err == nil || err.Error() != "session is not accepting input" {
		t.Fatal("input executed before identity persistence", err)
	}
	s.mu.Unlock()
	locked = false
	eventually(t, func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.inputReady })
	if err := p.writeInput("accepted\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not finish after durable input was accepted")
	}
	if history := historyOf(p); !strings.Contains(history, "<accepted>") || strings.Contains(history, "before-save") {
		t.Fatal("rejected startup input was buffered or replayed", history)
	}
}

func TestNaturalExitRetainsLeaderUntilChildrenAreCleanedAndNeverSignalsAfterReap(t *testing.T) {
	s := testServer(t)
	_, sentinel := startRecoverySentinel(t, true)
	p := startTest(t, s, `trap '' HUP TERM; sleep 60 & printf 'child:%s\n' "$!"; IFS= read -r line; exit 23`)
	eventually(t, func() bool { return strings.Contains(historyOf(p), "child:") })
	var childPID int
	if _, err := fmt.Sscanf(strings.TrimSpace(historyOf(p)), "child:%d", &childPID); err != nil || childPID <= 1 {
		t.Fatal("missing child identity", historyOf(p), err)
	}
	childFD, err := unix.PidfdOpen(childPID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(childFD)
	defer unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0)
	identity := p.snapshot().ProcessIdentity
	if identity == nil {
		t.Fatal("running child identity was not persisted")
	}
	// Pause cleanup after the real shell exits. Its zombie must still reserve
	// the session ID, so a later process cannot reuse it during child cleanup.
	p.processMu.Lock()
	locked := true
	defer func() {
		if locked {
			p.processMu.Unlock()
		}
	}()
	if err := p.writeInput("finish\n"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		facts, err := inspectProcess(identity.PID)
		return err == nil && facts.startTime == identity.StartTime && facts.state == 'Z'
	})
	if !processAlive(childPID) {
		t.Fatal("fixture child did not survive shell exit")
	}
	p.processMu.Unlock()
	locked = false
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		t.Fatal("natural exit did not finish bounded child cleanup")
	}
	if p.snapshot().ExitCode == nil || *p.snapshot().ExitCode != 23 || processAlive(childPID) {
		t.Fatal("natural exit lost its status or left the same-session child alive", p.snapshot())
	}
	if _, err := inspectProcess(identity.PID); !os.IsNotExist(err) {
		t.Fatal("finished leader was not reaped", err)
	}
	// Even a valid current child identity must not authorize any signals once
	// this liveSession's original command was reaped.
	boot, err := bootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.meta.ProcessIdentity = &ProcessIdentity{PID: sentinel.pid, StartTime: sentinel.startTime, BootID: boot}
	p.mu.Unlock()
	if err := p.signalProcess(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if current, err := inspectProcess(sentinel.pid); err != nil || current.startTime != sentinel.startTime || !processAlive(sentinel.pid) {
		t.Fatal("post-reap cleanup signalled an unrelated child", current, err)
	}
}
