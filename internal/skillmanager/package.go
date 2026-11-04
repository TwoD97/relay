// Package skillmanager stores user-imported skills as data. Imports are never executed.
package skillmanager

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const MaxBytes = 4 << 20
const MaxFiles = 64

var slug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type File struct {
	Path       string `json:"path"`
	Data       []byte `json:"data"`
	Executable bool   `json:"executable,omitempty"`
}
type Package struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Files       []File `json:"files"`
}
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest"`
	Bytes       int    `json:"bytes"`
	Files       int    `json:"files"`
}

func ValidName(name string) bool {
	return len(name) > 0 && len(name) <= 64 && slug.MatchString(name) && name != "relay" && name != "relay-mastermind"
}
func ValidDigest(digest string) bool { return digestPattern.MatchString(digest) }
func (p Package) Entry() Entry {
	e := Entry{Name: p.Name, Description: p.Description, Digest: p.Digest, Files: len(p.Files)}
	for _, f := range p.Files {
		e.Bytes += len(f.Data)
	}
	return e
}
func validPath(name string) bool {
	if name == "" || len(name) > 240 || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || regexp.MustCompile(`^(COM|LPT)[0-9]$`).MatchString(base) {
			return false
		}
		for _, r := range part {
			if r < 32 || strings.ContainsRune(`<>"|?*`, r) {
				return false
			}
		}
	}
	return utf8.ValidString(name)
}
func Parse(data []byte, archive bool) (Package, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return Package{}, errors.New("Skill imports must be between 1 byte and 4 MiB")
	}
	var files []File
	if !archive {
		files = []File{{Path: "SKILL.md", Data: append([]byte(nil), data...)}}
	} else {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return Package{}, errors.New("Could not read the skill ZIP")
		}
		if len(z.File) > MaxFiles*2 {
			return Package{}, errors.New("Skill ZIP has too many entries")
		}
		total := 0
		for _, f := range z.File {
			if f.Mode().IsDir() {
				if !validPath(strings.TrimSuffix(f.Name, "/")) {
					return Package{}, errors.New("Skill ZIP has an unsafe directory")
				}
				continue
			}
			if !f.Mode().IsRegular() || !validPath(f.Name) || f.UncompressedSize64 > MaxBytes {
				return Package{}, errors.New("Skill ZIP must contain bounded regular files with relative paths")
			}
			r, err := f.Open()
			if err != nil {
				return Package{}, errors.New("Could not open a skill file")
			}
			b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
			_ = r.Close()
			total += len(b)
			if err != nil || total > MaxBytes || len(files) >= MaxFiles {
				return Package{}, errors.New("Skill ZIP exceeds 4 MiB or 64 files")
			}
			files = append(files, File{Path: f.Name, Data: b, Executable: f.Mode()&0111 != 0})
		}
		root := false
		for _, f := range files {
			if f.Path == "SKILL.md" {
				root = true
			}
		}
		if !root && len(files) > 0 {
			prefix := strings.SplitN(files[0].Path, "/", 2)[0] + "/"
			for i := range files {
				if !strings.HasPrefix(files[i].Path, prefix) {
					return Package{}, errors.New("Skill ZIP must have one skill root")
				}
				files[i].Path = strings.TrimPrefix(files[i].Path, prefix)
			}
		}
	}
	return normalize(files)
}
func normalize(files []File) (Package, error) {
	p := Package{Files: files}
	if len(files) == 0 || len(files) > MaxFiles {
		return p, errors.New("A skill must contain SKILL.md and at most 64 files")
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	seen := map[string]bool{}
	total := 0
	var markdown []byte
	for _, f := range p.Files {
		key := strings.ToLower(f.Path)
		if !validPath(f.Path) || seen[key] {
			return p, errors.New("Skill files have unsafe or duplicate paths")
		}
		seen[key] = true
		total += len(f.Data)
		if total > MaxBytes {
			return p, errors.New("Skill exceeds 4 MiB")
		}
		if f.Path == "SKILL.md" {
			markdown = f.Data
		}
	}
	for key := range seen {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return p, errors.New("Skill file collides with a directory")
			}
		}
	}
	if len(markdown) == 0 || len(markdown) > 128<<10 || !utf8.Valid(markdown) {
		return p, errors.New("SKILL.md must be UTF-8 text no larger than 128 KiB")
	}
	text := strings.ReplaceAll(string(markdown), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return p, errors.New("SKILL.md requires YAML name and description frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return p, errors.New("SKILL.md frontmatter is incomplete")
	}
	var header map[string]any
	if err := yaml.Unmarshal([]byte(text[4:4+end]+"\n"), &header); err != nil {
		return p, errors.New("SKILL.md frontmatter is invalid")
	}
	p.Name, _ = header["name"].(string)
	p.Description, _ = header["description"].(string)
	if !ValidName(p.Name) {
		return p, errors.New("Skill name must use up to 64 lowercase letters, digits and single hyphens; relay and relay-mastermind are reserved")
	}
	if p.Name == "synced" || p.Name == "anthropic-skills" {
		return p, errors.New("This skill name is reserved by Claude Code and cannot be shared with both providers")
	}
	if strings.TrimSpace(p.Description) == "" || len(p.Description) > 4096 || strings.ContainsRune(p.Description, 0) {
		return p, errors.New("Skill description must be nonempty and no larger than 4 KiB")
	}
	payload, _ := json.Marshal(p.Files)
	sum := sha256.Sum256(payload)
	p.Digest = hex.EncodeToString(sum[:])
	return p, nil
}
func Validate(p Package) (Package, error) {
	n, err := normalize(p.Files)
	if err != nil {
		return n, err
	}
	if p.Name != n.Name || p.Description != n.Description || p.Digest != n.Digest {
		return n, errors.New("Skill package identity or digest does not match its files")
	}
	return n, nil
}
func Export(p Package) ([]byte, error) {
	p, err := Validate(p)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for _, f := range p.Files {
		h := &zip.FileHeader{Name: p.Name + "/" + f.Path, Method: zip.Deflate}
		if f.Executable {
			h.SetMode(0700)
		} else {
			h.SetMode(0600)
		}
		w, e := z.CreateHeader(h)
		if e != nil {
			return nil, e
		}
		if _, e = w.Write(f.Data); e != nil {
			return nil, e
		}
	}
	if err = z.Close(); err != nil {
		return nil, fmt.Errorf("export skill: %w", err)
	}
	return out.Bytes(), nil
}
