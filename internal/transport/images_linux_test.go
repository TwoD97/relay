//go:build linux

package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/TwoD97/relay/internal/sessionimage"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func imageFixture(t *testing.T) sessionimage.Image {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 7, 5))
	picture.Set(2, 2, color.NRGBA{G: 255, A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	result, err := sessionimage.Validate(data.Bytes(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestImageUploadScriptAtomicLiteralAndQuota(t *testing.T) {
	home := filepath.Join(t.TempDir(), "literal ' & $(false) home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	image := imageFixture(t)
	key, _ := sessionimage.Key(strings.Repeat("a", 32), time.Unix(100, 0))
	run := func(data []byte) ([]byte, error) {
		cmd := exec.Command("/bin/sh", "-c", imageUploadScript(key, image))
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = bytes.NewReader(data)
		return cmd.CombinedOutput()
	}
	payload, _ := io.ReadAll(image.Reader())
	var group sync.WaitGroup
	failures := make(chan string, 6)
	for range 6 {
		group.Add(1)
		go func() {
			defer group.Done()
			output, err := run(payload)
			if err != nil || !bytes.Contains(output, []byte("\x00ok\x00")) {
				failures <- fmt.Sprintf("%s %v", output, err)
			}
		}()
	}
	group.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	target := filepath.Join(home, ".local/share/relay/images", key, image.Filename())
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("stored data", err)
	}
	output, err := run([]byte("corrupt"))
	if err != nil || !bytes.Contains(output, []byte("\x00invalid\x00")) {
		t.Fatal("corrupt stdin accepted", string(output), err)
	}
	actual, _ = os.ReadFile(target)
	if !bytes.Equal(actual, payload) {
		t.Fatal("invalid upload replaced image")
	}
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	quota, err := os.Create(filepath.Join(filepath.Dir(target), "existing.png"))
	if err != nil {
		t.Fatal(err)
	}
	quota.Truncate(sessionimage.SessionQuota)
	quota.Close()
	output, err = run(payload)
	if err != nil || !bytes.Contains(output, []byte("\x00quota\x00")) {
		t.Fatal("quota bypassed", string(output), err)
	}
	temporaries, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".upload.*"))
	if len(temporaries) != 0 {
		t.Fatal("upload temporaries leaked")
	}
}
func realSSHImageRoundtrip(t *testing.T, ctx context.Context, connection *Connection, sessionID string) {
	t.Helper()
	picture := imageFixture(t)
	key, _ := sessionimage.Key(sessionID, time.Unix(100, 0))
	before, err := connection.Run(ctx, `stat -c '%i' "$HOME/.local/share/relay/run/daemon.sock"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	path, err := connection.UploadSessionImage(ctx, key, picture)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := connection.UploadSessionImage(ctx, key, picture)
	if err != nil || repeated != path {
		t.Fatal("image repeat", err)
	}
	bytes, err := connection.Run(ctx, "cat -- "+quoteShell(path), nil)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(bytes)) != picture.SHA256() {
		t.Fatal("real SSH image bytes", err)
	}
	after, err := connection.Run(ctx, `stat -c '%i' "$HOME/.local/share/relay/run/daemon.sock"`, nil)
	if err != nil || string(before) != string(after) {
		t.Fatal("upload replaced running daemon", err)
	}
}
