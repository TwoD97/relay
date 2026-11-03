package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func openAgentFile(path string) (*os.File, error) { return stageReadFile(path) }
func verifyAgentFileOwner(f *os.File, _ os.FileInfo) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	if owner == nil || !windows.EqualSid(owner, sid) {
		return errors.New("Mastermind capability must be owned by this user")
	}
	return nil
}
