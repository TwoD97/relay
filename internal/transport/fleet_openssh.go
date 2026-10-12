//go:build !windows && !relay_ssh_native

package transport

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func (c *Connection) FleetEndpoint(ctx context.Context) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{"-G", "-o", "CanonicalizeHostname=no", "-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none"}
	if c.cfg.Port != 0 {
		args = append(args, "-p", strconv.Itoa(c.cfg.Port))
	}
	args = append(args, "--", c.cfg.Target)
	cmd := exec.CommandContext(ctx, c.sshPath, args...)
	cmd.WaitDelay = time.Second
	output := &boundedBuffer{limit: 128 << 10}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil || output.truncated {
		return "", 0, errors.New("could not resolve saved SSH configuration to a portable endpoint")
	}
	var user, host string
	var port int
	scan := bufio.NewScanner(strings.NewReader(string(output.Bytes())))
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), " ")
		if !ok {
			continue
		}
		switch key {
		case "hostname":
			host = strings.TrimSpace(value)
		case "user":
			user = strings.TrimSpace(value)
		case "port":
			port, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if scan.Err() != nil {
		return "", 0, errors.New("SSH configuration response exceeded its limit")
	}
	return fleetEndpoint(user, host, port)
}
