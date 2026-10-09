package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func controllerSecurity(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd
}

func assertPrivateControllerSecurity(t *testing.T, path string) {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sd := controllerSecurity(t, path)
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, sid) {
		t.Fatal("new controller object did not receive the current user's owner SID", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 || !strings.Contains(sd.String(), "D:P") || !strings.Contains(sd.String(), ";;;"+sid.String()+")") || !strings.Contains(sd.String(), ";;;SY)") {
		t.Fatal("new controller object did not receive a protected user/SYSTEM-only DACL", path, err)
	}
}

func TestWindowsControllerCreationSetsExplicitOwnerAndPrivateDACL(t *testing.T) {
	attributes, err := privateSecurityAttributes()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := attributes.SecurityDescriptor.Owner()
	sid, sidErr := currentUserSID()
	if err != nil || sidErr != nil || owner == nil || !windows.EqualSid(owner, sid) {
		t.Fatal("creation descriptor relies on the token's default owner", err, sidErr)
	}
	if attributes.InheritHandle != 0 || attributes.Length != uint32(unsafe.Sizeof(windows.SecurityAttributes{})) {
		t.Fatal("invalid creation attributes or inheritable private handles")
	}
	root := t.TempDir() // On an elevated runner this can be owned by Administrators.
	dir := filepath.Join(root, "missing parent", "private state")
	lock, err := lockControllerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	staging, err := privateControllerTempDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(staging, "controller.log")
	f, err := privateControllerFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, path := range []string{filepath.Dir(dir), dir, filepath.Join(dir, "run"), filepath.Join(dir, "run", "controller.lock"), staging, file} {
		assertPrivateControllerSecurity(t, path)
	}
}

func TestWindowsControllerDoesNotAdoptAdministratorOwnedState(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// Administrator ownership is deliberately different from ownership by the
	// current user, even when that user's elevated token can modify the object.
	sd, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for _, directory := range []bool{true, false} {
		name := "file"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing state")
			utf16, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			if directory {
				err = windows.CreateDirectory(utf16, attributes)
			} else {
				var handle windows.Handle
				handle, err = windows.CreateFile(utf16, windows.GENERIC_WRITE, 0, attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err == nil {
					windows.CloseHandle(handle)
				}
			}
			runtime.KeepAlive(attributes)
			if errors.Is(err, windows.ERROR_INVALID_OWNER) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Skip("creating an Administrator-owned adversarial fixture requires an elevated token")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := controllerSecurity(t, path).String()
			if directory {
				err = privateControllerDirectory(path)
			} else {
				var f *os.File
				f, err = privateControllerFile(path)
				if f != nil {
					f.Close()
				}
			}
			if err == nil || !strings.Contains(err.Error(), "owned by the current Windows user") {
				t.Fatal("accepted another owner's existing state", err)
			}
			if after := controllerSecurity(t, path).String(); after != before {
				t.Fatal("rejected existing state had its owner or DACL changed")
			}
		})
	}
}
