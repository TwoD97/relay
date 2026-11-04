package whatsappbridge

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
func parseTimestamp(value string) (time.Time, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, errors.New("invalid message time")
	}
	return time.Unix(n, 0).UTC(), nil
}
func owned(info os.FileInfo, dir bool) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0 && ((dir && info.IsDir()) || (!dir && info.Mode().IsRegular()))
}
func privateRead(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !owned(info, false) {
		return nil, errors.New("not a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("private file exceeds limit")
	}
	return data, nil
}
func privateWrite(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && !owned(info, false) {
		return errors.New("queue target is not private")
	} else if err != nil && !isNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".queue-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func lockDirectory(path string) (func(), error) {
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !owned(info, true) {
		return nil, errors.New("Bridge state must be a private owned directory")
	}
	f, err := os.OpenFile(filepath.Join(path, "bridge.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !owned(info, false) {
		f.Close()
		return nil, errors.New("Bridge lock must be private")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("Another bridge owns this directory")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
