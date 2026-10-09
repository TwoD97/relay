//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// runIsolatedCommand gives a noninteractive probe or installer its own session.
// Keep its leader unreaped until scoped child cleanup finishes. Cmd.Cancel must
// never use a numeric group: os/exec can invoke it concurrently with Process.Wait
// after that PID has already been released by the kernel.
func runIsolatedCommand(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = nil
	// Bound copying from inherited stdout/stderr pipes, including a descendant
	// that deliberately escaped the original session. Go's own cancellation
	// fallback uses its Process handle, never a numeric process group.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	identity, err := identifyProcess(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("identify command process: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- waitCommandExit(cmd.Process.Pid) }()
	var exitErr, cleanupErr error
	select {
	case exitErr = <-exited:
	case <-ctx.Done():
		cleanupErr = signalSession(identity, syscall.SIGKILL)
		if cleanupErr != nil {
			_ = cmd.Process.Kill()
		}
		exitErr = <-exited
	}
	if exitErr == nil {
		// An exited parent can still leave children holding output pipes open.
		cleanupErr = errors.Join(cleanupErr, signalSession(identity, syscall.SIGKILL))
	} else {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), cleanupErr, exitErr)
	}
	return errors.Join(waitErr, cleanupErr, exitErr)
}
