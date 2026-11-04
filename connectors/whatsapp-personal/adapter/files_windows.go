package adapter

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func privateDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sid := user.User.Sid
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	var create func(string) error
	create = func(path string) error {
		p, e := windows.UTF16PtrFromString(path)
		if e != nil {
			return e
		}
		e = windows.CreateDirectory(p, attributes)
		if errors.Is(e, windows.ERROR_ALREADY_EXISTS) {
			return nil
		}
		if errors.Is(e, windows.ERROR_PATH_NOT_FOUND) && filepath.Dir(path) != path {
			if e = create(filepath.Dir(path)); e != nil {
				return e
			}
			e = windows.CreateDirectory(p, attributes)
		}
		return e
	}
	if err = create(path); err != nil {
		return err
	}
	runtime.KeepAlive(attributes)
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("Personal WhatsApp storage must be a regular private directory")
	}
	security, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := security.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, sid) {
		return errors.New("Personal WhatsApp storage must be owned by the current Windows user")
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func linked(info os.FileInfo) bool {
	value, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return info.Mode()&os.ModeSymlink != 0 || (ok && value.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0)
}
