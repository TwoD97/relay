package personalwhatsapp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConnectorFixtureProcess(t *testing.T) {
	mode := os.Getenv("RELAY_CONNECTOR_FIXTURE")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "send" {
			if path := os.Getenv("RELAY_CONNECTOR_RECEIPTS"); path != "" {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					os.Exit(3)
				}
				fmt.Fprintln(f, req.MessageID)
				f.Close()
			}
			switch mode {
			case "hang":
				time.Sleep(time.Minute)
			case "wrong-id":
				req.ID++
			case "oversize":
				fmt.Println(strings.Repeat("X", maxFrame+1))
			case "exit":
				fmt.Fprintln(os.Stderr, "PRIVATE AUTH OUTPUT MUST NOT ESCAPE")
				os.Exit(1)
			}
		}
		response := frame{Kind: "response", ID: req.ID, State: &State{Linked: true, Connected: true}}
		if req.Method == "pair" {
			response.Code = "ABCD-EFGH"
		}
		if mode == "error" && req.Method == "send" {
			response.Error = "PRIVATE PHONE AND ACCOUNT DATA"
		}
		if req.Method == "connect" {
			out.Encode(frame{Kind: "message", Message: &Message{ID: "message1", Text: "status", At: time.Now()}})
		}
		out.Encode(response)
		if req.Method == "close" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}
func fixtureClient(t *testing.T, mode string, receive func(Message)) (Client, string) {
	t.Helper()
	receipts := filepath.Join(t.TempDir(), "receipts")
	cmd := exec.Command(os.Args[0], "-test.run=^TestConnectorFixtureProcess$")
	cmd.Env = append(os.Environ(), "RELAY_CONNECTOR_FIXTURE="+mode, "RELAY_CONNECTOR_RECEIPTS="+receipts)
	configureProcess(cmd)
	client, err := startClient(context.Background(), cmd, receive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	return client, receipts
}
func TestConnectorProtocolStateMessagesAndNoSecrets(t *testing.T) {
	messages := make(chan Message, 1)
	client, _ := fixtureClient(t, "normal", func(m Message) { messages <- m })
	if !client.State().Linked {
		t.Fatal("initial state not received")
	}
	code, err := client.Pair(context.Background(), "1234567890")
	if err != nil || code != "ABCD-EFGH" {
		t.Fatalf("pair response: %q %v", code, err)
	}
	if err = client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-messages:
		if m.Text != "status" {
			t.Fatal("wrong event")
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	failing, _ := fixtureClient(t, "error", nil)
	err = failing.Send(context.Background(), "send1", "hello")
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("raw connector failure exposed: %v", err)
	}
}

func TestConnectorEnvironmentExcludesInheritedCapabilities(t *testing.T) {
	got := strings.Join(connectorEnvironment([]string{"SystemRoot=C:\\Windows", "HOME=/test/home", "HTTPS_PROXY=http://proxy.invalid", "RELAY_AGENT_CAPABILITY_FILE=PRIVATE", "ANTHROPIC_API_KEY=PRIVATE", "OPENAI_API_KEY=PRIVATE", "NODE_OPTIONS=PRIVATE", "LD_PRELOAD=PRIVATE"}), "\n")
	if strings.Contains(got, "PRIVATE") || !strings.Contains(got, "SystemRoot=C:\\Windows") || !strings.Contains(got, "HTTPS_PROXY=http://proxy.invalid") {
		t.Fatal("connector environment lost OS settings or inherited unrelated capabilities")
	}
}
func TestConnectorUnknownSendIsNeverReplayed(t *testing.T) {
	for _, mode := range []string{"hang", "wrong-id", "oversize", "exit"} {
		t.Run(mode, func(t *testing.T) {
			client, path := fixtureClient(t, mode, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := client.Send(ctx, "send1", "test message")
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("expected safe unknown error: %v", err)
			}
			if err = client.Send(context.Background(), "send2", "must not send"); err == nil {
				t.Fatal("stopped connector accepted mutation")
			}
			if err = client.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "send1\n" {
				t.Fatalf("mutation replayed or absent: %q %v", data, err)
			}
			if !client.State().Stopped {
				t.Fatal("dead helper state not surfaced")
			}
		})
	}
}
func TestConnectorVerifiedStagingConcurrentAndCorrupt(t *testing.T) {
	bundle, parent := t.TempDir(), filepath.Join(t.TempDir(), "helpers")
	var manifest strings.Builder
	for _, name := range helperNames {
		data := []byte("fixture executable " + name)
		sum := sha256.Sum256(data)
		fmt.Fprintf(&manifest, "%x  %s\n", sum, name)
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS.whatsapp"), []byte(manifest.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	paths := make(chan string, 8)
	errors := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			path, err := stageHelper(context.Background(), bundle, parent)
			paths <- path
			errors <- err
		}()
	}
	wait.Wait()
	close(paths)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var staged string
	for path := range paths {
		if staged != "" && staged != path {
			t.Fatal("concurrent stage diverged")
		}
		staged = path
	}
	// Reuse never opens a running PE for write or relies on installation files.
	if err := os.Chmod(staged, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := stageHelper(context.Background(), bundle, parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := stageHelper(context.Background(), bundle, parent); err == nil {
		t.Fatal("overwrote/reused corrupt staged helper")
	}
	data, _ := os.ReadFile(staged)
	if string(data) != "tampered" {
		t.Fatal("corrupt evidence was overwritten")
	}
}
func TestConnectorManifestAndLinkedArtifactRefusal(t *testing.T) {
	sum := sha256.Sum256([]byte("fixture"))
	digest := hex.EncodeToString(sum[:])
	var valid strings.Builder
	for _, name := range helperNames {
		fmt.Fprintf(&valid, "%s  %s\n", digest, name)
	}
	if _, err := helperManifest([]byte(valid.String())); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{valid.String() + digest + "  unknown.exe\n", strings.Replace(valid.String(), helperNames[1], helperNames[0], 1), strings.Replace(valid.String(), digest, strings.Repeat("Z", 64), 1), ""} {
		if _, err := helperManifest([]byte(bad)); err == nil {
			t.Fatal("accepted bad manifest")
		}
	}
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "SHA256SUMS.whatsapp"), []byte(valid.String()), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(bundle, platformHelper())); err != nil {
		t.Skip("symlink privilege unavailable")
	}
	if _, err := stageHelper(context.Background(), bundle, filepath.Join(t.TempDir(), "staged")); err == nil {
		t.Fatal("followed linked helper")
	}
}

// Optional release gate: opens an empty isolated database through the actual
// verified executable, never connects or pairs a WhatsApp account.
func TestConnectorPackagedOfflineRoundTrip(t *testing.T) {
	bundle := os.Getenv("RELAY_PERSONAL_CONNECTOR_BUNDLE")
	if bundle == "" {
		t.Skip("set RELAY_PERSONAL_CONNECTOR_BUNDLE to a built optional connector bundle")
	}
	directory := filepath.Join(t.TempDir(), "epoch")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := OpenWithBundle(ctx, directory, bundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := client.State()
	if state.Linked || state.Connected || state.Problem != "" {
		t.Fatalf("unexpected nonempty fixture state: linked=%v connected=%v", state.Linked, state.Connected)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(directory, "device.sqlite")); err != nil {
		t.Fatal("helper did not initialize private storage")
	}
	if err = Forget(directory); err != nil {
		t.Fatal(err)
	}
}
