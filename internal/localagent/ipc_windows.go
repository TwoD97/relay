//go:build windows

package localagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"net"
	"path/filepath"
	"strings"
)

func Endpoint(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(sid.String() + "\x00" + strings.ToLower(filepath.Clean(dir))))
	return fmt.Sprintf(`\\.\pipe\Relay-localagent-%x`, key[:20]), nil
}
func DialContext(ctx context.Context, dir string) (net.Conn, error) {
	endpoint, err := Endpoint(dir)
	if err != nil {
		return nil, err
	}
	conn, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if err = verifyPeer(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func verifyPeer(conn net.Conn) error {
	file, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("cannot verify the Windows local agent pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(file.Fd()), &pid); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	if !windows.EqualSid(user.User.Sid, sid) {
		return errors.New("local agent pipe belongs to a different Windows user")
	}
	return nil
}
