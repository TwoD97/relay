package personalwhatsapp

import (
	"errors"
	"os"
)

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Personal WhatsApp storage is not a private directory")
	}
	return os.Chmod(path, 0700)
}
func linked(info os.FileInfo) bool { return info.Mode()&os.ModeSymlink != 0 }
