//go:build linux

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func commandFixturePIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	var leader, child int
	eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		_, err = fmt.Sscanf(string(data), "%d %d", &leader, &child)
		return err == nil && leader > 1 && child > 1
	})
	for _, pid := range []int{leader, child} {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); _ = unix.Close(fd) })
	}
	return leader, child
}

func TestIsolatedCommandCancellationStopsChildrenWithoutTouchingOtherSessions(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			_, sentinel := startRecoverySentinel(t, true)
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			pidFile := filepath.Join(t.TempDir(), "pids")
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `trap '' HUP TERM; sleep 60 & printf '%s %s' "$$" "$!" > "$1"; wait`, "fixture", pidFile)
			result := make(chan error, 1)
			go func() { result <- runMaintenanceCommand(ctx, cmd, io.Discard) }()
			leader, child := commandFixturePIDs(t, pidFile)
			if !deadline {
				cancel()
			}
			select {
			case err := <-result:
				want := context.Canceled
				if deadline {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatal("lost cancellation result", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled installer exceeded cleanup deadline")
			}
			if processAlive(leader) || processAlive(child) {
				t.Fatal("cancelled installer left same-session processes alive")
			}
			if current, err := inspectProcess(sentinel.pid); err != nil || current.startTime != sentinel.startTime || !processAlive(sentinel.pid) {
				t.Fatal("cancelled installer signalled another session", current, err)
			}
		})
	}
}

func TestIsolatedCommandNaturalExitReapsChildrenAndPreservesOutputAndExitCode(t *testing.T) {
	_, sentinel := startRecoverySentinel(t, true)
	pidFile := filepath.Join(t.TempDir(), "pids")
	gate := filepath.Join(t.TempDir(), "exit")
	cmd := exec.Command("/bin/sh", "-c", `trap '' HUP TERM; sleep 60 & printf '%s %s' "$$" "$!" > "$1"; while [ ! -e "$2" ]; do sleep 0.01; done; printf 'saved output'; exit 19`, "fixture", pidFile, gate)
	var output bytes.Buffer
	result := make(chan error, 1)
	go func() { result <- runMaintenanceCommand(context.Background(), cmd, &output) }()
	leader, child := commandFixturePIDs(t, pidFile)
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 19 {
			t.Fatal("lost natural exit status", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inherited output pipes blocked completed installer")
	}
	if output.String() != "saved output" || processAlive(leader) || processAlive(child) {
		t.Fatal("lost output or left completed installer children alive", output.String())
	}
	if current, err := inspectProcess(sentinel.pid); err != nil || current.startTime != sentinel.startTime || !processAlive(sentinel.pid) {
		t.Fatal("finished installer signalled another session", current, err)
	}
}

func TestProbeStillBoundsOutputAndHonorsAlreadyCancelledContext(t *testing.T) {
	s := testServer(t)
	output, err := s.probe(context.Background(), "/bin/sh", "-c", "head -c 16384 /dev/zero")
	if err != nil || len(output) != 8192 {
		t.Fatal("probe output boundary changed", len(output), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "must-not-run")
	_, err = s.probe(ctx, "/bin/sh", "-c", `touch "$1"`, "fixture", marker)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("probe ignored cancellation", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("cancelled probe executed", err)
	}
}
