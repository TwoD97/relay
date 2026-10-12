package fleetregistry

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func owned(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("fleet state is not owned by the current user")
	}
	return nil
}

func lock(ctx context.Context, path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || owned(info) != nil {
		f.Close()
		return nil, errors.New("fleet lock is not a private owned regular file")
	}
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
		} else if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
