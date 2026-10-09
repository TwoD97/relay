package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

const stagedControllerFilename = "relay-controller.exe"

func stageCreateFile(path string) (*os.File, error) {
	return openOwnedStateCreation(path, false, windows.CREATE_NEW)
}

func stageReadFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err == nil && (info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY)) != 0 {
		err = errors.New("controller release files must be regular files, not links")
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
