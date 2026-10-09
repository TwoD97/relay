//go:build !windows && !relay_ssh_native

package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestTargetValidation(t *testing.T) {
	for _, target := range []string{"work", "alice@work.example", "user-name@192.0.2.1", "2001:db8::1", "a@[2001:db8::1]", "a_b@ssh-alias", "fe80::1%eth0"} {
		if err := ValidateTarget(target); err != nil {
			t.Errorf("valid target %q: %v", target, err)
		}
	}
	for _, target := range []string{"", "-oProxyCommand=whoami", "x;touch /tmp/file", "$(whoami)", "a@b@c", "a\nb", "a\x00b", "@host", "user@", "ssh://host", "host:22", "user@[2001:db8::1", "user@2001:db8::1]", "bad user@host", "user@/tmp/socket", "fe80::1%$(id)", "fe80::1%eth0\ncommand", "fe80::1%a b"} {
		if err := ValidateTarget(target); err == nil {
			t.Errorf("accepted unsafe target %q", target)
		}
	}
	for _, port := range []int{-1, 65536, 100000} {
		if ValidatePort(port) == nil {
			t.Errorf("accepted port %d", port)
		}
	}
	for _, port := range []int{0, 1, 22, 65535} {
		if err := ValidatePort(port); err != nil {
			t.Errorf("valid port %d: %v", port, err)
		}
	}
}

func TestShellQuotingSurvivesRemoteShell(t *testing.T) {
	for _, value := range []string{"", "normal", "two words", "'\"`$()\\\n", "$(printf injected); printf injected", "first\nsecond\n"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			output, err := exec.Command("/bin/sh", "-c", "printf '%s' "+quoteShell(value)).Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != value {
				t.Fatalf("shell changed value: got %q, want %q", output, value)
			}
		})
	}
	// OpenSSH joins its command arguments into a shell command before sending.
	// Exercise both that outer shell and the explicit inner /bin/sh invocation.
	script := "printf '%s\\n' \"a'b\"; cat"
	args := commandArgs(Config{Target: "host", ControlPath: "/private/control"}, script)
	command := exec.Command("/bin/sh", "-c", args[len(args)-1])
	command.Stdin = strings.NewReader("payload\x00with\xffbytes")
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "a'b\npayload\x00with\xffbytes" {
		t.Fatalf("script or binary input changed: %q", out)
	}
}

func TestOpenSSHConfigurationEnforcesTrustAndNoFallback(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH is not installed")
	}
	cfg := Config{Target: "test.invalid", Port: 2202, ControlPath: "/tmp/relay-test/control"}
	for _, test := range []struct {
		name string
		args []string
		want map[string]string
	}{
		{"master", masterArgs(cfg), map[string]string{"controlmaster": "true", "controlpersist": "no", "stricthostkeychecking": "ask", "forwardagent": "no", "forwardx11": "no", "permitlocalcommand": "no", "port": "2202"}},
		{"slave", commandArgs(cfg, "true"), map[string]string{"controlmaster": "false", "batchmode": "yes", "stricthostkeychecking": "true", "proxycommand": "false", "forwardagent": "no", "forwardx11": "no", "port": "2202"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"-F", "/dev/null", "-G"}, test.args...)
			out, err := exec.Command(ssh, args...).Output()
			if err != nil {
				t.Fatal(err)
			}
			parsed := make(map[string]string)
			for line := range strings.SplitSeq(string(out), "\n") {
				if key, value, ok := strings.Cut(line, " "); ok {
					parsed[key] = value
				}
			}
			for key, want := range test.want {
				if parsed[key] != want {
					t.Errorf("%s = %q, want %q", key, parsed[key], want)
				}
			}
		})
	}
}

func TestControlSocketMustBePrivateAndUnused(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "c")
	// Unix socket length is intentionally strict; TempDir names vary by runner.
	if len(path) > 100 {
		t.Skip("test path exceeds Unix socket path limit")
	}
	if err := validateControlPath(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if validateControlPath(path) == nil {
		t.Fatal("accepted a publicly accessible control directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if validateControlPath(path) == nil {
		t.Fatal("accepted a preexisting socket path")
	}
	if validateControlPath("relative") == nil {
		t.Fatal("accepted relative control path")
	}
}

func TestReplayAndSlowSubscriberAreBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Connection{ctx: ctx, subs: make(map[chan []byte]struct{})}
	c.publish([]byte("prior"))
	history, output, unsubscribe := c.Subscribe()
	defer unsubscribe()
	if string(history) != "prior" {
		t.Fatalf("replay %q", history)
	}
	c.publish([]byte("next"))
	if string(<-output) != "next" {
		t.Fatal("missing live output")
	}
	for range 65 {
		c.publish(bytes.Repeat([]byte("x"), 16<<10))
	}
	if len(c.history) != maxReplay {
		t.Fatalf("history length %d", len(c.history))
	}
	for range output {
	} // Must close instead of blocking the producer indefinitely.
	if len(c.subs) != 0 {
		t.Fatal("slow subscriber retained")
	}
	if string(history) != "prior" {
		t.Fatal("replay aliases mutable storage")
	}
	c.publish(bytes.Repeat([]byte("z"), 2*maxReplay))
	if len(c.history) != maxReplay || c.history[0] != 'z' {
		t.Fatal("oversized chunk escaped replay bound")
	}
}

func TestVerifiedArtifactRejectsTamperingAndAmbiguousManifest(t *testing.T) {
	dir := t.TempDir()
	name := "relay-linux-amd64"
	data := []byte("verified executable")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(name, data)
	write("SHA256SUMS", []byte(digest+"  "+name+"\n"))
	f, got, err := verifiedArtifact(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || !bytes.Equal(read, data) || got != digest {
		t.Fatalf("bad verified artifact: %q %s %v", read, got, err)
	}
	write(name, []byte("tampered executable"))
	if _, _, err := verifiedArtifact(dir, name); err == nil {
		t.Fatal("tampered artifact accepted")
	}
	write(name, data)
	write("SHA256SUMS", []byte(digest+"  "+name+"\n"+digest+"  "+name+"\n"))
	if _, _, err := verifiedArtifact(dir, name); err == nil {
		t.Fatal("duplicate manifest entry accepted")
	}
}

func TestUploadIsVerifiedAtomicAndSeparateFromLegacy(t *testing.T) {
	home := t.TempDir()
	data := []byte("#!/bin/sh\nprintf test-runtime\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	release := "test-version/" + digest
	run := func(script string, payload []byte) ([]byte, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = bytes.NewReader(payload)
		return cmd.CombinedOutput()
	}
	if out, err := run(uploadScript(release, digest), data); err != nil {
		t.Fatalf("upload: %v %s", err, out)
	}
	installed := filepath.Join(home, ".local/share/relay/releases", release, "relay")
	got, err := os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed file %q, %v", got, err)
	}
	if out, err := run(uploadScript(release, digest), []byte("corrupt")); err == nil {
		t.Fatalf("corrupt upload accepted: %s", out)
	}
	got, err = os.ReadFile(installed)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("failed upload damaged installed artifact")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(installed), ".upload.*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary upload leaked: %v %v", matches, err)
	}
	if out, err := run(activateScript(release), nil); err != nil {
		t.Fatalf("activate: %v %s", err, out)
	}
	link := filepath.Join(home, ".local/share/relay/bin/relay")
	if resolved, err := filepath.EvalSymlinks(link); err != nil || resolved != installed {
		t.Fatalf("active runtime %s %v", resolved, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/bin/relay")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy path touched: %v", err)
	}
	// Replacing a link to a directory must replace the link itself, not copy
	// executable links into the unrelated directory (mv's default behavior).
	directory := filepath.Join(home, "unrelated")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if out, err := run(activateScript(release), nil); err != nil {
		t.Fatalf("reactivate directory symlink: %v %s", err, out)
	}
	if resolved, err := filepath.EvalSymlinks(link); err != nil || resolved != installed {
		t.Fatalf("reactivation target %s %v", resolved, err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("wrote into unrelated directory: %v %v", entries, err)
	}
}

func fakeConnection(t *testing.T, body string) *Connection {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Connection{cfg: Config{Target: "unused", ControlPath: "/unused"}, sshPath: path, ctx: ctx, cancel: cancel, ready: true}
}

func TestRunCancellationAndOutputBound(t *testing.T) {
	c := fakeConnection(t, "for argument do command=$argument; done\nexec /bin/sh -c \"$command\"\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Run(ctx, "sleep 30", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error: %v", err)
	}
	out, err := c.Run(context.Background(), "head -c 2097152 /dev/zero", nil)
	if err == nil || len(out) != maxCommandOutput {
		t.Fatalf("output bound: len=%d err=%v", len(out), err)
	}
}

func TestBridgeSupportsDeadlinesAndSurvivesDialContext(t *testing.T) {
	c := fakeConnection(t, "exec cat\n")
	dialCtx, cancel := context.WithCancel(context.Background())
	stream, err := c.DialContext(dialCtx, "tcp", "relay:80")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel() // net/http is allowed to cancel its dial context after success.
	if err := stream.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("roundtrip %q: %v", buf, err)
	}
	if err := stream.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("deadline error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTerminalResizePreservesReadDeadlines(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	terminal, err := pollableTerminal(master)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := resizeTerminal(terminal, 120, 40); err != nil {
		t.Fatal(err)
	}
	if err := terminal.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { var buf [1]byte; _, err := terminal.Read(buf[:]); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("read deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("resizing changed the terminal into blocking mode")
	}
}

func TestRuntimeProtocolCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name      string
		health    health
		wantError bool
	}{
		{"same build", health{Version: "client-new", Protocol: 1}, false},
		{"older compatible server", health{Version: "0.1.0-dev.a7360f2d3765", Protocol: 1}, false},
		{"newer compatible server", health{Version: "2.0.0", Protocol: 1}, false},
		{"wrong protocol", health{Version: "client-new", Protocol: 2}, true},
		{"missing protocol", health{Version: "client-new"}, true},
		{"missing build", health{Protocol: 1}, true},
		{"malformed build", health{Version: "bad\nversion", Protocol: 1}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkHealth(tt.health, "client-new"); (err != nil) != tt.wantError {
				t.Fatalf("compatibility error = %v, want error %v", err, tt.wantError)
			}
		})
	}
}
