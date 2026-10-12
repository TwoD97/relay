package fleetregistry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFleetImportRevisionsRemoveNamesAndRejectReplay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fleet")
	a := fixture("a", 1)
	if err := Import(context.Background(), dir, a, testNow); err != nil {
		t.Fatal(err)
	}
	if err := Import(context.Background(), dir, a, testNow); err != nil {
		t.Fatal("identical delivery must be idempotent", err)
	}
	conflict := fixture("a", 1)
	conflict.Hosts[0].Name = "Changed without revision"
	if err := Import(context.Background(), dir, conflict, testNow); err == nil {
		t.Fatal("same revision replaced different content")
	}
	newer := fixture("a", 2)
	newer.Hosts = []Host{}
	if err := Import(context.Background(), dir, newer, testNow); err != nil {
		t.Fatal(err)
	}
	if err := Import(context.Background(), dir, a, testNow); err == nil {
		t.Fatal("delayed delivery resurrected a removed host")
	}
	listing, err := List(dir, testNow)
	if err != nil || len(listing.Sources) != 1 || len(listing.Sources[0].Hosts) != 0 {
		t.Fatal("removal was not durable", listing, err)
	}
	a.IssuedAt, a.ExpiresAt = testNow.Add(time.Hour), testNow.Add(time.Hour+Lifetime)
	if err := Import(context.Background(), dir, a, testNow.Add(time.Hour)); err == nil {
		t.Fatal("expired source lost its anti-replay revision")
	}
	b := fixture("b", 1)
	if err := Import(context.Background(), dir, b, testNow); err != nil {
		t.Fatal(err)
	}
	listing, err = List(dir, testNow.Add(Lifetime))
	if err != nil || len(listing.Sources) != 2 || !listing.Sources[1].Stale {
		t.Fatal("distinct sources or expiry were lost", listing, err)
	}
}

func TestFleetConcurrentImportsRetainHighestRevision(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fleet")
	var wait sync.WaitGroup
	for i := uint64(1); i <= 20; i++ {
		wait.Add(1)
		go func(revision uint64) {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := Import(ctx, dir, fixture("a", revision), testNow); err != nil && !strings.Contains(err.Error(), "older") {
				t.Error(err)
			}
		}(i)
	}
	wait.Wait()
	listing, err := List(dir, testNow)
	if err != nil || len(listing.Sources) != 1 || listing.Sources[0].Revision != 20 {
		t.Fatal("concurrent snapshots regressed", listing, err)
	}
}

func TestFleetCorruptionAndSymlinksFailClosed(t *testing.T) {
	for _, kind := range []string{"file-symlink", "directory-symlink", "public-file", "unknown-field", "wrong-source"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "fleet")
			s := fixture("a", 1)
			if err := Import(context.Background(), dir, s, testNow); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, s.ControllerID+".json")
			switch kind {
			case "file-symlink":
				if err := os.Rename(path, filepath.Join(base, "sentinel")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(base, "sentinel"), path); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				if err := os.Rename(dir, dir+"-other"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-other", dir); err != nil {
					t.Fatal(err)
				}
			case "public-file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "unknown-field":
				data, _ := os.ReadFile(path)
				data = append([]byte(`{"credential":"not allowed",`), data[1:]...)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-source":
				s.ControllerID = strings.Repeat("b", 48)
				data, _ := json.Marshal(s)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := List(dir, testNow); err == nil {
				t.Fatal("tampered registry was accepted")
			}
		})
	}
}
