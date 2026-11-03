//go:build linux

package localagent

import (
	"encoding/json"
	terminal "github.com/TwoD97/relay/internal/runtime"
	"os/exec"
)

func startProcess(c command, cols, rows int) (process, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	return terminal.StartOwnedPTY(cmd, cols, rows)
}
func recoverProcess(raw json.RawMessage) string { return terminal.RecoverOwnedPTY(raw) }
