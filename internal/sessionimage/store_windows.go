package sessionimage

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"runtime"
	"unsafe"
)

func imageSecurity() (*windows.SecurityAttributes, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	sid := user.User.Sid
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return nil, nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, sid, nil
}
func privateDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, _, err := imageSecurity()
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(name, attributes)
	runtime.KeepAlive(attributes)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	file, err := openImageObject(path, false, true)
	if err != nil {
		return err
	}
	return file.Close()
}
func openOwned(path string, create bool) (*os.File, error) {
	return openImageObject(path, create, false)
}
func openImageObject(path string, create, directory bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes, sid, err := imageSecurity()
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_READ | windows.READ_CONTROL | windows.WRITE_DAC)
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	creation := uint32(windows.OPEN_EXISTING)
	if create {
		access |= windows.GENERIC_WRITE
		creation = windows.CREATE_NEW
	}
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
		access = windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.WRITE_DAC
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, attributes, creation, flags, 0)
	runtime.KeepAlive(attributes)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && (info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory) {
		err = errors.New("image storage must not contain links or devices")
	}
	if err == nil {
		var sd *windows.SECURITY_DESCRIPTOR
		sd, err = windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err == nil {
			owner, _, e := sd.Owner()
			err = e
			if err == nil && (owner == nil || !windows.EqualSid(owner, sid)) {
				err = errors.New("image storage belongs to another Windows user")
			}
		}
	}
	if err == nil {
		dacl, _, e := attributes.SecurityDescriptor.DACL()
		err = e
		if err == nil {
			err = windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
