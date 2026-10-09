package main

import (
	"errors"
	"os"
	"syscall"
)

const stagedControllerFilename = "relay-controller"

func stageCreateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
}

func stageReadFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("controller release files must be regular files, not links")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
