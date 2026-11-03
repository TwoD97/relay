package localagent

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockHarnessInstall(path string) (*os.File, error) {
	file, err := openPrivate(path, false)
	if errors.Is(err, os.ErrNotExist) {
		file, err = openPrivate(path, true)
		if errors.Is(err, os.ErrExist) {
			file, err = openPrivate(path, false)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, errors.New("harness maintenance is already running")
	}
	return file, nil
}
