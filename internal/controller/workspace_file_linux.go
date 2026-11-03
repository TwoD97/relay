package controller

import (
	"os"
	"syscall"
)

func openWorkspaceRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func makeWorkspaceCapabilityDirectory(path string) error { return os.Mkdir(path, 0700) }
func createWorkspaceCapability(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
}
