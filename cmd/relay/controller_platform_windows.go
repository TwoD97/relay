package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const localRuntimeSupported = false

func defaultDir(name string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		panic(err)
	}
	// Keep state outside NSIS's default %LOCALAPPDATA%\Relay install directory.
	return filepath.Join(base, "RelayData", name)
}
func socketPath(dir string) string { return filepath.Join(dir, "run", "daemon.sock") }

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func controllerSocket(dir string) string {
	// Bind the namespace to both the user's SID and the canonical state path.
	// The pipe DACL and peer-token check are the actual security boundaries.
	sid, err := currentUserSID()
	if err != nil {
		return ""
	}
	key := sha256.Sum256([]byte(sid.String() + "\x00" + strings.ToLower(filepath.Clean(dir))))
	return fmt.Sprintf(`\\.\pipe\Relay-controller-%x`, key[:20])
}

func validateControllerOptions(opts uiOptions) error {
	if opts.Local {
		return errors.New("local terminal runtimes require Linux; on Windows connect an SSH host or pass --local=false")
	}
	if controllerSocket(opts.StateDir) == "" {
		return errors.New("could not identify the current Windows user")
	}
	return nil
}

func detachController(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}
}

func controllerUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func privateSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
}

func secureOwnedHandle(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	if owner == nil || !windows.EqualSid(owner, sid) {
		return errors.New("controller state must be owned by the current Windows user")
	}
	private, err := privateSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := private.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func openOwnedState(path string, directory bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC)
	creation := uint32(windows.OPEN_ALWAYS)
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		access = windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_READ_ATTRIBUTES
		creation = windows.OPEN_EXISTING
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, creation, flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && (info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory) {
		err = errors.New("controller state must be a regular owned file or directory, not a link")
	}
	if err == nil {
		err = secureOwnedHandle(handle)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateControllerDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	f, err := openOwnedState(path, true)
	if err != nil {
		return err
	}
	return f.Close()
}

func prepareControllerDirectory(dir string) error {
	if err := privateControllerDirectory(dir); err != nil {
		return err
	}
	return privateControllerDirectory(filepath.Join(dir, "run"))
}

func privateControllerFile(path string) (*os.File, error) {
	f, err := openOwnedState(path, false)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
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
	var overlap windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlap)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errControllerRunning
		}
		return nil, err
	}
	return f, nil
}

func dialControllerControl(ctx context.Context, dir string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, controllerSocket(dir))
	if err != nil {
		return nil, err
	}
	if err := verifyControllerPeer(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func verifyControllerPeer(conn net.Conn) error {
	file, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("cannot verify the Windows controller pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(file.Fd()), &pid); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	if !windows.EqualSid(user.User.Sid, sid) {
		return errors.New("controller pipe belongs to a different Windows user")
	}
	return nil
}

func listenControllerControl(dir string) (net.Listener, func(), error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	listener, err := winio.ListenPipe(controllerSocket(dir), &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid.String() + ")",
		InputBufferSize:    64 << 10, OutputBufferSize: 64 << 10,
	})
	return listener, func() {}, err
}
