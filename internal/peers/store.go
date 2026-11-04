// Package peers stores source-owned, inert workspace snapshots. The controller
// selects shareable fields; this store never executes or merges their contents.
package peers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxBytes = 4 << 20
const MaxSources = 8

var (
	ErrConflict = errors.New("Peer snapshot changed; read it before publishing")
	ErrNotFound = errors.New("Peer source not found")
	ErrCapacity = errors.New("Peer snapshot capacity reached")
	ErrInvalid  = errors.New("Invalid peer snapshot")
	ErrCorrupt  = errors.New("Saved peer snapshot is invalid and was preserved")
	ErrBusy     = errors.New("Peer snapshot store is busy; read again shortly")
)

type Snapshot struct {
	SourceID  string          `json:"sourceId"`
	Name      string          `json:"name"`
	Revision  uint64          `json:"revision"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Data      json.RawMessage `json:"data"`
}
type Record struct {
	Snapshot
	Digest string `json:"digest"`
}
type Summary struct {
	SourceID  string    `json:"sourceId"`
	Name      string    `json:"name"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
	Digest    string    `json:"digest"`
	Bytes     int       `json:"bytes"`
}
type SourceError struct {
	SourceID string `json:"sourceId"`
	Error    string `json:"error"`
}
type Index struct {
	Sources []Summary     `json:"sources"`
	Errors  []SourceError `json:"errors,omitempty"`
}
type Store struct {
	mu        sync.Mutex
	directory string
}

// ValidateRecord checks the exact canonical snapshot digest before an importing
// controller treats a peer response as shareable metadata.
func ValidateRecord(record Record) error {
	normalized, _, err := normalize(record.Snapshot)
	if err != nil || !validDigest(record.Digest) || record.Digest != normalized.Digest {
		return ErrCorrupt
	}
	return nil
}

func ValidSource(id string) bool {
	if len(id) != 48 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func normalize(snapshot Snapshot) (Record, []byte, error) {
	if !ValidSource(snapshot.SourceID) || len(snapshot.Name) == 0 || len(snapshot.Name) > 80 || !utf8.ValidString(snapshot.Name) || strings.TrimSpace(snapshot.Name) == "" || strings.ContainsFunc(snapshot.Name, func(r rune) bool { return !unicode.IsPrint(r) }) || snapshot.Revision == 0 || snapshot.Revision >= 1<<53 || snapshot.UpdatedAt.IsZero() || len(snapshot.Data) == 0 || len(snapshot.Data) > MaxBytes {
		return Record{}, nil, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(snapshot.Data))
	decoder.UseNumber()
	var value map[string]any
	if decoder.Decode(&value) != nil || value == nil || decoder.Decode(new(any)) != io.EOF || !boundedDepth(value, 0) {
		return Record{}, nil, ErrInvalid
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return Record{}, nil, ErrInvalid
	}
	snapshot.Data = canonical
	snapshot.UpdatedAt = snapshot.UpdatedAt.UTC()
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > MaxBytes {
		return Record{}, nil, ErrInvalid
	}
	digest := sha256.Sum256(encoded)
	return Record{Snapshot: snapshot, Digest: hex.EncodeToString(digest[:])}, encoded, nil
}
func boundedDepth(value any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch value := value.(type) {
	case map[string]any:
		for _, child := range value {
			if !boundedDepth(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !boundedDepth(child, depth+1) {
				return false
			}
		}
	}
	return true
}
func Open(directory string) (*Store, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = privateDirectory(directory); err != nil {
		return nil, err
	}
	return &Store{directory: directory}, nil
}
func (s *Store) locked(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockDirectory(s.directory)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}
func (s *Store) sourceIDs() ([]string, error) {
	f, err := os.Open(s.directory)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(65)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 64 {
		return nil, ErrCapacity
	}
	ids := []string{}
	for _, entry := range entries {
		if id := strings.TrimSuffix(entry.Name(), ".json"); ValidSource(id) && entry.Name() == id+".json" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
func (s *Store) read(id string) (Record, error) {
	if !ValidSource(id) {
		return Record{}, ErrInvalid
	}
	f, err := openRegular(filepath.Join(s.directory, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrCorrupt
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBytes+256 {
		return Record{}, ErrCorrupt
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+257))
	if err != nil || len(data) > MaxBytes+256 {
		return Record{}, ErrCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.SourceID != id {
		return Record{}, ErrCorrupt
	}
	normalized, _, err := normalize(record.Snapshot)
	if err != nil || record.Digest != normalized.Digest {
		return Record{}, ErrCorrupt
	}
	return normalized, nil
}
func (s *Store) List() (result Index, err error) {
	result.Sources = []Summary{}
	err = s.locked(func() error {
		ids, e := s.sourceIDs()
		if e != nil {
			return e
		}
		if len(ids) > MaxSources {
			return ErrCapacity
		}
		for _, id := range ids {
			record, e := s.read(id)
			if e != nil {
				result.Errors = append(result.Errors, SourceError{id, ErrCorrupt.Error()})
				continue
			}
			_, data, _ := normalize(record.Snapshot)
			result.Sources = append(result.Sources, Summary{record.SourceID, record.Name, record.Revision, record.UpdatedAt, record.Digest, len(data)})
		}
		return nil
	})
	return
}
func (s *Store) Get(id string) (result Record, err error) {
	err = s.locked(func() error { result, err = s.read(id); return err })
	return
}
func (s *Store) Put(expectedDigest string, snapshot Snapshot) (result Record, err error) {
	result, _, err = normalize(snapshot)
	if err != nil {
		return Record{}, err
	}
	if expectedDigest != "" && !validDigest(expectedDigest) {
		return Record{}, ErrInvalid
	}
	err = s.locked(func() error {
		current, e := s.read(snapshot.SourceID)
		if e == nil {
			if current.Digest == result.Digest {
				return nil
			}
			if expectedDigest != current.Digest || snapshot.Revision <= current.Revision {
				return ErrConflict
			}
		} else if errors.Is(e, ErrNotFound) {
			if expectedDigest != "" {
				return ErrConflict
			}
			ids, e := s.sourceIDs()
			if e != nil {
				return e
			}
			if len(ids) >= MaxSources {
				return ErrCapacity
			}
		} else {
			return e
		}
		data, e := json.Marshal(result)
		if e != nil {
			return e
		}
		return s.write(snapshot.SourceID, data)
	})
	if err != nil {
		return Record{}, err
	}
	return result, nil
}
func (s *Store) Delete(id, expectedDigest string) error {
	if !ValidSource(id) || !validDigest(expectedDigest) {
		return ErrInvalid
	}
	return s.locked(func() error {
		record, err := s.read(id)
		if err != nil {
			return err
		}
		if record.Digest != expectedDigest {
			return ErrConflict
		}
		if err = os.Remove(filepath.Join(s.directory, id+".json")); err != nil {
			return err
		}
		return syncDirectory(s.directory)
	})
}
func (s *Store) write(id string, data []byte) error {
	f, err := os.CreateTemp(s.directory, ".peer-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = replaceFile(name, filepath.Join(s.directory, id+".json")); err != nil {
		return err
	}
	if err = syncDirectory(s.directory); err != nil {
		return fmt.Errorf("peer publication was not confirmed; read before another attempt: %w", err)
	}
	return nil
}
