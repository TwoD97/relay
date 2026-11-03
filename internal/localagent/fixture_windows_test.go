package localagent

import (
	"bufio"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func fixtureConsoleSize() (int, int) {
	handle, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0, 0
	}
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(handle, &info) != nil {
		return 0, 0
	}
	return int(info.Window.Right - info.Window.Left + 1), int(info.Window.Bottom - info.Window.Top + 1)
}
func pinFixtureProcess(pid int) (func() bool, func(), error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return nil, nil, err
	}
	return func() bool {
		result, e := windows.WaitForSingleObject(handle, 0)
		return e == nil && result == uint32(windows.WAIT_TIMEOUT)
	}, func() { windows.CloseHandle(handle) }, nil
}

func TestWindowsDaemonCrashClosesOwnedJob(t *testing.T) {
	exe, _ := os.Executable()
	host := exec.Command(exe)
	host.Env = setEnv(os.Environ(), "LOCALAGENT_CRASH_HOST", "1")
	stdout, err := host.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Process.Kill(); _ = host.Wait() }()
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := ansi.Strip(scanner.Text())
			if strings.Contains(line, "CHILD ") {
				lines <- line
				return
			}
		}
	}()
	var childID int
	select {
	case line := <-lines:
		start := strings.Index(line, "CHILD ")
		fmt.Sscanf(line[start:], "CHILD %d", &childID)
	case <-time.After(5 * time.Second):
		t.Fatal("crash host did not launch its owned child")
	}
	alive, closeHandle, err := pinFixtureProcess(childID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle()
	if err = host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = host.Wait()
	until := time.Now().Add(3 * time.Second)
	for alive() && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if alive() {
		t.Fatal("ConPTY process survived owner daemon crash")
	}
}
