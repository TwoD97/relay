//go:build linux && relay_ssh_native

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func nativeFixture(t *testing.T, beforeOpen ...func(context.Context)) (string, ssh.Signer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if metadata.User() == "relay" && string(password) == "native-secret" {
			return nil, nil
		}
		return nil, errors.New("incorrect credentials")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer raw.Close()
				stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
				defer stop()
				server, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				connectionCtx, connectionCancel := context.WithCancel(ctx)
				defer connectionCancel()
				go func() { _ = server.Wait(); connectionCancel() }()
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "sessions only")
						continue
					}
					if len(beforeOpen) > 0 {
						beforeOpen[0](connectionCtx)
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					workers.Go(func() {
						defer channel.Close()
						for request := range requests {
							if request.Type != "exec" {
								_ = request.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							if ssh.Unmarshal(request.Payload, &payload) != nil {
								_ = request.Reply(false, nil)
								return
							}
							_ = request.Reply(true, nil)
							command := payload.Command
							if command == "/bin/sh -c "+quoteShell(bridgeScript) {
								command = "cat"
							}
							cmd := exec.CommandContext(connectionCtx, "/bin/sh", "-c", command)
							cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
							cmd.Cancel = func() error {
								if cmd.Process == nil {
									return os.ErrProcessDone
								}
								return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
							}
							cmd.WaitDelay = time.Second
							cmd.Stdin = channel
							cmd.Stdout = channel
							cmd.Stderr = channel.Stderr()
							err := cmd.Run()
							status := uint32(0)
							if err != nil {
								status = 1
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					})
				}
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); workers.Wait() })
	return listener.Addr().String(), signer
}

func TestNativeCancelledChannelOpensPreserveConnectionAndRemainBounded(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	address, _ := nativeFixture(t, func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	nativeFixtureConfig(t, filepath.Join(t.TempDir(), "known_hosts"))
	c := loginNativeFixture(t, address, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			session, err := c.newSession(ctx)
			if session != nil {
				_ = session.Close()
			}
			results <- err
		}()
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		pending := len(c.sessionSlots)
		c.mu.Unlock()
		if pending == 16 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("channel opens did not reach bounded pending state")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	for i := 0; i < 16; i++ {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatal("open ignored cancellation", err)
		}
	}
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelBlocked()
	if _, err := c.newSession(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("excess pending channel was not bounded", err)
	}
	if !c.Ready() {
		t.Fatal("cancelled requests disconnected shared SSH transport")
	}
	once.Do(func() { close(release) })
	reuse, cancelReuse := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelReuse()
	output, err := c.Run(reuse, "printf shared-connection-survives", nil)
	if err != nil || string(output) != "shared-connection-survives" {
		t.Fatal("transport did not recover after abandoned channel opens", err, string(output))
	}
}

func nativeFixtureConfig(t *testing.T, knownPath string) {
	t.Helper()
	sshExecutable, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Host *\n IdentityFile none\n IdentityAgent none\n UserKnownHostsFile "+knownPath+"\n GlobalKnownHostsFile none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexec "+quoteShell(sshExecutable)+" -F "+quoteShell(config)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RELAY_SSH_KNOWN_HOSTS", knownPath)
}

func loginNativeFixture(t *testing.T, address string, expectTrust bool) *Connection {
	t.Helper()
	host, portText, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portText)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c, err := Start(ctx, Config{Target: "relay@" + host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	history, output, unsubscribe := c.Subscribe()
	defer unsubscribe()
	transcript := string(history)
	ready := make(chan error, 1)
	go func() { ready <- c.WaitReady(ctx) }()
	trusted, password := false, false
	for {
		if !trusted && strings.Contains(transcript, "(yes/no)") {
			if _, err := c.Write([]byte("yes\r")); err != nil {
				t.Fatal(err)
			}
			trusted = true
		}
		if !password && strings.Contains(transcript, "password:") {
			if _, err := c.Write([]byte("native-secret\r")); err != nil {
				t.Fatal(err)
			}
			password = true
		}
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal("native login:", err, transcript)
			}
			if trusted != expectTrust || !password || strings.Contains(transcript, "native-secret") {
				t.Fatal("native login prompt/privacy contract failed")
			}
			return c
		case data, ok := <-output:
			if !ok {
				t.Fatal("authentication terminal closed:", transcript, c.Err())
			}
			transcript += string(data)
		case <-ctx.Done():
			t.Fatal("native SSH login timed out:", transcript)
		}
	}
}

func TestNativeSSHAuthenticationCommandsStreamsAndHostKeyRefusal(t *testing.T) {
	address, signer := nativeFixture(t)
	knownPath := filepath.Join(t.TempDir(), "known_hosts")
	nativeFixtureConfig(t, knownPath)
	c := loginNativeFixture(t, address, true)
	stored, err := os.ReadFile(knownPath)
	if err != nil || !strings.Contains(string(stored), signer.PublicKey().Type()) {
		t.Fatal("accepted host was not persisted", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	payload := bytes.Repeat([]byte("binary\x00payload\n"), 10000)
	output, err := c.Run(ctx, "cat", bytes.NewReader(payload))
	if err != nil || !bytes.Equal(output, payload) {
		t.Fatal("native binary stdin/command output failed", err, len(output))
	}
	output, err = c.Run(ctx, "printf '%s' "+quoteShell("spaces ' dollars $ and `literal`"), nil)
	if err != nil || string(output) != "spaces ' dollars $ and `literal`" {
		t.Fatal("quoted command changed", err, string(output))
	}
	folder := t.TempDir()
	if err := os.Mkdir(filepath.Join(folder, "project ' $[]"), 0700); err != nil {
		t.Fatal(err)
	}
	listing, err := c.ListDirectories(ctx, DirectoryQuery{Path: folder, Prefix: "project ' $["})
	if err != nil || listing.Path != folder || len(listing.Directories) != 1 {
		t.Fatal("native SSH directory lookup", listing, err)
	}
	cancelled, cancelLookup := context.WithCancel(ctx)
	cancelLookup()
	if _, err := c.ListDirectories(cancelled, DirectoryQuery{Path: folder}); err == nil {
		t.Fatal("native SSH directory lookup ignored cancellation")
	}
	dialCtx, dialCancel := context.WithCancel(context.Background())
	stream, err := c.DialContext(dialCtx, "tcp", "unused")
	if err != nil {
		t.Fatal(err)
	}
	dialCancel()
	message := []byte("bridge survives cancelled dial context")
	wrote := make(chan error, 1)
	go func() { _, err := stream.Write(message); wrote <- err }()
	reply := make([]byte, len(message))
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadFull(stream, reply)
	if err != nil || !bytes.Equal(reply, message) || <-wrote != nil {
		t.Fatal("native stream failed", err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream deadline not enforced")
	}
	_ = stream.Close()
	short, stop := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer stop()
	if _, err := c.Run(short, "sleep 5", nil); err == nil {
		t.Fatal("command ignored context cancellation")
	}
	if _, err := c.Run(context.Background(), "printf alive", nil); err != nil {
		t.Fatal("cancelled channel destroyed healthy connection", err)
	}
	_ = c.Close()
	if _, err := c.Run(context.Background(), "true", nil); err == nil {
		t.Fatal("disconnected connection implicitly reauthenticated")
	}
	if _, err := c.ListDirectories(context.Background(), DirectoryQuery{Path: folder}); err == nil {
		t.Fatal("directory lookup reconnected without authentication")
	}
	reconnected := loginNativeFixture(t, address, false)
	_ = reconnected.Close()
	_, otherPrivate, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPrivate)
	if err := os.WriteFile(knownPath, []byte(knownhosts.Line([]string{address}, other.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(address)
	port, _ := strconv.Atoi(portText)
	bad, err := Start(context.Background(), Config{Target: "relay@" + host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("changed host key accepted: %v", err)
	}
	transcript, _, unsubscribe := bad.Subscribe()
	unsubscribe()
	if strings.Contains(string(transcript), "password:") {
		t.Fatal("asked for credentials before refusing changed host key")
	}
}

func TestNativePromptDoesNotQueueOrEchoPasswords(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Connection{ctx: ctx, cancel: cancel, subs: map[chan []byte]struct{}{}}
	first := make(chan string, 1)
	c.prompt = first
	if _, err := c.Write([]byte("secret\rnext-secret\r")); err != nil {
		t.Fatal(err)
	}
	if got := <-first; got != "secret" {
		t.Fatal("prompt answer changed")
	}
	if strings.Contains(string(c.history), "secret") || len(c.input) != 0 || c.prompt != nil {
		t.Fatal("credential was echoed or queued for another prompt")
	}
	if _, err := c.Write([]byte("stale-input")); err == nil {
		t.Fatal("input accepted outside a prompt")
	}
}

func TestNativeKnownHostOverrideMustBeAbsolute(t *testing.T) {
	t.Setenv("RELAY_SSH_KNOWN_HOSTS", "relative")
	_, err := resolveNativeConfig(context.Background(), Config{Target: "relay@127.0.0.1", Port: 22})
	if err == nil {
		t.Fatal("relative known-hosts override accepted")
	}
}

func TestNativeKnownHostsPathsPreserveSpaces(t *testing.T) {
	home := filepath.Join(t.TempDir(), "User Name")
	paths, err := configKnownPaths(home+"/.ssh/known_hosts "+home+"/.ssh/known_hosts2", home)
	if err != nil || len(paths) != 2 || paths[0] != filepath.Join(home, ".ssh", "known_hosts") {
		t.Fatal("default SSH paths with spaces changed", paths, err)
	}
	paths, err = configKnownPaths(`"/private/custom host keys" /other/keys`, home)
	if err != nil || len(paths) != 2 || paths[0] != "/private/custom host keys" {
		t.Fatal("quoted SSH path changed", paths, err)
	}
}
