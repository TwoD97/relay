package peers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "private-peers"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func snapshot(n int) Snapshot {
	return Snapshot{SourceID: fmt.Sprintf("%048x", n), Name: "Desktop fixture", Revision: 1, UpdatedAt: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC), Data: json.RawMessage(`{"pages":[],"tasks":[]}`)}
}
func TestPeerSnapshotsCanonicalCASReopenAndDelete(t *testing.T) {
	s := fixture(t)
	initial := snapshot(1)
	initial.Data = json.RawMessage(`{ "tasks": [], "pages": [] }`)
	record, err := s.Put("", initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecord(record); err != nil {
		t.Fatal(err)
	}
	tampered := record
	tampered.Name = "Changed without matching digest"
	if err := ValidateRecord(tampered); !errors.Is(err, ErrCorrupt) {
		t.Fatal("unverified record accepted", err)
	}
	canonical := snapshot(1)
	canonical.UpdatedAt = initial.UpdatedAt.In(time.FixedZone("fixture", 3600))
	again, err := s.Put(strings.Repeat("f", 64), canonical)
	if err != nil || record.Digest != again.Digest {
		t.Fatal("identical snapshot not idempotent", again, err)
	}
	other, err := Open(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := other.Get(initial.SourceID)
	if err != nil || saved.Digest != record.Digest || string(saved.Data) != `{"pages":[],"tasks":[]}` {
		t.Fatal(saved, err)
	}
	changed := canonical
	changed.Data = json.RawMessage(`{"tasks":[{"title":"Actual shared task"}]}`)
	if _, err = other.Put(record.Digest, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("same revision replaced", err)
	}
	changed.Revision++
	if _, err = other.Put("", changed); !errors.Is(err, ErrConflict) {
		t.Fatal("missing CAS replaced", err)
	}
	updated, err := other.Put(record.Digest, changed)
	if err != nil || updated.Digest == record.Digest {
		t.Fatal(updated, err)
	}
	if err = s.Delete(initial.SourceID, record.Digest); !errors.Is(err, ErrConflict) {
		t.Fatal("stale deletion", err)
	}
	if err = s.Delete(initial.SourceID, updated.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(initial.SourceID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	index, err := s.List()
	if err != nil || len(index.Sources) != 0 || index.Sources == nil {
		t.Fatal(index, err)
	}
}
func TestPeerSnapshotsConcurrentCASHasOneWinner(t *testing.T) {
	s := fixture(t)
	initial, err := s.Put("", snapshot(2))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 8)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			change := initial.Snapshot
			change.Revision++
			change.Name = fmt.Sprintf("Publisher %d", i)
			_, err := s.Put(initial.Digest, change)
			results <- err
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatal("CAS winners", winners)
	}
}
func TestPeerSnapshotsCrossStoreLockFailsBeforeMutation(t *testing.T) {
	s := fixture(t)
	other, err := Open(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockDirectory(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.Put("", snapshot(3))
	unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatal("second writer did not fail closed", err)
	}
	if _, err = other.Get(snapshot(3).SourceID); !errors.Is(err, ErrNotFound) {
		t.Fatal("busy write published", err)
	}
	if _, err = other.Put("", snapshot(3)); err != nil {
		t.Fatal(err)
	}
}
func TestPeerSnapshotsCapacityAndValidation(t *testing.T) {
	s := fixture(t)
	for i := 0; i < MaxSources; i++ {
		if _, err := s.Put("", snapshot(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put("", snapshot(MaxSources)); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	index, err := s.List()
	if err != nil || len(index.Sources) != MaxSources {
		t.Fatal(index, err)
	}
	for _, entry := range index.Sources {
		if entry.Bytes <= 0 || entry.Bytes > MaxBytes {
			t.Fatal(entry)
		}
	}
	for _, test := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"source", func(s *Snapshot) { s.SourceID = "../../outside" }},
		{"uppercase source", func(s *Snapshot) { s.SourceID = strings.Repeat("A", 48) }},
		{"empty name", func(s *Snapshot) { s.Name = "  " }},
		{"control", func(s *Snapshot) { s.Name = "Machine\n" }},
		{"format control", func(s *Snapshot) { s.Name = "Machine\u202e" }},
		{"long name", func(s *Snapshot) { s.Name = strings.Repeat("a", 81) }},
		{"zero revision", func(s *Snapshot) { s.Revision = 0 }},
		{"unsafe JSON integer revision", func(s *Snapshot) { s.Revision = 1 << 53 }},
		{"missing time", func(s *Snapshot) { s.UpdatedAt = time.Time{} }},
		{"array", func(s *Snapshot) { s.Data = json.RawMessage(`[]`) }},
		{"null", func(s *Snapshot) { s.Data = json.RawMessage(`null`) }},
		{"two values", func(s *Snapshot) { s.Data = json.RawMessage(`{} {}`) }},
		{"deep", func(s *Snapshot) {
			s.Data = json.RawMessage(`{"x":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`)
		}},
		{"oversized", func(s *Snapshot) { s.Data = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxBytes) + `"}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := snapshot(20)
			test.change(&candidate)
			if _, err := s.Put("", candidate); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
	// The limit applies to the entire canonical snapshot, not only its data.
	candidate := snapshot(20)
	candidate.Data = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxBytes-32) + `"}`)
	if _, err = s.Put("", candidate); !errors.Is(err, ErrInvalid) {
		t.Fatal("envelope bypassed limit", err)
	}
}
func TestPeerSnapshotsCorruptionIsVisibleAndNeverOverwritten(t *testing.T) {
	s := fixture(t)
	good, err := s.Put("", snapshot(1))
	if err != nil {
		t.Fatal(err)
	}
	bad, err := s.Put("", snapshot(2))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.directory, bad.SourceID+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(original, []byte("Desktop fixture"), []byte("Tampered fixture"), 1)
	if err = os.WriteFile(path, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	index, err := s.List()
	if err != nil || len(index.Sources) != 1 || index.Sources[0].Digest != good.Digest || len(index.Errors) != 1 || index.Errors[0].SourceID != bad.SourceID {
		t.Fatal(index, err)
	}
	change := bad.Snapshot
	change.Revision++
	if _, err = s.Put(bad.Digest, change); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if err = s.Delete(bad.SourceID, bad.Digest); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, tampered) {
		t.Fatal("corruption was silently replaced")
	}
	if _, err = s.Get(good.SourceID); err != nil {
		t.Fatal("unrelated valid source unavailable", err)
	}
}
func TestPeerSnapshotsRefuseLinkedStorage(t *testing.T) {
	s := fixture(t)
	out := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(out, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	id := snapshot(1).SourceID
	if err := os.Symlink(out, filepath.Join(s.directory, id+".json")); err != nil {
		t.Skipf("test user cannot create a symlink: %v", err)
	}
	if _, err := s.Put("", snapshot(1)); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "untouched" {
		t.Fatal("linked target changed")
	}
	link := filepath.Join(t.TempDir(), "linked-store")
	if err := os.Symlink(s.directory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("linked store accepted")
	}
}
