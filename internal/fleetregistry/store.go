package fleetregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func readFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 {
		return nil, errors.New("fleet state must be a private regular file")
	}
	if err := owned(before); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("fleet file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if len(data) > MaxBytes {
		return nil, errors.New("fleet state exceeds 128 KiB")
	}
	return data, err
}

func privateDirectory(dir string, create bool) error {
	if !filepath.IsAbs(dir) {
		return errors.New("fleet directory must be absolute")
	}
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("fleet directory must be a private directory, not a symlink")
	}
	return owned(info)
}

func List(dir string, now time.Time) (Listing, error) {
	result := Listing{Sources: []Source{}}
	if err := privateDirectory(dir, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return result, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !ValidControllerID(id) {
			return result, errors.New("unexpected fleet source file; preserve it for inspection")
		}
		if len(result.Sources) == MaxSources {
			return result, errors.New("fleet source limit exceeded (32)")
		}
		data, err := readFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return result, err
		}
		snapshot, err := Decode(bytes.NewReader(data))
		if err != nil || snapshot.ControllerID != id {
			return result, fmt.Errorf("invalid fleet source %s; preserve it for inspection", id)
		}
		result.Sources = append(result.Sources, Source{Snapshot: snapshot, Stale: !snapshot.ExpiresAt.After(now) || snapshot.IssuedAt.After(now.Add(2*time.Minute))})
	}
	sort.Slice(result.Sources, func(i, j int) bool { return result.Sources[i].ControllerID < result.Sources[j].ControllerID })
	return result, nil
}

// Import applies a complete, newer snapshot under one process-shared lock.
// Retain expired revisions so delayed deliveries cannot resurrect deleted names.
func Import(ctx context.Context, dir string, snapshot Snapshot, now time.Time) error {
	if err := Validate(snapshot); err != nil {
		return err
	}
	if !snapshot.ExpiresAt.After(now) || snapshot.IssuedAt.After(now.Add(2*time.Minute)) {
		return errors.New("fleet snapshot is expired or its controller clock is ahead")
	}
	if err := privateDirectory(dir, true); err != nil {
		return err
	}
	unlock, err := lock(ctx, filepath.Join(dir, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := List(dir, now)
	if err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > MaxBytes {
		return errors.New("fleet snapshot exceeds its size limit")
	}
	found := false
	for _, source := range current.Sources {
		if source.ControllerID != snapshot.ControllerID {
			continue
		}
		found = true
		if source.Revision > snapshot.Revision {
			return errors.New("fleet snapshot revision is older than the saved source")
		}
		if source.Revision == snapshot.Revision {
			old, _ := json.Marshal(source.Snapshot)
			if bytes.Equal(data, old) {
				return nil
			}
			return errors.New("fleet snapshot revision conflicts with saved content")
		}
	}
	if !found && len(current.Sources) >= MaxSources {
		return errors.New("fleet source limit reached (32); preserve and remove unused expired source files explicitly")
	}
	f, err := os.CreateTemp(dir, ".fleet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, snapshot.ControllerID+".json")); err != nil {
		return err
	}
	return syncDirectory(dir)
}
