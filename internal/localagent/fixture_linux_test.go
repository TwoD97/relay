package localagent

import (
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"os"
)

func fixtureConsoleSize() (int, int) {
	size, err := pty.GetsizeFull(os.Stdin)
	if err != nil {
		return 0, 0
	}
	return int(size.Cols), int(size.Rows)
}
func pinFixtureProcess(pid int) (func() bool, func(), error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, nil, err
	}
	return func() bool {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, e := unix.Poll(poll, 0)
		return e == nil && n == 0
	}, func() { unix.Close(fd) }, nil
}
