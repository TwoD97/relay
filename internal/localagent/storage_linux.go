//go:build linux

package localagent

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("local agent state must be owned by this user")
	}
	return f.Chmod(0700)
}
func openPrivate(path string, create bool) (*os.File, error) {
	flags := os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL | unix.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
		err = errors.New("local agent state must be a regular owned file")
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func replaceFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	return nil
}
