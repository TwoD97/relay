package main

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type controllerProcess struct {
	PID        int
	handle     windows.Handle
	executable string
}

func sameControllerPath(a, b string) bool {
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func pinControllerProcess(conn net.Conn) (*controllerProcess, error) {
	file, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return nil, errors.New("cannot pin controller pipe peer")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(file.Fd()), &pid); err != nil {
		return nil, err
	}
	if pid <= 1 {
		return nil, errors.New("invalid controller peer PID")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return nil, err
	}
	var token windows.Token
	if err = windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err == nil {
		var sid *windows.SID
		sid, err = currentUserSID()
		if err == nil && !windows.EqualSid(user.User.Sid, sid) {
			err = errors.New("controller pipe belongs to another Windows user")
		}
	}
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	return &controllerProcess{PID: int(pid), handle: handle}, nil
}

func (p *controllerProcess) Close() { _ = windows.CloseHandle(p.handle) }

func controllerCommandLine(handle windows.Handle) ([]string, error) {
	// This legacy-only identity check fails closed if the native query becomes
	// unavailable. No WMI helper, shell command, or unbounded allocation is used.
	data := make([]byte, 64<<10)
	var size uint32
	if err := windows.NtQueryInformationProcess(handle, windows.ProcessCommandLineInformation, unsafe.Pointer(&data[0]), uint32(len(data)), &size); err != nil {
		return nil, err
	}
	if size < uint32(unsafe.Sizeof(windows.NTUnicodeString{})) || size > uint32(len(data)) {
		return nil, errors.New("invalid controller command line size")
	}
	value := (*windows.NTUnicodeString)(unsafe.Pointer(&data[0]))
	start := uintptr(unsafe.Pointer(&data[0]))
	position := uintptr(unsafe.Pointer(value.Buffer))
	length := uintptr(value.Length)
	if length%2 != 0 || position < start || position-start > uintptr(size) || length > uintptr(size)-(position-start) {
		return nil, errors.New("invalid controller command line buffer")
	}
	text := windows.UTF16ToString(unsafe.Slice(value.Buffer, int(length/2)))
	runtime.KeepAlive(data)
	return windows.DecomposeCommandLine(text)
}

func (p *controllerProcess) Verify(ctx context.Context, health controllerHealth, opts uiOptions) error {
	if health.PID != p.PID {
		return errors.New("pinned controller PID mismatch")
	}
	args, err := controllerCommandLine(p.handle)
	if err != nil {
		return err
	}
	if err := validateControllerArguments(args, health, opts); err != nil {
		return err
	}
	name := make([]uint16, 32768)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(p.handle, 0, &name[0], &size); err != nil {
		return err
	}
	executable := windows.UTF16ToString(name[:size])
	if health.Executable != "" && !sameControllerPath(health.Executable, executable) {
		return errors.New("controller executable does not match private health")
	}
	if err := verifyControllerRelease(executable, opts.StateDir); err != nil {
		return err
	}
	if err := verifyControllerVersion(ctx, executable, health.Version); err != nil {
		return err
	}
	p.executable = executable
	return nil
}

func (p *controllerProcess) StopLegacy(stateDir string) error {
	if err := verifyControllerRelease(p.executable, stateDir); err != nil {
		return err
	}
	return windows.TerminateProcess(p.handle, 0)
}

func (p *controllerProcess) Wait(ctx context.Context) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		status, err := windows.WaitForSingleObject(p.handle, 50)
		if err != nil {
			return err
		}
		if status == windows.WAIT_OBJECT_0 {
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
