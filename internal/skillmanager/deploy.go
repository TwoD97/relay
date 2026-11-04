package skillmanager

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type WorkerRequest struct {
	Action    string   `json:"action"`
	Name      string   `json:"name,omitempty"`
	Digest    string   `json:"digest,omitempty"`
	Operation string   `json:"operation,omitempty"`
	Package   *Package `json:"package,omitempty"`
}
type ProviderStatus struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Digest   string `json:"digest,omitempty"`
	Detail   string `json:"detail,omitempty"`
}
type Deployment struct {
	Name      string           `json:"name"`
	Providers []ProviderStatus `json:"providers"`
}
type WorkerResult struct {
	Protocol    int          `json:"protocol"`
	Deployments []Deployment `json:"deployments"`
}
type pending struct {
	Operation string `json:"operation"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
}
type registration struct {
	Digest  string   `json:"digest,omitempty"`
	Pending *pending `json:"pending,omitempty"`
}
type receipt struct {
	Format    int                     `json:"format"`
	Name      string                  `json:"name"`
	Providers map[string]registration `json:"providers"`
}

var providers = []string{"claude", "codex"}

func providerRoot(home, provider string) string {
	dir := ".claude"
	if provider == "codex" {
		dir = ".agents"
	}
	return filepath.Join(home, dir)
}
func receiptPath(base, name string) string { return filepath.Join(base, "deployments", name+".json") }
func loadReceipt(base, name string) (receipt, error) {
	r := receipt{Format: 1, Name: name, Providers: map[string]registration{}}
	err := readJSON(receiptPath(base, name), &r, 16<<10)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if r.Format != 1 || r.Name != name || r.Providers == nil || len(r.Providers) > 2 {
		return r, errors.New("Invalid skill deployment receipt; existing files preserved")
	}
	for id, v := range r.Providers {
		if (id != "claude" && id != "codex") || (v.Digest != "" && !ValidDigest(v.Digest)) {
			return r, errors.New("Invalid skill registration")
		}
		if p := v.Pending; p != nil {
			if len(p.Operation) != 32 || !digestPattern.MatchString(p.Operation+p.Operation) || (p.Before != "" && !ValidDigest(p.Before)) || (p.After != "" && !ValidDigest(p.After)) || p.Before != v.Digest {
				return r, errors.New("Invalid pending skill registration")
			}
		}
	}
	return r, nil
}
func saveReceipt(base string, r receipt) error { return atomicJSON(receiptPath(base, r.Name), r) }
func checkDir(path string) error {
	if path != filepath.Dir(path) {
		if err := checkDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	m, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !m.IsDir() || linked(m) {
		return errors.New("Skill path contains a linked or non-directory parent")
	}
	return nil
}
func matches(dir string, p Package) error {
	if err := checkDir(dir); err != nil {
		return err
	}
	wanted := map[string]File{}
	dirs := map[string]bool{".": true}
	for _, f := range p.Files {
		wanted[f.Path] = f
		for d := filepath.Dir(filepath.FromSlash(f.Path)); d != "."; d = filepath.Dir(d) {
			dirs[filepath.ToSlash(d)] = true
		}
	}
	count := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if linked(info) {
			return errors.New("Skill contains linked files")
		}
		if entry.IsDir() {
			if !dirs[rel] {
				return errors.New("Skill contains additional directories")
			}
			return nil
		}
		f, ok := wanted[rel]
		if !ok || !info.Mode().IsRegular() {
			return errors.New("Skill contains additional or unsupported files")
		}
		b, e := readFile(path, MaxBytes)
		if e != nil {
			return e
		}
		if !bytes.Equal(b, f.Data) || (runtime.GOOS != "windows" && (info.Mode()&0111 != 0) != f.Executable) {
			return errors.New("Skill was edited outside Relay")
		}
		count++
		return nil
	})
	if err == nil && count != len(p.Files) {
		err = errors.New("Skill files are missing")
	}
	return err
}
func checkDigest(base, dir, digest string) error {
	var p Package
	if err := readPackage(base, digest, &p); err != nil {
		return err
	}
	return matches(dir, p)
}
func writeTree(dir string, p Package) error {
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		return errors.New("Skill staging path already exists")
	}
	if err := ensureDir(dir); err != nil {
		return err
	}
	for _, item := range p.Files {
		path := filepath.Join(dir, filepath.FromSlash(item.Path))
		if err := ensureDir(filepath.Dir(path)); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if item.Executable {
			mode = 0700
		}
		f, err := createFile(path, mode)
		if err != nil {
			return err
		}
		_, err = f.Write(item.Data)
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return err
		}
	}
	return syncDir(dir)
}
func removeChecked(base, dir, digest string) error {
	if err := checkDigest(base, dir, digest); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
func pathMissing(path string) bool { _, err := os.Lstat(path); return os.IsNotExist(err) }
func snapshot(base, home string, r receipt) Deployment {
	d := Deployment{Name: r.Name, Providers: []ProviderStatus{}}
	for _, provider := range providers {
		v := r.Providers[provider]
		status := ProviderStatus{Provider: provider, Digest: v.Digest}
		target := filepath.Join(providerRoot(home, provider), "skills", r.Name)
		switch {
		case v.Pending != nil:
			status.Status = "uncertain"
			status.Detail = "A previous filesystem operation is unconfirmed. Review deployment before another change."
		case v.Digest != "":
			if err := checkDigest(base, target, v.Digest); err == nil {
				status.Status = "installed"
			} else {
				status.Status = "conflict"
				status.Detail = "Managed files changed or are unavailable. Existing files were preserved."
			}
		case pathMissing(target):
			status.Status = "absent"
		default:
			status.Status = "conflict"
			status.Detail = "An existing skill is not owned by Relay and was preserved."
		}
		d.Providers = append(d.Providers, status)
	}
	return d
}
func reconcile(base, home string, r *receipt) error {
	for _, provider := range providers {
		v := r.Providers[provider]
		p := v.Pending
		if p == nil {
			continue
		}
		root := providerRoot(home, provider)
		target := filepath.Join(root, "skills", r.Name)
		stage := filepath.Join(root, ".relay-skills-stage", p.Operation)
		backup := filepath.Join(root, ".relay-skills-stage", p.Operation+"-previous")
		if p.After != "" && checkDigest(base, target, p.After) == nil {
			v.Digest = p.After
		} else if p.After == "" && pathMissing(target) && (p.Before == "" || checkDigest(base, backup, p.Before) == nil) {
			v.Digest = ""
		} else if p.Before != "" && checkDigest(base, target, p.Before) == nil {
			v.Digest = p.Before
		} else if p.Before != "" && pathMissing(target) && checkDigest(base, backup, p.Before) == nil {
			if err := renameNew(backup, target); err != nil {
				return err
			}
			v.Digest = p.Before
		} else if p.Before == "" && pathMissing(target) {
			v.Digest = ""
		} else {
			return errors.New("Skill files do not match the saved operation. Existing files were preserved")
		}
		// Only exact recorded bytes may be cleaned. Unrecognized stage/backup trees
		// block reconciliation instead of being recursively removed.
		if !pathMissing(stage) {
			if p.After == "" {
				return errors.New("Unexpected skill stage preserved")
			}
			if err := removeChecked(base, stage, p.After); err != nil {
				return err
			}
		}
		if !pathMissing(backup) {
			if p.Before == "" {
				return errors.New("Unexpected skill backup preserved")
			}
			if err := removeChecked(base, backup, p.Before); err != nil {
				return err
			}
		}
		v.Pending = nil
		r.Providers[provider] = v
		if err := saveReceipt(base, *r); err != nil {
			return err
		}
	}
	return nil
}
func Worker(base, home string, request WorkerRequest) (result WorkerResult, err error) {
	result = WorkerResult{Protocol: 1, Deployments: []Deployment{}}
	if !filepath.IsAbs(home) || !filepath.IsAbs(base) {
		return result, errors.New("Skills require absolute private storage paths")
	}
	if request.Action != "status" && request.Action != "deploy" && request.Action != "remove" && request.Action != "reconcile" {
		return result, errors.New("Invalid skill operation")
	}
	if request.Name != "" && !ValidName(request.Name) {
		return result, errors.New("Invalid skill name")
	}
	if request.Action != "status" && request.Name == "" {
		return result, errors.New("Select a skill")
	}
	var p Package
	if request.Action == "deploy" {
		if request.Package == nil {
			return result, errors.New("Skill package is missing")
		}
		p, err = Validate(*request.Package)
		if err != nil {
			return result, err
		}
		if p.Name != request.Name || p.Digest != request.Digest {
			return result, errors.New("Skill deployment identity changed")
		}
	}
	err = withLock(base, func() error {
		if e := ensureDir(filepath.Join(base, "deployments")); e != nil {
			return e
		}
		if request.Action == "status" {
			names := []string{}
			if request.Name != "" {
				names = append(names, request.Name)
			} else {
				entries, e := os.ReadDir(filepath.Join(base, "deployments"))
				if e != nil {
					return e
				}
				if len(entries) > 128 {
					return errors.New("Too many saved skill deployments")
				}
				for _, entry := range entries {
					name := strings.TrimSuffix(entry.Name(), ".json")
					if ValidName(name) && entry.Name() == name+".json" {
						names = append(names, name)
					}
				}
			}
			for _, name := range names {
				r, e := loadReceipt(base, name)
				if e != nil {
					return e
				}
				result.Deployments = append(result.Deployments, snapshot(base, home, r))
			}
			return nil
		}
		r, e := loadReceipt(base, request.Name)
		if e != nil {
			return e
		}
		if request.Action == "reconcile" {
			if e = reconcile(base, home, &r); e != nil {
				return e
			}
			result.Deployments = append(result.Deployments, snapshot(base, home, r))
			return nil
		}
		if !ValidDigest(request.Digest) {
			return errors.New("A verified skill digest is required")
		}
		// Check both registrations before changing either one. A collision on one
		// provider must not silently replace or partially publish the other.
		current := snapshot(base, home, r)
		for _, status := range current.Providers {
			if status.Status == "conflict" || status.Status == "uncertain" {
				result.Deployments = append(result.Deployments, current)
				return errors.New("Skill deployment needs review; existing files were preserved")
			}
			if request.Action == "remove" && status.Digest != "" && status.Digest != request.Digest {
				return ErrConflict
			}
		}
		if request.Action == "deploy" {
			if pathMissing(receiptPath(base, request.Name)) {
				entries, err := os.ReadDir(filepath.Join(base, "deployments"))
				if err != nil {
					return err
				}
				if len(entries) >= 128 {
					return errors.New("Managed skill deployment limit reached")
				}
			}
			if e = storePackage(base, p); e != nil {
				return e
			}
		}
		for _, provider := range providers {
			v := r.Providers[provider]
			before := v.Digest
			after := ""
			if request.Action == "deploy" {
				after = request.Digest
			}
			if after == v.Digest {
				continue
			}
			root := providerRoot(home, provider)
			parent := filepath.Join(root, "skills")
			stageRoot := filepath.Join(root, ".relay-skills-stage")
			if e = ensureDir(parent); e != nil {
				return e
			}
			if e = ensureDir(stageRoot); e != nil {
				return e
			}
			staged, e := os.ReadDir(stageRoot)
			if e != nil {
				return e
			}
			if len(staged) >= 64 {
				return errors.New("Skill staging needs review before more packages can be installed")
			}
			operation, e := randomID()
			if e != nil {
				return e
			}
			stage := filepath.Join(stageRoot, operation)
			backup := filepath.Join(stageRoot, operation+"-previous")
			target := filepath.Join(parent, request.Name)
			if after != "" {
				if e = writeTree(stage, p); e != nil {
					return e
				}
			}
			v.Pending = &pending{Operation: operation, Before: v.Digest, After: after}
			r.Providers[provider] = v
			if e = saveReceipt(base, r); e != nil {
				return fmt.Errorf("Skill preparation was not confirmed; review before retrying: %w", e)
			}
			if v.Digest != "" {
				if e = checkDigest(base, target, v.Digest); e != nil {
					return e
				}
				if e = renameNew(target, backup); e != nil {
					return e
				}
			} else if !pathMissing(target) {
				return errors.New("A new external skill appeared; it was preserved")
			}
			if after != "" {
				if e = renameNew(stage, target); e != nil {
					return e
				}
			}
			if e = syncDir(parent); e != nil {
				return e
			}
			// Record the completed identity before discarding the previous bytes.
			v.Digest = after
			v.Pending = nil
			r.Providers[provider] = v
			if e = saveReceipt(base, r); e != nil {
				return fmt.Errorf("Skill publication was not confirmed; review before retrying: %w", e)
			}
			if !pathMissing(backup) {
				if e = removeChecked(base, backup, before); e != nil { /* old bytes are safe to retain; never touch unknown files */
				}
			}
		}
		result.Deployments = append(result.Deployments, snapshot(base, home, r))
		return nil
	})
	return
}
