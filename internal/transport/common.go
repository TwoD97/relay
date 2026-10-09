package transport

import (
	"bytes"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

const (
	maxReplay        = 256 << 10
	maxCommandOutput = 1 << 20
	commandTimeout   = 2 * time.Minute
)

// Config accepts the same hostname, user@hostname, or configured alias as ssh.
// Port zero respects the user's SSH configuration. ControlPath is optional;
// when supplied it must have a private, user-owned parent directory.
type Config struct {
	Target      string
	Port        int
	ControlPath string
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func ValidateTarget(target string) error {
	if len(target) == 0 || len(target) > 320 || strings.Count(target, "@") > 1 {
		return errors.New("enter an SSH alias, hostname, or user@hostname")
	}
	host := target
	if user, rest, ok := strings.Cut(target, "@"); ok {
		if !namePattern.MatchString(user) {
			return errors.New("invalid SSH username")
		}
		host = rest
	}
	if namePattern.MatchString(host) {
		return nil
	}
	if address, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil {
		// Square brackets must either both be present or both be absent.
		// netip permits arbitrary zone text, but SSH may interpolate a zone into
		// a user's configured ProxyCommand. Keep zones to safe interface names.
		if strings.HasPrefix(host, "[") == strings.HasSuffix(host, "]") && (address.Zone() == "" || namePattern.MatchString(address.Zone())) {
			return nil
		}
	}
	return errors.New("invalid SSH host: use a hostname, IP address, or configured alias")
}

func ValidatePort(port int) error {
	if port < 0 || port > 65535 {
		return errors.New("SSH port must be between 1 and 65535, or zero for the SSH configuration default")
	}
	return nil
}

func quoteShell(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// boundedBuffer retains the beginning of command output without ever blocking
// its producer. exec serializes writes when stdout/stderr share the same writer.
type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
func (b *boundedBuffer) Len() int      { return b.buffer.Len() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
