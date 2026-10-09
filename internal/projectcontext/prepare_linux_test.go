//go:build linux

package projectcontext

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func put(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0640); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPreparePreservesExistingBytesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	originals := map[string][]byte{
		"AGENTS.md":          []byte("# Existing policy\r\nKeep my exact spacing.  \r\n"),
		"CLAUDE.md":          []byte("# Claude rules\n@.claude/project.md\nNo trailing newline"),
		"MEMORY.md":          []byte("My existing memory.\n"),
		"HANDOFF.md":         []byte("My existing handoff.\n"),
		"AGENTS.override.md": []byte("# Explicit override\nKeep it.\n"),
	}
	for name, data := range originals {
		put(t, dir, name, data)
	}
	result, err := Prepare(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != dir || len(result.Files) != 5 || len(result.Warnings) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	after := map[string][]byte{}
	for _, file := range result.Files {
		data := read(t, dir, file.Path)
		after[file.Path] = data
		if !bytes.HasPrefix(data, originals[file.Path]) {
			t.Fatalf("rewrote original %s", file.Path)
		}
		if file.Path == "MEMORY.md" || file.Path == "HANDOFF.md" {
			if file.Status != "preserved" || !bytes.Equal(data, originals[file.Path]) {
				t.Fatalf("changed user notes %s", file.Path)
			}
		} else if file.Status != "updated" || bytes.Count(data, []byte(beginMarker)) != 1 {
			t.Fatalf("missing one marked block in %s: %s", file.Path, file.Status)
		}
		stat, _ := os.Stat(filepath.Join(dir, file.Path))
		if stat.Mode().Perm() != 0640 {
			t.Fatalf("changed existing permissions on %s", file.Path)
		}
	}
	result, err = Prepare(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range result.Files {
		if file.Status != "preserved" || !bytes.Equal(read(t, dir, file.Path), after[file.Path]) {
			t.Fatalf("repeated setup changed %s", file.Path)
		}
	}
}

func TestPrepareConcurrentCallersCreateOneCompleteSet(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() { _, err := Prepare(context.Background(), dir); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Fatalf("unexpected files or abandoned temporary file: %v", entries)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if bytes.Count(read(t, dir, name), []byte(beginMarker)) != 1 {
			t.Fatal("duplicated managed block", name)
		}
	}
	if string(read(t, dir, "MEMORY.md")) != memoryTemplate || string(read(t, dir, "HANDOFF.md")) != handoffTemplate {
		t.Fatal("incomplete note templates")
	}
}

func TestPrepareRejectsUnsafeTargetsBeforeAnyWrite(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling", "hardlink", "directory", "fifo", "too-large", "invalid-text", "incomplete-marker", "edited-marker", "duplicate-marker"} {
		t.Run(kind, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			put(t, outside, "private", []byte("unchanged outside"))
			path := filepath.Join(dir, "CLAUDE.md")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(outside, "private"), path)
			case "dangling":
				err = os.Symlink(filepath.Join(outside, "absent"), path)
			case "hardlink":
				err = os.Link(filepath.Join(outside, "private"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "too-large":
				put(t, dir, "CLAUDE.md", bytes.Repeat([]byte("x"), maxFileBytes+1))
			case "invalid-text":
				put(t, dir, "CLAUDE.md", []byte{'x', 0, 0xff})
			case "incomplete-marker":
				put(t, dir, "CLAUDE.md", []byte(beginMarker+"\ninterrupted write"))
			case "edited-marker":
				put(t, dir, "CLAUDE.md", []byte(beginMarker+"\ncustom block\n"+endMarker))
			case "duplicate-marker":
				put(t, dir, "CLAUDE.md", []byte(claudeBlock+claudeBlock))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Prepare(context.Background(), dir); err == nil {
				t.Fatal("unsafe file accepted")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 || entries[0].Name() != "CLAUDE.md" {
				t.Fatalf("preflight failure wrote other files: %v", entries)
			}
			if string(read(t, outside, "private")) != "unchanged outside" {
				t.Fatal("modified unrelated file")
			}
		})
	}
}

func TestPrepareCancellationWhileDirectoryLocked(t *testing.T) {
	dir := t.TempDir()
	fd, err := openDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := Prepare(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lock ignored cancellation", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("cancelled preparation wrote files")
	}
}

func TestFileReplacementAndNewFileRaceDoNotClobberContent(t *testing.T) {
	dir := t.TempDir()
	fd, err := openDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	put(t, dir, "AGENTS.md", []byte("initial"))
	target := &target{name: "AGENTS.md", block: notesBlock}
	if err := preflight(fd, target); err != nil {
		t.Fatal(err)
	}
	put(t, dir, "replacement", []byte("concurrent user edit"))
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := appendFile(fd, target); err == nil {
		t.Fatal("replacement race accepted")
	}
	if err := createFile(fd, "AGENTS.md", []byte("must not replace")); err == nil {
		t.Fatal("create overwrote concurrent file")
	}
	if string(read(t, dir, "AGENTS.md")) != "concurrent user edit" {
		t.Fatal("concurrent edit clobbered")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
}

func TestPreparationResolvesProjectSymlinkWithoutFollowingFileSymlinks(t *testing.T) {
	dir, links := t.TempDir(), t.TempDir()
	path := filepath.Join(links, "project ' $(printf injection) space")
	if err := os.Symlink(dir, path); err != nil {
		t.Fatal(err)
	}
	result, err := Prepare(context.Background(), path)
	if err != nil || result.Path != dir {
		t.Fatal(result, err)
	}
}

func TestValidatePath(t *testing.T) {
	for _, path := range []string{"", "relative", "~/../private", "/tmp/../private", "/tmp/a\nb", "/tmp/\x00", "/tmp/\xff", strings.Repeat("/", 4097)} {
		if ValidatePath(path) == nil {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
	for _, path := range []string{"~", "~/project", "/tmp/project ' $(echo literal)", "/tmp/with space", "/tmp/a..b"} {
		if err := ValidatePath(path); err != nil {
			t.Errorf("valid path %q: %v", path, err)
		}
	}
}
