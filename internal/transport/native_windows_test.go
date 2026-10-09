//go:build windows

package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// These tests run on native Windows, with a pure Go SSH server and no subprocess
// or remote shell. The resolved configuration is fixture-owned: the tests do not
// read the user's SSH identities/configuration or contact the Windows SSH agent.
// Start/OpenSSH configuration discovery and remote Linux PTYs are outside this
// fixture; production authConfig, host-key storage, Run and DialContext are used.
func TestWindowsNativeSSHTrustAuthenticationAndPrivacy(t *testing.T) {
	server := newWindowsSSHFixture(t)
	known := filepath.Join(t.TempDir(), "profile with spaces", "known_hosts")
	c, transcript, err := server.login(t, known, "yes", "windows-fixture-secret")
	if err != nil || !c.Ready() || !strings.Contains(transcript, ssh.FingerprintSHA256(server.signer.PublicKey())) {
		t.Fatalf("interactive host trust/password login failed: %v", err)
	}
	if strings.Contains(transcript, "windows-fixture-secret") {
		t.Fatal("password appeared in authentication replay")
	}
	stored, err := os.ReadFile(known)
	if err != nil || !bytes.Contains(stored, []byte(server.signer.PublicKey().Type())) {
		t.Fatal("accepted host key was not saved to the isolated Windows path", err)
	}
	_ = c.Close()

	c, transcript, err = server.login(t, known, "yes", "windows-fixture-secret")
	if err != nil || strings.Contains(transcript, "(yes/no)") {
		t.Fatal("persisted host key was not reused", err)
	}
	_ = c.Close()

	before := server.passwords.Load()
	_, transcript, err = server.login(t, known, "yes", "deliberately-wrong-password")
	if err == nil || strings.Contains(transcript, "deliberately-wrong-password") {
		t.Fatal("incorrect password accepted or echoed", err)
	}
	if attempts := server.passwords.Load() - before; attempts < 1 || attempts > 3 {
		t.Fatalf("password retries were not bounded: %d", attempts)
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte(knownhosts.Line([]string{server.address}, wrong.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before = server.passwords.Load()
	_, transcript, err = server.login(t, known, "yes", "windows-fixture-secret")
	if err == nil || !strings.Contains(err.Error(), "host key changed") || strings.Contains(transcript, "password:") || server.passwords.Load() != before {
		t.Fatal("changed host key was not refused before requesting credentials", err)
	}
	_, transcript, err = server.login(t, filepath.Join(t.TempDir(), "declined_known_hosts"), "no", "windows-fixture-secret")
	if err == nil || !strings.Contains(err.Error(), "not accepted") || strings.Contains(transcript, "password:") {
		t.Fatal("declined host trust reached authentication", err)
	}
}

func TestWindowsNativeSSHCommandsStreamsCancellationAndNoReauthentication(t *testing.T) {
	server := newWindowsSSHFixture(t)
	c, _, err := server.login(t, filepath.Join(t.TempDir(), "known_hosts"), "yes", "windows-fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte("binary\x00stdin\r\n"), 10000)
	output, err := c.Run(ctx, "fixture-echo", bytes.NewReader(payload))
	if err != nil || !bytes.Equal(output, payload) {
		t.Fatal("native SSH stdin/stdout did not preserve binary data", err, len(output))
	}
	output, err = c.Run(ctx, "fixture-stderr", nil)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 7 || !bytes.Contains(output, []byte("STDOUT_MARKER")) || !bytes.Contains(output, []byte("STDERR_MARKER")) {
		t.Fatal("native command lost stderr or exit status", err, string(output))
	}
	output, err = c.Run(ctx, "fixture-large", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB") || len(output) != maxCommandOutput {
		t.Fatal("command output was not bounded", err, len(output))
	}

	dialCtx, cancelDial := context.WithCancel(ctx)
	stream, err := c.DialContext(dialCtx, "tcp", "runtime")
	if err != nil {
		cancelDial()
		t.Fatal(err)
	}
	defer stream.Close()
	cancelDial() // A completed HTTP dial does not own the stream lifetime.
	message := []byte("native bridge\x00survives dial cancellation")
	wrote := make(chan error, 1)
	go func() { _, err := stream.Write(message); wrote <- err }()
	_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply := make([]byte, len(message))
	if _, err := io.ReadFull(stream, reply); err != nil || !bytes.Equal(reply, message) {
		t.Fatal("native bridge stream changed bytes or closed after dial cancellation", err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err = stream.Read(make([]byte, 1))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("native stream read deadline was ignored", err)
	}
	_ = stream.Close()

	short, cancelRun := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = c.Run(short, "fixture-wait", nil)
	cancelRun()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("native command ignored its cancellation", err)
	}
	output, err = c.Run(ctx, "fixture-echo", strings.NewReader("still-authenticated"))
	if err != nil || string(output) != "still-authenticated" || server.connections.Load() != 1 {
		t.Fatal("cancelling one channel closed or reauthenticated the shared SSH connection", err)
	}
	_ = c.Close()
	if _, err := c.Run(ctx, "fixture-echo", nil); err == nil {
		t.Fatal("closed native connection accepted a command")
	}
	if _, err := c.DialContext(ctx, "tcp", "runtime"); err == nil {
		t.Fatal("closed native connection opened a stream")
	}
	if server.connections.Load() != 1 {
		t.Fatal("disconnected operations implicitly reauthenticated")
	}
}

type windowsSSHFixture struct {
	address     string
	signer      ssh.Signer
	connections atomic.Int32
	passwords   atomic.Int32
}

func newWindowsSSHFixture(t *testing.T) *windowsSSHFixture {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &windowsSSHFixture{signer: signer}
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		fixture.passwords.Add(1)
		if meta.User() == "fixture" && string(password) == "windows-fixture-secret" {
			return nil, nil
		}
		return nil, errors.New("fixture authentication refused")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.address = listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			fixture.connections.Add(1)
			workers.Go(func() {
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
				defer stop()
				server, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer server.Close()
				workers.Go(func() { ssh.DiscardRequests(requests) })
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "session fixture only")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err == nil {
						workers.Go(func() { windowsSSHChannel(channel, requests) })
					}
				}
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); workers.Wait() })
	return fixture
}

func windowsSSHChannel(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for request := range requests {
		var payload struct{ Command string }
		if request.Type != "exec" || ssh.Unmarshal(request.Payload, &payload) != nil {
			_ = request.Reply(false, nil)
			continue
		}
		_ = request.Reply(true, nil)
		status := uint32(0)
		switch payload.Command {
		case "/bin/sh -c " + quoteShell("fixture-echo"), "/bin/sh -c " + quoteShell(bridgeScript):
			_, _ = io.Copy(channel, channel)
		case "/bin/sh -c " + quoteShell("fixture-stderr"):
			_, _ = io.WriteString(channel, "STDOUT_MARKER")
			_, _ = io.WriteString(channel.Stderr(), "STDERR_MARKER")
			status = 7
		case "/bin/sh -c " + quoteShell("fixture-large"):
			_, _ = channel.Write(bytes.Repeat([]byte("x"), maxCommandOutput+1024))
		case "/bin/sh -c " + quoteShell("fixture-wait"):
			// Closing this session must wake the server without ending other
			// channels on the authenticated SSH connection.
			for range requests {
			}
		default:
			status = 127
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func (f *windowsSSHFixture) login(t *testing.T, known, trust, password string) (*Connection, string, error) {
	t.Helper()
	ctx, stopLogin := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(stopLogin)
	connectionCtx, cancel := context.WithCancel(ctx)
	c := &Connection{ctx: connectionCtx, cancel: cancel, done: make(chan struct{}), readyDone: make(chan struct{}), subs: map[chan []byte]struct{}{}}
	t.Cleanup(func() { _ = c.Close() })
	host, portString, _ := net.SplitHostPort(f.address)
	port, _ := strconv.Atoi(portString)
	config := nativeConfig{host: host, port: port, user: "fixture", identities: []string{"none"}, agentPath: "none", identitiesOnly: true, knownWrite: known, knownFiles: []string{known}}
	_, output, unsubscribe := c.Subscribe()
	defer unsubscribe()
	// Only connection construction is fixture glue. Authentication callbacks,
	// interactive input, trust persistence, commands and streams are production.
	go func() {
		var err error
		ready := false
		defer func() {
			cancel()
			c.mu.Lock()
			c.ready, c.ended, c.err = false, true, err
			c.mu.Unlock()
			if !ready {
				close(c.readyDone)
			}
			close(c.done)
		}()
		auth, cleanup, authErr := c.authConfig(config)
		if authErr != nil {
			err = authErr
			return
		}
		defer cleanup()
		raw, dialErr := (&net.Dialer{}).DialContext(connectionCtx, "tcp", f.address)
		if dialErr != nil {
			err = dialErr
			return
		}
		defer raw.Close()
		stop := context.AfterFunc(connectionCtx, func() { _ = raw.Close() })
		defer stop()
		conn, channels, requests, connectErr := ssh.NewClientConn(deadlineSSHConn{raw}, f.address, auth)
		if connectErr != nil {
			err = connectErr
			return
		}
		client := ssh.NewClient(conn, channels, requests)
		defer client.Close()
		c.mu.Lock()
		c.client, c.ready = client, true
		c.mu.Unlock()
		ready = true
		close(c.readyDone)
		err = client.Wait()
	}()
	var transcript strings.Builder
	for {
		select {
		case data := <-output:
			transcript.Write(data)
			var answer string
			if bytes.Contains(data, []byte("(yes/no)")) {
				answer = trust
			} else if bytes.Contains(data, []byte("password:")) {
				answer = password
			}
			if answer != "" {
				if _, err := c.Write([]byte(answer + "\r")); err != nil {
					t.Fatal("answer fixture SSH prompt", err)
				}
			}
		case <-c.readyDone:
			// Snapshot includes any final output that won a select race with ready.
			history, _, stop := c.Subscribe()
			stop()
			return c, string(history), c.WaitReady(ctx)
		case <-ctx.Done():
			return c, transcript.String(), ctx.Err()
		}
	}
}
