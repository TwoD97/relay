//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// The lock coordinates our daemons, but does not prove a pre-existing socket
// belongs to one. Preserve any live/unrecognised endpoint instead of unlinking it.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("runtime socket path already exists and is not a socket; refusing to remove it")
	}
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		return errors.New("a live server already owns the runtime socket; refusing to replace it")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot prove existing runtime socket is stale: %w", err)
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("runtime socket changed while checking it; refusing to remove it")
	}
	return os.Remove(path)
}
