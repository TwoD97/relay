//go:build linux

package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type signalFixture struct {
	command  *exec.Cmd
	identity *ProcessIdentity
	child    processFacts
	reaped   bool
}

func newSignalFixture(t *testing.T, exitLeader, detachedChild bool) *signalFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	ending := "wait"
	if exitLeader {
		ending = "exit 0"
	}
	// Bash job control puts sleep in a separate process group. Both remain in
	// this disposable Linux session; their inherited TERM handler is ignored.
	jobControl, childCommand := "set -m", "/bin/sleep 60"
	if detachedChild {
		jobControl, childCommand = "set +m", "setsid /bin/sleep 60"
	}
	command := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", jobControl+"; trap '' HUP TERM; "+childCommand+" & printf '%s %s\\n' \"$$\" \"$!\"; "+ending)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	fixture := &signalFixture{command: command}
	t.Cleanup(func() {
		_ = command.Process.Kill() // os.Process targets this exact owned child.
		if !fixture.reaped {
			_ = command.Wait()
		}
	})
	fixture.identity, err = identifyProcess(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal("fixture did not publish process identities", err)
	}
	var leader, child int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d %d", &leader, &child); err != nil || leader != command.Process.Pid || child <= 1 {
		t.Fatal("invalid fixture process identities", line, err)
	}
	expectedSession := leader
	if detachedChild {
		expectedSession = child
	}
	eventually(t, func() bool {
		fixture.child, err = inspectProcess(child)
		return err == nil && fixture.child.session == expectedSession && fixture.child.uid == os.Geteuid()
	})
	fd, err := unix.PidfdOpen(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Failure cleanup pins the fixture child rather than using a numeric group.
	t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
	return fixture
}

func assertSignalSentinelAlive(t *testing.T, expected processFacts) {
	t.Helper()
	actual, err := inspectProcess(expected.pid)
	if err != nil || actual.startTime != expected.startTime || actual.state == 'Z' || actual.state == 'X' || actual.state == 'T' || actual.state == 't' {
		t.Fatal("unrelated sentinel was terminated or stopped", actual, err)
	}
}

func TestSignalSessionPinsMembersAndPreservesUnrelatedSentinel(t *testing.T) {
	_, sentinel := startRecoverySentinel(t, true)
	fixture := newSignalFixture(t, false, false)
	group, err := syscall.Getpgid(fixture.child.pid)
	if err != nil || group == fixture.identity.PID {
		t.Fatal("fixture did not create a separate foreground/background group", group, err)
	}
	if err := signalSession(fixture.identity, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	assertSignalSentinelAlive(t, sentinel)
	// The fixture ignores TERM, so KILL must cover both process groups.
	if err := signalSession(fixture.identity, syscall.SIGKILL); err != nil {
		t.Fatal("verified session cleanup failed", err)
	}
	_ = fixture.command.Wait()
	fixture.reaped = true
	if processAlive(fixture.child.pid) {
		t.Fatal("separate-group child survived session cleanup")
	}
	assertSignalSentinelAlive(t, sentinel)
	if err := signalSession(fixture.identity, syscall.SIGKILL); err == nil {
		t.Fatal("reaped leader identity was allowed to authorize another signal")
	}
	assertSignalSentinelAlive(t, sentinel)
}

func TestSignalSessionCleansDescendantsWhileExitedLeaderRemainsUnreaped(t *testing.T) {
	_, sentinel := startRecoverySentinel(t, true)
	fixture := newSignalFixture(t, true, false)
	eventually(t, func() bool {
		leader, err := inspectProcess(fixture.identity.PID)
		return err == nil && leader.state == 'Z'
	})
	if !processAlive(fixture.child.pid) {
		t.Fatal("fixture did not leave a child after its leader exited")
	}
	if err := signalSession(fixture.identity, syscall.SIGKILL); err != nil {
		t.Fatal("unreaped leader did not authorize safe descendant cleanup", err)
	}
	if err := fixture.command.Wait(); err != nil {
		t.Fatal("leader's original successful exit status changed", err)
	}
	fixture.reaped = true
	if processAlive(fixture.child.pid) {
		t.Fatal("orphan child survived unreaped-leader cleanup")
	}
	assertSignalSentinelAlive(t, sentinel)
}

func TestSignalSessionRejectsStaleIdentityWithoutTouchingItsPID(t *testing.T) {
	_, sentinel := startRecoverySentinel(t, true)
	identity, err := identifyProcess(sentinel.pid)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ProcessIdentity){
		func(identity *ProcessIdentity) { identity.StartTime++ },
		func(identity *ProcessIdentity) { identity.BootID = "00000000-0000-0000-0000-000000000000" },
		func(identity *ProcessIdentity) { identity.PID = 1 },
	} {
		bad := *identity
		mutate(&bad)
		if err := signalSession(&bad, syscall.SIGKILL); err == nil {
			t.Fatal("unproven process identity authorized a signal")
		}
		assertSignalSentinelAlive(t, sentinel)
	}
}

func TestSignalSessionRejectsSessionOwnedByAnotherParent(t *testing.T) {
	fixture := newSignalFixture(t, false, true)
	// This is a real same-user session leader with a matching birth identity,
	// but its parent is the fixture shell, not this test's runtime process.
	if fixture.child.parentPID != fixture.command.Process.Pid || fixture.child.session != fixture.child.pid {
		t.Fatal("fixture did not create a session owned by another parent")
	}
	boot, err := bootIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity := &ProcessIdentity{PID: fixture.child.pid, StartTime: fixture.child.startTime, BootID: boot}
	if err := signalSession(identity, syscall.SIGKILL); err == nil {
		t.Fatal("another parent's session identity authorized signalling")
	}
	assertSignalSentinelAlive(t, fixture.child)
	if err := signalSession(fixture.identity, syscall.SIGKILL); err != nil {
		t.Fatal("fixture cleanup", err)
	}
}
