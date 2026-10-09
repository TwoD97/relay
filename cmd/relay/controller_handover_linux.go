package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type controllerProcess struct {
	PID        int
	handle     int
	executable string
}

func sameControllerPath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func pinControllerProcess(conn net.Conn) (*controllerProcess, error) {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return nil, errors.New("cannot inspect controller Unix peer")
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *unix.Ucred
	var inspectErr error
	if err = raw.Control(func(fd uintptr) { cred, inspectErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return nil, err
	}
	if inspectErr != nil {
		return nil, inspectErr
	}
	if cred == nil || cred.Pid <= 1 || int(cred.Uid) != os.Getuid() {
		return nil, errors.New("controller peer is not owned by the current user")
	}
	handle, err := unix.PidfdOpen(int(cred.Pid), 0)
	if err != nil {
		return nil, fmt.Errorf("cannot safely pin controller process; Linux pidfd support is required: %w", err)
	}
	return &controllerProcess{PID: int(cred.Pid), handle: handle}, nil
}

func (p *controllerProcess) Close() { _ = unix.Close(p.handle) }

func (p *controllerProcess) Verify(ctx context.Context, health controllerHealth, opts uiOptions) error {
	if health.PID != p.PID {
		return errors.New("pinned controller PID mismatch")
	}
	root := filepath.Join("/proc", strconv.Itoa(p.PID))
	file, err := os.Open(filepath.Join(root, "cmdline"))
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	file.Close()
	if err != nil || len(data) > 64<<10 {
		return errors.New("cannot verify bounded controller command line")
	}
	var args []string
	for _, arg := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		args = append(args, string(arg))
	}
	if err := validateControllerArguments(args, health, opts); err != nil {
		return err
	}
	executable, err := os.Readlink(filepath.Join(root, "exe"))
	if err != nil {
		return err
	}
	if health.Executable != "" && !sameControllerPath(health.Executable, executable) {
		return errors.New("controller executable does not match private health")
	}
	if err := verifyControllerVersion(ctx, filepath.Join(root, "exe"), health.Version); err != nil {
		return err
	}
	if health.Handover != 0 {
		if err := verifyControllerRelease(executable, opts.StateDir); err != nil {
			return err
		}
	}
	p.executable = executable
	return nil
}

func (p *controllerProcess) StopLegacy(stateDir string) error {
	if err := verifyControllerRelease(p.executable, stateDir); err != nil {
		return fmt.Errorf("legacy controller cannot be upgraded automatically: %w. Stop this verified controller with `kill -TERM %d`, then choose Reconnect; terminal daemons remain running", err, p.PID)
	}
	return unix.PidfdSendSignal(p.handle, unix.SIGTERM, nil, 0)
}

func (p *controllerProcess) Wait(ctx context.Context) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		fds := []unix.PollFd{{Fd: int32(p.handle), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 50)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		if n > 0 && fds[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("controller shutdown timed out")
		}
	}
}
