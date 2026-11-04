package skillmanager

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T, body string) Package {
	t.Helper()
	p, e := normalize([]File{{Path: "SKILL.md", Data: []byte("---\nname: test-skill\ndescription: |\n  An isolated test skill.\n---\n" + body)}, {Path: "scripts/helper.sh", Data: []byte("#!/bin/sh\nprintf fixture\n"), Executable: true}})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestPackageRoundTripAndFrontmatter(t *testing.T) {
	p := fixture(t, "Read the fixture.")
	b, e := Export(p)
	if e != nil {
		t.Fatal(e)
	}
	got, e := Parse(b, true)
	if e != nil || got.Digest != p.Digest || len(got.Files) != 2 {
		t.Fatal(got, e)
	}
	if got.Description != "An isolated test skill.\n" {
		t.Fatal(got.Description)
	}
	for _, text := range []string{"name: relay\ndescription: Reserved", "name: relay-mastermind\ndescription: Reserved", "name: synced\ndescription: Provider reserved", "name: ok\nname: duplicate\ndescription: invalid", "name: ../escape\ndescription: invalid"} {
		if _, e = Parse([]byte("---\n"+text+"\n---\nbody"), false); e == nil {
			t.Fatal("accepted", text)
		}
	}
}
func TestPackageRejectsTraversalLinksDuplicateAndExpandedLimits(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a\\b", "a/../b", "C:/evil", "x/NUL.txt", "a//b", ".claude/settings.json"} {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		w, _ := z.Create(name)
		_, _ = w.Write([]byte("bad"))
		_ = z.Close()
		if _, e := Parse(b.Bytes(), true); e == nil {
			t.Fatal("accepted", name)
		}
	}
	for _, kind := range []string{"link", "duplicate", "bomb", "directory collision"} {
		t.Run(kind, func(t *testing.T) {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			w, _ := z.Create("SKILL.md")
			_, _ = w.Write(fixture(t, "").Files[0].Data)
			switch kind {
			case "link":
				h := &zip.FileHeader{Name: "escape"}
				h.SetMode(os.ModeSymlink | 0777)
				w, _ = z.CreateHeader(h)
				_, _ = w.Write([]byte("/tmp/target"))
			case "duplicate":
				w, _ = z.Create("skill.md")
				_, _ = w.Write([]byte("bad"))
			case "bomb":
				w, _ = z.Create("large")
				_, _ = w.Write(make([]byte, MaxBytes))
			case "directory collision":
				_, _ = z.Create("scripts")
				_, _ = z.Create("scripts/child")
			}
			_ = z.Close()
			if _, e := Parse(b.Bytes(), true); e == nil {
				t.Fatal("accepted", kind)
			}
		})
	}
}
func TestCatalogCASAndCorruptionPreserved(t *testing.T) {
	base := filepath.Join(t.TempDir(), "library")
	p := fixture(t, "one")
	c, e := Import(base, 0, "", p)
	if e != nil || c.Revision != 1 {
		t.Fatal(c, e)
	}
	newP := fixture(t, "two")
	if _, e = Import(base, 0, p.Digest, newP); e != ErrConflict {
		t.Fatal(e)
	}
	if _, e = Import(base, 1, "", newP); e != ErrConflict {
		t.Fatal(e)
	}
	c, e = Import(base, 1, p.Digest, newP)
	if e != nil || c.Revision != 2 {
		t.Fatal(c, e)
	}
	got, e := Get(base, p.Name)
	if e != nil || got.Digest != newP.Digest {
		t.Fatal(got, e)
	}
	if _, e = Remove(base, p.Name, p.Digest, 2); e != ErrConflict {
		t.Fatal(e)
	}
	c, e = Remove(base, p.Name, newP.Digest, 2)
	if e != nil || len(c.Entries) != 0 {
		t.Fatal(c, e)
	}
	file := filepath.Join(base, "catalog.json")
	_ = os.WriteFile(file, []byte("invalid"), 0600)
	if _, e = Import(base, 0, "", p); e == nil {
		t.Fatal("replaced corrupt catalog")
	}
	b, _ := os.ReadFile(file)
	if string(b) != "invalid" {
		t.Fatal("corrupt data changed")
	}
}
func TestConcurrentCatalogImportsCommitOnce(t *testing.T) {
	base := filepath.Join(t.TempDir(), "library")
	p := fixture(t, "one")
	var wg sync.WaitGroup
	var mu sync.Mutex
	count := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := Import(base, 0, "", p); e == nil {
				mu.Lock()
				count++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if count != 1 {
		t.Fatal(count)
	}
}
func TestDeployUpdateRemovePreserveUnrelatedAndExecuteNothing(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "managed")
	p := fixture(t, "never execute scripts")
	req := WorkerRequest{Action: "deploy", Name: p.Name, Digest: p.Digest, Package: &p}
	r, e := Worker(base, home, req)
	if e != nil {
		t.Fatal(e)
	}
	for _, status := range r.Deployments[0].Providers {
		if status.Status != "installed" {
			t.Fatal(status)
		}
	}
	for _, provider := range providers {
		if e = matches(filepath.Join(providerRoot(home, provider), "skills", p.Name), p); e != nil {
			t.Fatal(e)
		}
	}
	sentinel := filepath.Join(home, "sentinel")
	_ = os.WriteFile(sentinel, []byte("keep"), 0600)
	p2 := fixture(t, "updated")
	req.Digest = p2.Digest
	req.Package = &p2
	if _, e = Worker(base, home, req); e != nil {
		t.Fatal(e)
	}
	r, e = Worker(base, home, WorkerRequest{Action: "remove", Name: p.Name, Digest: p2.Digest})
	if e != nil {
		t.Fatal(e)
	}
	for _, status := range r.Deployments[0].Providers {
		if status.Status != "absent" {
			t.Fatal(status)
		}
	}
	if b, _ := os.ReadFile(sentinel); string(b) != "keep" {
		t.Fatal("sentinel changed")
	}
}
func TestUnknownOrModifiedProviderSkillBlocksBoth(t *testing.T) {
	for _, scenario := range []string{"external", "edited", "extra directory", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			base := filepath.Join(home, "managed")
			p := fixture(t, "original")
			target := filepath.Join(providerRoot(home, "codex"), "skills", p.Name)
			if scenario != "external" {
				if _, e := Worker(base, home, WorkerRequest{Action: "deploy", Name: p.Name, Digest: p.Digest, Package: &p}); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := ensureDir(target); e != nil {
					t.Fatal(e)
				}
			}
			switch scenario {
			case "external", "edited":
				_ = os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("User skill"), 0600)
			case "extra directory":
				_ = os.Mkdir(filepath.Join(target, "custom"), 0700)
			case "symlink":
				if e := os.Symlink(filepath.Join(home, "sentinel"), filepath.Join(target, "unexpected")); e != nil {
					t.Skip("symlink unavailable")
				}
			}
			p2 := fixture(t, "updated")
			if _, e := Worker(base, home, WorkerRequest{Action: "deploy", Name: p.Name, Digest: p2.Digest, Package: &p2}); e == nil {
				t.Fatal("overwrote unknown skill")
			}
			claude := filepath.Join(providerRoot(home, "claude"), "skills", p.Name)
			if scenario == "external" {
				if !pathMissing(claude) {
					t.Fatal("partial publication on conflict")
				}
			} else if e := matches(claude, p); e != nil {
				t.Fatal("other provider changed", e)
			}
		})
	}
}
func TestInterruptedPublicationReconcilesWithoutRepeatingDeploy(t *testing.T) {
	for _, phase := range []string{"prepared", "gap", "published", "removed"} {
		t.Run(phase, func(t *testing.T) {
			home := t.TempDir()
			base := filepath.Join(home, "managed")
			old := fixture(t, "before")
			next := fixture(t, "after")
			if _, e := Worker(base, home, WorkerRequest{Action: "deploy", Name: old.Name, Digest: old.Digest, Package: &old}); e != nil {
				t.Fatal(e)
			}
			_ = atomicJSON(filepath.Join(base, "packages", next.Digest+".json"), next)
			r, _ := loadReceipt(base, old.Name)
			operation := strings.Repeat("a", 32)
			pending := &pending{Operation: operation, Before: old.Digest, After: next.Digest}
			root := providerRoot(home, "claude")
			target := filepath.Join(root, "skills", old.Name)
			stage := filepath.Join(root, ".relay-skills-stage", operation)
			backup := stage + "-previous"
			if phase == "removed" {
				pending.After = ""
			} else if e := writeTree(stage, next); e != nil {
				t.Fatal(e)
			}
			if phase != "prepared" {
				if e := renameNew(target, backup); e != nil {
					t.Fatal(e)
				}
			}
			if phase == "published" {
				if e := renameNew(stage, target); e != nil {
					t.Fatal(e)
				}
			}
			r.Providers["claude"] = registration{Digest: old.Digest, Pending: pending}
			if e := saveReceipt(base, r); e != nil {
				t.Fatal(e)
			}
			unknown, e := Worker(base, home, WorkerRequest{Action: "status", Name: old.Name})
			if e != nil || unknown.Deployments[0].Providers[0].Status != "uncertain" {
				t.Fatal(unknown, e)
			}
			if _, e = Worker(base, home, WorkerRequest{Action: "deploy", Name: old.Name, Digest: next.Digest, Package: &next}); e == nil {
				t.Fatal("replayed unknown deployment")
			}
			result, e := Worker(base, home, WorkerRequest{Action: "reconcile", Name: old.Name})
			if e != nil {
				t.Fatal(e)
			}
			got := result.Deployments[0].Providers[0]
			expected := old.Digest
			if phase == "published" {
				expected = next.Digest
			}
			if phase == "removed" {
				expected = ""
			}
			if got.Digest != expected {
				t.Fatal(got)
			}
			saved, e := loadReceipt(base, old.Name)
			if e != nil || saved.Providers["claude"].Pending != nil {
				t.Fatal(saved, e)
			}
		})
	}
}
func TestReceiptRejectsUnknownFieldsAndRenameNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "managed")
	_ = ensureDir(filepath.Join(base, "deployments"))
	p := fixture(t, "")
	raw, _ := json.Marshal(map[string]any{"format": 1, "name": p.Name, "providers": map[string]any{}, "unknown": true})
	_ = os.WriteFile(receiptPath(base, p.Name), raw, 0600)
	if _, e := Worker(base, home, WorkerRequest{Action: "status", Name: p.Name}); e == nil {
		t.Fatal("accepted unknown receipt fields")
	}
	from, to := filepath.Join(home, "from"), filepath.Join(home, "to")
	_ = os.Mkdir(from, 0700)
	_ = os.Mkdir(to, 0700)
	if e := renameNew(from, to); e == nil {
		t.Fatal("replaced existing unknown directory")
	}
}
