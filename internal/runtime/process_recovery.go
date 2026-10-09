//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Identity is persisted before a session starts accepting terminal input. PID
// alone is never sufficient evidence to signal a process after a runtime crash.
type ProcessIdentity struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"startTime"`
	BootID    string `json:"bootId"`
}

type SessionRecovery struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type processFacts struct {
	pid, parentPID, session, uid int
	startTime                    uint64
	state                        byte
}

func bootIdentity() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if len(value) != 36 {
		return "", errors.New("invalid Linux boot identity")
	}
	return value, nil
}

func inspectProcess(pid int) (processFacts, error) {
	var result processFacts
	if pid <= 1 {
		return result, errors.New("unsafe process ID")
	}
	dir := "/proc/" + strconv.Itoa(pid)
	status, err := os.ReadFile(dir + "/status")
	if err != nil {
		return result, err
	}
	uid := -1
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 5 && fields[0] == "Uid:" {
			uid, err = strconv.Atoi(fields[2]) // effective UID
			break
		}
	}
	// /proc/<pid>'s filesystem owner may become root when a same-user process
	// disables core dumps. The kernel's effective UID field is the real boundary.
	if err != nil || uid < 0 {
		return result, errors.New("process owner is unavailable")
	}
	data, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return result, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return result, errors.New("invalid process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return result, errors.New("incomplete process identity")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil {
		return result, err
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return result, err
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return result, err
	}
	return processFacts{pid: pid, parentPID: parentPID, session: session, uid: uid, startTime: started, state: fields[0][0]}, nil
}

func identifyProcess(pid int) (*ProcessIdentity, error) {
	boot, err := bootIdentity()
	if err != nil {
		return nil, err
	}
	facts, err := inspectProcess(pid)
	if err != nil {
		return nil, err
	}
	if facts.session != pid || facts.uid != os.Geteuid() {
		return nil, errors.New("session leader identity does not match the runtime")
	}
	return &ProcessIdentity{PID: pid, StartTime: facts.startTime, BootID: boot}, nil
}

type pinnedProcess struct {
	facts      processFacts
	fd         int
	wasStopped bool
}

func unverifiedRecovery(reason string) *SessionRecovery {
	return &SessionRecovery{Status: "unverified", Detail: "The runtime stopped unexpectedly; this terminal cannot be resumed. " + reason + " Remaining processes could not be verified; inspect this host before starting duplicate work."}
}

func recoverInterruptedProcess(identity *ProcessIdentity) *SessionRecovery {
	if identity == nil || identity.PID <= 1 || identity.StartTime == 0 || len(identity.BootID) != 36 {
		return unverifiedRecovery("The saved session has no usable process identity.")
	}
	boot, err := bootIdentity()
	if err != nil {
		return unverifiedRecovery("The current host boot identity is unavailable.")
	}
	if boot != identity.BootID {
		return &SessionRecovery{Status: "host-restarted", Detail: "This host restarted after the session was saved. Its terminal process cannot be resumed; retained output is available."}
	}
	leader, err := inspectProcess(identity.PID)
	if err != nil || leader.startTime != identity.StartTime || leader.session != identity.PID || leader.uid != os.Geteuid() || leader.state == 'Z' {
		return unverifiedRecovery("The original session leader is missing or its identity has changed; no process was signalled.")
	}
	pins := make(map[int]*pinnedProcess)
	killing := false
	defer func() {
		for _, pin := range pins {
			// Abort restores only processes that this recovery attempt stopped.
			if !killing && !pin.wasStopped {
				_ = unix.PidfdSendSignal(pin.fd, unix.SIGCONT, nil, 0)
			}
			_ = unix.Close(pin.fd)
		}
	}()
	pin := func(facts processFacts) error {
		fd, err := unix.PidfdOpen(facts.pid, 0)
		if err != nil {
			return err
		}
		current, err := inspectProcess(facts.pid)
		if err != nil || current.startTime != facts.startTime || current.session != identity.PID || current.uid != os.Geteuid() {
			_ = unix.Close(fd)
			return errors.New("process identity changed before pinning")
		}
		if err := unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0); err != nil {
			_ = unix.Close(fd)
			return err
		}
		pins[facts.pid] = &pinnedProcess{facts: current, fd: fd, wasStopped: current.state == 'T' || current.state == 't'}
		return nil
	}
	if err := pin(leader); err != nil {
		return unverifiedRecovery("Safe process signalling is unavailable; no cleanup was performed.")
	}
	// Freeze a bounded, stable census while the verified leader still exists.
	// No numeric process-group signal is used, including after the leader exits.
	stable := false
	for attempt := 0; attempt < 4; attempt++ {
		current, err := inspectProcess(identity.PID)
		if err != nil || current.startTime != identity.StartTime || current.session != identity.PID || current.uid != os.Geteuid() {
			return unverifiedRecovery("The session leader changed during recovery.")
		}
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return unverifiedRecovery("The session process list is unavailable.")
		}
		added, pending := false, false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 {
				continue
			}
			facts, err := inspectProcess(pid)
			if err != nil || facts.session != identity.PID || facts.state == 'Z' {
				continue
			}
			if facts.uid != os.Geteuid() {
				return unverifiedRecovery("A session process has a different owner.")
			}
			if known := pins[pid]; known != nil {
				if facts.startTime != known.facts.startTime {
					return unverifiedRecovery("A process identity changed during recovery.")
				}
				pending = pending || (facts.state != 'T' && facts.state != 't')
				continue
			}
			if len(pins) >= 256 {
				return unverifiedRecovery("The session exceeds the bounded recovery process limit.")
			}
			if err := pin(facts); err != nil {
				return unverifiedRecovery("A session process could not be pinned safely.")
			}
			added = true
		}
		if !added && !pending {
			stable = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !stable {
		return unverifiedRecovery("The session process list did not stabilize safely.")
	}
	killing = true
	poll := make([]unix.PollFd, 0, len(pins))
	killFailed := false
	for _, pin := range pins {
		if err := unix.PidfdSendSignal(pin.fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			killFailed = true
			if !pin.wasStopped {
				_ = unix.PidfdSendSignal(pin.fd, unix.SIGCONT, nil, 0)
			}
			continue
		}
		poll = append(poll, unix.PollFd{Fd: int32(pin.fd), Events: unix.POLLIN})
	}
	remaining := len(poll)
	deadline := time.Now().Add(500 * time.Millisecond)
	for remaining > 0 && time.Now().Before(deadline) {
		if _, err := unix.Poll(poll, 25); err != nil && !errors.Is(err, unix.EINTR) {
			return unverifiedRecovery("Process exit could not be confirmed.")
		}
		for i := range poll {
			if poll[i].Fd >= 0 && poll[i].Revents&unix.POLLIN != 0 {
				poll[i].Fd = -1
				remaining--
			}
		}
	}
	if killFailed || remaining != 0 {
		return unverifiedRecovery("Stop was requested for the identified processes, but exit could not be confirmed.")
	}
	return &SessionRecovery{Status: "stopped", Detail: fmt.Sprintf("The runtime stopped unexpectedly. Relay stopped %d verified process(es) left in this terminal's Linux session. This terminal cannot be resumed; retained output is available. Detached descendants are not covered.", len(pins))}
}
