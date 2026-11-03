package localagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

func readPrivate(path string, limit int64) ([]byte, error) {
	f, err := openPrivate(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("local agent state exceeds its limit")
	}
	return b, err
}
func atomicWrite(path string, data []byte) error {
	if err := checkDestination(path); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".write-"+id())
	f, err := openPrivate(tmp, true)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = checkDestination(path); err != nil {
		return err
	}
	return replaceFile(tmp, path)
}
func checkDestination(path string) error {
	f, err := openPrivate(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}
