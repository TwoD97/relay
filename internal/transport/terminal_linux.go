//go:build linux && !relay_ssh_native

package transport

import (
	"os"
	"syscall"
	"unsafe"

	"github.com/creack/pty"
)

// creack/pty performs ioctls through File.Fd, putting the descriptor in blocking
// mode. Rewrap a nonblocking duplicate so Go registers it with the poller and
// terminal input deadlines and Close both interrupt pending I/O.
func pollableTerminal(original *os.File) (*os.File, error) {
	defer original.Close()
	fd, err := syscall.Dup(int(original.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "relay-ssh-terminal"), nil
}

func resizeTerminal(terminal *os.File, cols, rows uint16) error {
	raw, err := terminal.SyscallConn()
	if err != nil {
		return err
	}
	size := pty.Winsize{Cols: cols, Rows: rows}
	var ioctlErr syscall.Errno
	err = raw.Control(func(fd uintptr) {
		_, _, ioctlErr = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
	})
	if err != nil {
		return err
	}
	if ioctlErr != 0 {
		return ioctlErr
	}
	return nil
}
