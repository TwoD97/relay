//go:build windows

package localagent

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"
)

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func privateSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	// Elevated tokens may default to Administrators as the owner. Specify the
	// user's SID at creation, without taking ownership of any existing object.
	return windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
}

func privateSecurityAttributes() (*windows.SecurityAttributes, error) {
	sd, err := privateSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func secureOwnedHandle(handle windows.Handle) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
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
		return errors.New("local agent state must be owned by the current Windows user")
	}
	private, err := privateSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := private.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func openOwnedState(path string, directory bool) (*os.File, error) {
	return openOwnedStateCreation(path, directory, windows.OPEN_ALWAYS)
}

func openOwnedStateCreation(path string, directory bool, creation uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC)
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		access = windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_READ_ATTRIBUTES
		creation = windows.OPEN_EXISTING
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	attributes, err := privateSecurityAttributes()
	if err != nil {
		return nil, err
	}
	// Windows only applies these attributes to a newly created file. Existing
	// files still have to pass the unchanged owner and reparse-point checks.
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, attributes, creation, flags, 0)
	runtime.KeepAlive(attributes)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && (info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory) {
		err = errors.New("local agent state must be a regular owned file or directory, not a link")
	}
	if err == nil {
		err = secureOwnedHandle(handle)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func privateDir(path string) error {
	if err := createPrivateControllerParents(filepath.Clean(path)); err != nil {
		return err
	}
	f, err := openOwnedState(path, true)
	if err != nil {
		return err
	}
	return f.Close()
}

func createPrivateControllerDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := privateSecurityAttributes()
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, attributes)
	runtime.KeepAlive(attributes)
	return err
}

// Like MkdirAll, leave existing ancestors alone. Every missing component gets
// an explicit owner at creation; the requested state directory is then opened
// without following a final reparse point and its owner is verified.
func createPrivateControllerParents(path string) error {
	err := createPrivateControllerDirectory(path)
	if err == nil || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return err
	}
	if err := createPrivateControllerParents(parent); err != nil {
		return err
	}
	err = createPrivateControllerDirectory(path)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil
	}
	return err
}

func openPrivate(path string, create bool) (*os.File, error) {
	creation := uint32(windows.OPEN_EXISTING)
	if create {
		creation = windows.CREATE_NEW
	}
	return openOwnedStateCreation(path, false, creation)
}
func replaceFile(from, to string) error {
	a, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
