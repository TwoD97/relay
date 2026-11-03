package controller

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func TestWorkspaceWindowsCapabilityExplicitPrivateOwner(t *testing.T) {
	s, _ := workspaceFixture(t)
	path, err := s.PrepareWorkspaceAgentCapability(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Dir(path), path} {
		sd, err := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !windows.EqualSid(owner, user.User.Sid) {
			t.Fatal("capability relied on token default owner")
		}
		acl, _, err := sd.DACL()
		if err != nil || acl == nil || acl.AceCount != 2 {
			t.Fatal("capability ACL must contain only user and SYSTEM")
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("capability ACL must be protected")
		}
		seenUser, seenSystem := false, false
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 || ace.Mask != (windows.STANDARD_RIGHTS_REQUIRED|windows.SYNCHRONIZE|0x1ff) {
				t.Fatal("unexpected capability permissions")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			switch {
			case windows.EqualSid(sid, user.User.Sid):
				seenUser = true
			case windows.EqualSid(sid, system):
				seenSystem = true
			default:
				t.Fatal("capability allowed another Windows identity")
			}
		}
		runtime.KeepAlive(sd)
		if !seenUser || !seenSystem {
			t.Fatal("capability ACL missing required principal")
		}
	}
}
