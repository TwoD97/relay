//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// OwnedPTY exposes Relay's existing identity-safe PTY lifecycle to the local
// coordinator service. Final reaping and process-tree signalling are serialized.
type OwnedPTY struct {
	*os.File
	cmd      *exec.Cmd
	identity *ProcessIdentity
	mu       sync.Mutex
	reaped   bool
}

func StartOwnedPTY(cmd *exec.Cmd, cols, rows int) (*OwnedPTY, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	identity, err := identifyProcess(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = f.Close()
		_ = cmd.Wait()
		return nil, err
	}
	// Register a fresh nonblocking descriptor with Go's poller for write deadlines.
	fd, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err == nil {
		err = unix.SetNonblock(fd, true)
	}
	_ = f.Close()
	if err != nil {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		_ = signalSession(identity, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return &OwnedPTY{File: os.NewFile(uintptr(fd), "local-agent-pty"), cmd: cmd, identity: identity}, nil
}
func (p *OwnedPTY) Identity() json.RawMessage   { b, _ := json.Marshal(p.identity); return b }
func (p *OwnedPTY) Resize(cols, rows int) error { return resizePTY(p.File, uint16(cols), uint16(rows)) }
func (p *OwnedPTY) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return nil
	}
	err := signalSession(p.identity, syscall.SIGKILL)
	if err != nil {
		_ = p.cmd.Process.Kill()
	}
	return err
}
func (p *OwnedPTY) Wait() (int, error) {
	exitErr := waitCommandExit(p.cmd.Process.Pid)
	p.mu.Lock()
	defer p.mu.Unlock()
	cleanupErr := signalSession(p.identity, syscall.SIGKILL)
	if cleanupErr != nil {
		_ = p.cmd.Process.Kill()
	}
	waitErr := p.cmd.Wait()
	p.reaped = true
	code := 0
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		waitErr = nil
	}
	return code, errors.Join(exitErr, cleanupErr, waitErr)
}
func RecoverOwnedPTY(raw json.RawMessage) string {
	var identity ProcessIdentity
	if json.Unmarshal(raw, &identity) != nil {
		return "No usable process identity was saved; no process was signalled."
	}
	return recoverInterruptedProcess(&identity).Detail
}

// ProbeOwnedCommand keeps the same process ownership guarantees for discovery.
func ProbeOwnedCommand(ctx context.Context, cmd *exec.Cmd) error { return runIsolatedCommand(ctx, cmd) }
