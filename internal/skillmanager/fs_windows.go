package skillmanager

import (
	"golang.org/x/sys/windows"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

func linked(m os.FileInfo) bool {
	v, ok := m.Sys().(*syscall.Win32FileAttributeData)
	return m.Mode()&os.ModeSymlink != 0 || (ok && v.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0)
}
func attributes() (*windows.SecurityAttributes, error) {
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return nil, e
	}
	sid := u.User.Sid.String()
	sd, e := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)")
	if e != nil {
		return nil, e
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}
func makeDir(path string) error {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return e
	}
	a, e := attributes()
	if e != nil {
		return e
	}
	e = windows.CreateDirectory(p, a)
	runtime.KeepAlive(a)
	return e
}
func file(path string, access, creation uint32, private bool) (*os.File, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	var a *windows.SecurityAttributes
	if private {
		a, e = attributes()
		if e != nil {
			return nil, e
		}
	}
	h, e := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, a, creation, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	runtime.KeepAlive(a)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(h), path), nil
}
func createFile(path string, _ os.FileMode) (*os.File, error) {
	return file(path, windows.GENERIC_WRITE, windows.CREATE_NEW, true)
}
func openRead(path string) (*os.File, error) {
	return file(path, windows.GENERIC_READ, windows.OPEN_EXISTING, false)
}
func replaceFile(from, to string) error {
	f, e := windows.UTF16PtrFromString(from)
	if e != nil {
		return e
	}
	t, e := windows.UTF16PtrFromString(to)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
func syncDir(string) error { return nil }
func lockFile(path string) (func(), error) {
	f, e := file(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.CREATE_NEW, true)
	if os.IsExist(e) {
		f, e = file(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_EXISTING, false)
	}
	if e != nil {
		return nil, e
	}
	m, e := f.Stat()
	if e != nil || !m.Mode().IsRegular() || linked(m) {
		f.Close()
		return nil, os.ErrPermission
	}
	var over windows.Overlapped
	if e = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &over); e != nil {
		f.Close()
		return nil, e
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &over); f.Close() }, nil
}

func renameNew(from, to string) error {
	f, e := windows.UTF16PtrFromString(from)
	if e != nil {
		return e
	}
	t, e := windows.UTF16PtrFromString(to)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_WRITE_THROUGH)
}
