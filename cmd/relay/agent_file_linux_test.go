package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAgentReadRejectsFIFOAndLinksWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link, dir} {
		done := make(chan error, 1)
		go func() {
			f, e := openAgentRead(path, true)
			if f != nil {
				f.Close()
			}
			done <- e
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("accepted %s", path)
			}
		case <-time.After(time.Second):
			t.Fatalf("blocked opening %s", path)
		}
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if f, err := openAgentRead(file, true); err == nil {
		f.Close()
		t.Fatal("accepted public capability")
	}
	if f, err := openAgentRead(file, false); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
}
