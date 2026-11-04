package peers

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPeerSnapshotsFIFOReadIsBounded(t *testing.T) {
	s := fixture(t)
	id := snapshot(1).SourceID
	if err := syscall.Mkfifo(filepath.Join(s.directory, id+".json"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Get(id); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot read blocked on FIFO")
	}
}
