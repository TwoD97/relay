//go:build linux && relay_ssh_native

package transport

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func nativeSSHExecutable() (string, error) { return exec.LookPath("ssh") }
func nativeSSHConfig(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.WaitDelay = time.Second
	out := &boundedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	err := cmd.Run()
	if out.truncated {
		return nil, errors.New("OpenSSH configuration output is too large")
	}
	return out.Bytes(), err
}
func nativeSSHAgent(ctx context.Context, path string) (net.Conn, error) {
	if path == "" {
		path = os.Getenv("SSH_AUTH_SOCK")
	}
	if path == "" {
		return nil, os.ErrNotExist
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
func nativeKnownHostsLock(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
