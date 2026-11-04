//go:build !windows && !relay_ssh_native

package transport

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorBundleImmutableVerifiedAndPrivate(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	c := fakeConnection(t, "export HOME="+quoteShell(home)+"\nfor argument do command=$argument; done\nexec /bin/sh -c \"$command\"\n")
	dir := t.TempDir()
	manifest := ""
	for _, name := range []string{"relay-linux-amd64", "relay-linux-arm64"} {
		content := []byte("fixture-" + name)
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		manifest += fmt.Sprintf("%x  %s\n", sha256.Sum256(content), name)
	}
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(manifest), 0600)
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte("0.4.0-fixture\n"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path, err := c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture")
	if err != nil || again != path {
		t.Fatal("repeat staging changed identity", err)
	}
	for _, name := range []string{"VERSION", "SHA256SUMS", "relay-linux-amd64", "relay-linux-arm64"} {
		got, _ := os.ReadFile(filepath.Join(path, name))
		expected, _ := os.ReadFile(filepath.Join(dir, name))
		if string(got) != string(expected) {
			t.Fatal("incorrect payload", name)
		}
	}
	if _, err = os.Lstat(filepath.Join(home, ".local/share/relay/bin/relay")); !os.IsNotExist(err) {
		t.Fatal("staging activated a runtime")
	}
	target := filepath.Join(path, "relay-linux-amd64")
	os.WriteFile(target, []byte("foreign change"), 0700)
	if _, err = c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture"); err == nil {
		t.Fatal("changed artifact overwritten")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "foreign change" {
		t.Fatal("foreign bytes lost")
	}
	os.Remove(target)
	sentinel := filepath.Join(home, "sentinel")
	os.WriteFile(sentinel, []byte("untouched"), 0600)
	os.Symlink(sentinel, target)
	if _, err = c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture"); err == nil {
		t.Fatal("symlink accepted")
	}
	got, _ = os.ReadFile(sentinel)
	if string(got) != "untouched" {
		t.Fatal("symlink target changed")
	}
	if _, err = c.StageCoordinatorBundle(ctx, dir, "other-version"); err == nil {
		t.Fatal("version mismatch accepted")
	}
}
func TestCoordinatorBridgeScopedAndDialCancellation(t *testing.T) {
	c := fakeConnection(t, "exec cat\n")
	for _, id := range []string{"", strings.Repeat("a", 31), strings.Repeat("A", 32), "../../other", strings.Repeat("a", 32) + ";true"} {
		if _, err := c.DialCoordinatorContext(context.Background(), id); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.DialCoordinatorContext(ctx, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	stream.SetDeadline(time.Now().Add(time.Second))
	if _, err = stream.Write([]byte("coordinator")); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 11)
	if _, err = io.ReadFull(stream, body); err != nil || string(body) != "coordinator" {
		t.Fatal("dial context closed established bridge", err)
	}
}

func TestCoordinatorBundleStagesOptionalConnectorWithoutChangingCore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	os.Mkdir(home, 0700)
	c := fakeConnection(t, "export HOME="+quoteShell(home)+"\nfor argument do command=$argument; done\nexec /bin/sh -c \"$command\"\n")
	dir := t.TempDir()
	names := []string{"relay-linux-amd64", "relay-linux-arm64", "relay-whatsapp-personal-linux-amd64", "relay-whatsapp-personal-linux-arm64", "relay-whatsapp-personal-windows-amd64.exe"}
	core, optional := "", ""
	for i, name := range names {
		data := []byte("fixture-" + name)
		os.WriteFile(filepath.Join(dir, name), data, 0600)
		line := fmt.Sprintf("%x  %s\n", sha256.Sum256(data), name)
		if i < 2 {
			core += line
		} else {
			optional += line
		}
	}
	os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(core), 0600)
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.whatsapp"), []byte(optional), 0600)
	os.WriteFile(filepath.Join(dir, "VERSION"), []byte("0.4.0-fixture\n"), 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	path, err := c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range append(names, "SHA256SUMS.whatsapp") {
		got, err := os.ReadFile(filepath.Join(path, name))
		expected, _ := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != string(expected) {
			t.Fatal("optional bundle mismatch", name, err)
		}
	}
	os.WriteFile(filepath.Join(path, names[2]), []byte("foreign"), 0700)
	if _, err = c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture"); err == nil {
		t.Fatal("tampered connector overwritten")
	}
	got, _ := os.ReadFile(filepath.Join(path, names[2]))
	if string(got) != "foreign" {
		t.Fatal("foreign connector bytes changed")
	}
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.whatsapp"), []byte(optional+fmt.Sprintf("%x  ../other\n", sha256.Sum256([]byte("no")))), 0600)
	if _, err = c.StageCoordinatorBundle(ctx, dir, "0.4.0-fixture"); err == nil {
		t.Fatal("optional path traversal accepted")
	}
}
