//go:build linux

package projectcontext

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxFileBytes = 128 << 10

type target struct {
	name, block, initial string
	optional             bool
	before               []byte
	stat                 unix.Stat_t
	exists               bool
	append               []byte
}

// Prepare serializes cooperating writers on the directory inode. Every target
// is preflighted before mutation. Existing bytes are only appended to, never
// rewritten. Multiple files are not one filesystem transaction: an I/O failure
// can leave an incomplete setup, reported as an error rather than success.
func Prepare(ctx context.Context, path string) (Result, error) {
	if err := ValidatePath(path); err != nil {
		return Result{}, err
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Result{}, err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Result{}, fmt.Errorf("open project folder: %w", err)
	}
	fd, err := openDirectory(path)
	if err != nil {
		return Result{}, fmt.Errorf("open project folder: %w", err)
	}
	defer unix.Close(fd)
	if err := lockDirectory(ctx, fd); err != nil {
		return Result{}, err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	var directory unix.Stat_t
	if err := unix.Fstat(fd, &directory); err != nil {
		return Result{}, err
	}
	checkDirectory := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := openDirectory(path)
		if err != nil {
			return errors.New("project folder changed during preparation; choose the folder again")
		}
		defer unix.Close(current)
		var stat unix.Stat_t
		if unix.Fstat(current, &stat) != nil || stat.Dev != directory.Dev || stat.Ino != directory.Ino {
			return errors.New("project folder changed during preparation; choose the folder again")
		}
		return nil
	}
	targets := []*target{
		{name: "MEMORY.md", initial: memoryTemplate},
		{name: "HANDOFF.md", initial: handoffTemplate},
		{name: "AGENTS.md", block: notesBlock, initial: "# Project instructions\n\n" + notesBlock},
		{name: "AGENTS.override.md", block: notesBlock, optional: true},
		{name: "CLAUDE.md", block: claudeBlock, initial: "# Claude Code project instructions\n\n" + claudeBlock},
	}
	result := Result{Path: path, Files: []File{}, Warnings: []string{"Shared Markdown notes do not merge native conversations or private provider memory. Review these project files before committing them."}}
	for _, t := range targets {
		if err := preflight(fd, t); err != nil {
			return Result{}, fmt.Errorf("%s: %w", t.name, err)
		}
		if t.name == "AGENTS.override.md" && t.exists {
			result.Warnings = append(result.Warnings, "AGENTS.override.md exists. Shared notes were included there too; Codex's normal instruction precedence still applies.")
		}
		if t.block != "" && len(t.before)+len(t.append) > 24<<10 {
			result.Warnings = append(result.Warnings, t.name+" is large; ancestor instructions also count toward Codex's configured instruction limit.")
		}
	}
	for _, t := range targets {
		if t.optional && !t.exists {
			continue
		}
		if err := checkDirectory(); err != nil {
			return Result{}, fmt.Errorf("project context may be partly prepared: %w", err)
		}
		status := "preserved"
		if !t.exists {
			err = createFile(fd, t.name, []byte(t.initial))
			status = "created"
		} else {
			err = appendFile(fd, t)
			if len(t.append) != 0 {
				status = "updated"
			}
		}
		if err != nil {
			return Result{}, fmt.Errorf("project context may be partly prepared (%s): %w; review the files before retrying", t.name, err)
		}
		result.Files = append(result.Files, File{Path: t.name, Status: status})
	}
	if err := checkDirectory(); err != nil {
		return Result{}, err
	}
	return result, nil
}

// Walking from / using O_NOFOLLOW prevents an ancestor replacement from
// redirecting writes outside the already resolved project directory.
func openDirectory(path string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, name := range strings.Split(filepath.Clean(path), "/") {
		if name == "" {
			continue
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func lockDirectory(ctx context.Context, fd int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("lock project folder: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func openTarget(dir int, name string, flags int) (*os.File, unix.Stat_t, error) {
	var stat unix.Stat_t
	fd, err := unix.Openat(dir, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, stat, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err = unix.Fstat(fd, &stat); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
			err = errors.New("must be a regular, singly linked file owned by the current user (no symlinks)")
		}
	}
	if err != nil {
		f.Close()
		return nil, stat, err
	}
	return f, stat, nil
}

func readTarget(f *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("must be UTF-8 text without NUL bytes and no larger than 128 KiB")
	}
	return data, nil
}

func preflight(dir int, t *target) error {
	f, stat, err := openTarget(dir, t.name, unix.O_RDONLY)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	t.exists, t.stat = true, stat
	t.before, err = readTarget(f)
	if err != nil || t.block == "" {
		return err
	}
	text := string(t.before)
	if strings.Contains(text, beginMarker) || strings.Contains(text, endMarker) {
		if strings.Count(text, beginMarker) != 1 || strings.Count(text, endMarker) != 1 || !strings.Contains(text, t.block) {
			return errors.New("shared context block is incomplete or edited; review and remove that marked block before preparing again")
		}
		return nil
	}
	t.append = []byte("\n\n" + t.block)
	if len(t.before)+len(t.append) > maxFileBytes {
		return errors.New("shared context would exceed the 128 KiB file limit; move lengthy notes out of this instruction file first")
	}
	return nil
}

func sameFile(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && b.Nlink == 1 && a.Uid == b.Uid && b.Mode&unix.S_IFMT == unix.S_IFREG
}

func namedFileMatches(dir int, name string, stat unix.Stat_t) bool {
	var current unix.Stat_t
	return unix.Fstatat(dir, name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && sameFile(stat, current)
}

func appendFile(dir int, t *target) error {
	flags := unix.O_RDONLY
	if len(t.append) > 0 {
		flags = unix.O_RDWR | unix.O_APPEND
	}
	f, stat, err := openTarget(dir, t.name, flags)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := readTarget(f)
	if err != nil {
		return err
	}
	if !sameFile(t.stat, stat) || !bytes.Equal(data, t.before) || !namedFileMatches(dir, t.name, stat) {
		return errors.New("file changed during preparation; existing content was not overwritten")
	}
	if len(t.append) == 0 {
		return nil
	}
	// One O_APPEND write preserves the original prefix. Never roll back by
	// truncating: another editor may have appended its own content concurrently.
	n, err := f.Write(t.append)
	if err == nil && n != len(t.append) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return err
	}
	if !namedFileMatches(dir, t.name, stat) {
		return errors.New("file was replaced during preparation")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := readTarget(f)
	expected := append(append([]byte{}, t.before...), t.append...)
	if err != nil || !bytes.Equal(written, expected) {
		return errors.New("file content changed during preparation; review the marked block before retrying")
	}
	return nil
}

func createFile(dir int, name string, data []byte) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	temp := ".relay-context-" + hex.EncodeToString(token[:])
	fd, err := unix.Openat(dir, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), temp)
	defer f.Close()
	defer unix.Unlinkat(dir, temp, 0)
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if !namedFileMatches(dir, temp, stat) {
		return errors.New("temporary project file was replaced during preparation")
	}
	// NOREPLACE is essential: a newly created user file must win a race.
	if err := unix.Renameat2(dir, temp, dir, name, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publish file without replacing existing content: %w", err)
	}
	if !namedFileMatches(dir, name, stat) {
		return errors.New("new project file was replaced during preparation")
	}
	return unix.Fsync(dir)
}
