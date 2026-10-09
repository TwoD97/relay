//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Interactive shells put foreground jobs in separate process groups. Identify
// the PTY's Linux session, then pin each process before signalling it. The caller
// keeps the original child unreaped until cleanup finishes; a stale or reaped
// identity must never authorize signals to a subsequently reused PID or group.
func signalSession(identity *ProcessIdentity, signal syscall.Signal) error {
	if identity == nil || identity.PID <= 1 || identity.StartTime == 0 {
		return errors.New("session has no safe process identity")
	}
	if signal != syscall.SIGTERM && signal != syscall.SIGKILL {
		return errors.New("unsupported session termination signal")
	}
	boot, err := bootIdentity()
	if err != nil || boot != identity.BootID {
		return errors.New("session boot identity cannot be verified")
	}
	verifyLeader := func() error {
		leader, err := inspectProcess(identity.PID)
		if err != nil || leader.startTime != identity.StartTime || leader.session != identity.PID || leader.uid != os.Geteuid() || leader.parentPID != os.Getpid() {
			return errors.New("original session leader is no longer an unreaped child of this runtime")
		}
		return nil
	}
	if err := verifyLeader(); err != nil {
		return err
	}
	signalProcess := func(facts processFacts) error {
		fd, err := unix.PidfdOpen(facts.pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		current, err := inspectProcess(facts.pid)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || current.startTime != facts.startTime || current.session != identity.PID || current.uid != os.Geteuid() {
			return errors.New("session member identity changed before signalling")
		}
		if err := verifyLeader(); err != nil {
			return err
		}
		if err := unix.PidfdSendSignal(fd, unix.Signal(signal), nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		return nil
	}
	// Re-scan after KILL to catch a child whose fork was in flight during the
	// preceding census. Never claim complete cleanup if bounded sweeps leave a
	// live member, a different UID, or a process we cannot safely signal.
	var failures []error
	for sweep := 0; sweep < 4; sweep++ {
		if err := verifyLeader(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		members := 0
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 {
				continue
			}
			facts, err := inspectProcess(pid)
			if err != nil || facts.session != identity.PID || facts.state == 'Z' || facts.state == 'X' {
				continue
			}
			members++
			if members > 4096 {
				return errors.Join(append(failures, errors.New("session exceeds the bounded termination process limit"))...)
			}
			if facts.uid != os.Geteuid() {
				failures = append(failures, fmt.Errorf("session process %d has a different owner", pid))
				continue
			}
			if err := signalProcess(facts); err != nil {
				failures = append(failures, fmt.Errorf("signal session process %d: %w", pid, err))
			}
		}
		if signal == syscall.SIGTERM || members == 0 {
			return errors.Join(failures...)
		}
		if sweep < 3 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return errors.Join(append(failures, errors.New("session termination was requested but live process exit could not be confirmed"))...)
}

func resizePTY(file *os.File, cols, rows uint16) error {
	raw, err := file.SyscallConn()
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
