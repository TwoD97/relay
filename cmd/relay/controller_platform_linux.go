package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const localRuntimeSupported = true

func defaultDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(home, ".local", "share", name)
}
func socketPath(dir string) string       { return filepath.Join(dir, "run", "daemon.sock") }
func controllerSocket(dir string) string { return filepath.Join(dir, "run", "controller.sock") }

func validateControllerOptions(opts uiOptions) error {
	if len(controllerSocket(opts.StateDir)) > 103 || (opts.Local && len(socketPath(opts.RuntimeDir)) > 103) {
		return errors.New("state directory is too long for a Unix socket")
	}
	return nil
}
func detachController(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func controllerUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
func dialControllerControl(ctx context.Context, dir string) (net.Conn, error) {
	return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", controllerSocket(dir))
}
func listenControllerControl(dir string) (net.Listener, func(), error) {
	path := controllerSocket(dir)
	if err := removeStaleSocket(path); err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(path) }
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		cleanup()
		return nil, nil, err
	}
	return listener, cleanup, nil
}

func privateControllerDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("controller state directory must be a directory owned by the current user, not a symlink")
	}
	return os.Chmod(path, 0700)
}

func privateControllerTempDirectory(parent string) (string, error) {
	return os.MkdirTemp(parent, ".stage-")
}

func prepareControllerDirectory(dir string) error {
	if err := privateControllerDirectory(dir); err != nil {
		return err
	}
	return privateControllerDirectory(filepath.Join(dir, "run"))
}

func privateControllerFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || int(stat.Uid) != os.Getuid() {
			err = errors.New("controller file must be a regular file owned by the current user")
		}
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func lockControllerState(dir string) (*os.File, error) {
	return lockControllerFile(dir, "controller.lock")
}

func lockControllerFile(dir, name string) (*os.File, error) {
	if err := prepareControllerDirectory(dir); err != nil {
		return nil, err
	}
	f, err := privateControllerFile(filepath.Join(dir, "run", name))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errControllerRunning
		}
		return nil, err
	}
	return f, nil // Closing the descriptor releases the lifetime lock.
}
