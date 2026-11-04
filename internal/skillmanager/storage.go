package skillmanager

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

type Catalog struct {
	Revision uint64  `json:"revision"`
	Entries  []Entry `json:"entries"`
}
type catalogRecord struct {
	Format int `json:"format"`
	Catalog
}

var ErrConflict = errors.New("Skills changed. Reload the library before editing")

func ensureDir(path string) error {
	if path == filepath.Dir(path) {
		return nil
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	m, err := os.Lstat(path)
	if err == nil {
		if !m.IsDir() || linked(m) {
			return errors.New("Skill directory must be a real directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err = ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err = makeDir(path); err != nil && !os.IsExist(err) {
		return err
	}
	return ensureDir(path)
}
func readFile(path string, limit int) ([]byte, error) {
	f, err := openRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !m.Mode().IsRegular() || linked(m) || m.Size() > int64(limit) {
		return nil, errors.New("Skill data must be a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(b) > limit {
		return nil, errors.New("Skill data exceeds its limit")
	}
	return b, err
}
func readJSON(path string, out any, limit int) error {
	b, err := readFile(path, limit)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return errors.New("Saved skill data is invalid; the file was preserved")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("Saved skill data has trailing content")
	}
	return nil
}
func atomicJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicBytes(path, b)
}
func atomicBytes(path string, b []byte) error {
	if m, err := os.Lstat(path); err == nil && (!m.Mode().IsRegular() || linked(m)) {
		return errors.New("Skill data path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	tmp, err := randomID()
	if err != nil {
		return err
	}
	name := filepath.Join(filepath.Dir(path), ".skill-"+tmp+".tmp")
	f, err := createFile(name, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = replaceFile(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func withLock(base string, fn func() error) error {
	if err := ensureDir(base); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(base, "store.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}
func loadCatalog(base string) (Catalog, error) {
	var saved catalogRecord
	err := readJSON(filepath.Join(base, "catalog.json"), &saved, 512<<10)
	if os.IsNotExist(err) {
		return Catalog{Entries: []Entry{}}, nil
	}
	if err != nil {
		return Catalog{}, err
	}
	if saved.Format != 1 || saved.Revision >= 1<<53 || len(saved.Entries) > 64 {
		return Catalog{}, errors.New("Unsupported skills library")
	}
	seen := map[string]bool{}
	for _, e := range saved.Entries {
		if !ValidName(e.Name) || !ValidDigest(e.Digest) || seen[e.Name] || e.Bytes < 1 || e.Bytes > MaxBytes || e.Files < 1 || e.Files > MaxFiles || len(e.Description) > 4096 {
			return Catalog{}, errors.New("Invalid skills library")
		}
		seen[e.Name] = true
	}
	return saved.Catalog, nil
}
func List(base string) (result Catalog, err error) {
	err = withLock(base, func() error { result, err = loadCatalog(base); return err })
	return
}
func Get(base, name string) (result Package, err error) {
	if !ValidName(name) {
		return result, errors.New("Invalid skill name")
	}
	err = withLock(base, func() error {
		catalog, e := loadCatalog(base)
		if e != nil {
			return e
		}
		for _, entry := range catalog.Entries {
			if entry.Name == name {
				if e = readPackage(base, entry.Digest, &result); e != nil {
					return e
				}
				if result.Name != entry.Name {
					return errors.New("Skill library identity changed")
				}
				return nil
			}
		}
		return os.ErrNotExist
	})
	return
}
func readPackage(base, digest string, p *Package) error {
	if !ValidDigest(digest) {
		return errors.New("Invalid skill digest")
	}
	if err := readJSON(filepath.Join(base, "packages", digest+".json"), p, 6<<20); err != nil {
		return err
	}
	if p.Digest != digest {
		return errors.New("Skill package does not match its stored digest")
	}
	_, err := Validate(*p)
	return err
}
func Import(base string, revision uint64, expected string, p Package) (result Catalog, err error) {
	p, err = Validate(p)
	if err != nil {
		return
	}
	err = withLock(base, func() error {
		c, e := loadCatalog(base)
		if e != nil {
			return e
		}
		if c.Revision != revision {
			return ErrConflict
		}
		index := -1
		for i, v := range c.Entries {
			if v.Name == p.Name {
				index = i
				if v.Digest == p.Digest {
					result = c
					return nil
				}
				if expected != v.Digest {
					return ErrConflict
				}
			}
		}
		if index < 0 && expected != "" {
			return ErrConflict
		}
		if index < 0 && len(c.Entries) >= 64 {
			return errors.New("The library supports up to 64 skills")
		}
		if e = storePackage(base, p); e != nil {
			return e
		}
		if index < 0 {
			c.Entries = append(c.Entries, p.Entry())
		} else {
			c.Entries[index] = p.Entry()
		}
		sort.Slice(c.Entries, func(i, j int) bool { return c.Entries[i].Name < c.Entries[j].Name })
		c.Revision++
		if e = atomicJSON(filepath.Join(base, "catalog.json"), catalogRecord{Format: 1, Catalog: c}); e != nil {
			return fmt.Errorf("Library save was not confirmed; reload before editing: %w", e)
		}
		result = c
		return nil
	})
	return
}
func Remove(base, name, digest string, revision uint64) (result Catalog, err error) {
	err = withLock(base, func() error {
		c, e := loadCatalog(base)
		if e != nil {
			return e
		}
		if c.Revision != revision {
			return ErrConflict
		}
		found := -1
		for i, v := range c.Entries {
			if v.Name == name && v.Digest == digest {
				found = i
			}
		}
		if found < 0 {
			return ErrConflict
		}
		c.Entries = append(c.Entries[:found], c.Entries[found+1:]...)
		c.Revision++
		if e = atomicJSON(filepath.Join(base, "catalog.json"), catalogRecord{Format: 1, Catalog: c}); e != nil {
			return fmt.Errorf("Library removal was not confirmed; reload before editing: %w", e)
		}
		result = c
		return nil
	})
	return
}

func randomID() (string, error) {
	var b [16]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}

// Host packages are independent of controller versions and provider credentials.
func DefaultHostStore() (string, error) {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if !filepath.IsAbs(base) {
			return "", errors.New("Local application data directory is unavailable")
		}
		return filepath.Join(base, "RelayData", "shared-skills"), nil
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(home, ".local", "share", "relay", "shared-skills"), nil
}

func storePackage(base string, p Package) error {
	if err := ensureDir(filepath.Join(base, "packages")); err != nil {
		return err
	}
	var existing Package
	err := readPackage(base, p.Digest, &existing)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(base, "packages"))
	if err != nil {
		return err
	}
	if len(entries) >= 128 {
		return errors.New("Managed skill storage is full; remove unused skill packages before importing more")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	total := int64(len(data))
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			return e
		}
		total += info.Size()
	}
	if total > 512<<20 {
		return errors.New("Managed skill storage exceeds its 512 MiB limit")
	}
	return atomicBytes(filepath.Join(base, "packages", p.Digest+".json"), data)
}
