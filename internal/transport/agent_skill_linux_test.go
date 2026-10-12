//go:build linux

package transport

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	agentskill "github.com/TwoD97/relay/skills"
)

func TestAgentSkillRegistersBothHarnessesAndPreservesUserSkill(t *testing.T) {
	home := t.TempDir()
	run := func(content string) error {
		cmd := exec.Command("/bin/sh", "-c", agentSkillInstallScript)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Log(string(out))
		}
		return err
	}
	if err := run(agentskill.Content); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{".agents", ".claude"} {
		location := filepath.Join(home, parent, "skills", "relay")
		if _, err := os.Readlink(location); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(location, "SKILL.md"))
		if err != nil || string(b) != agentskill.Content {
			t.Fatal("skill registration did not resolve to bundled instructions", err)
		}
	}
	if err := run(agentskill.Content); err != nil {
		t.Fatal("idempotent registration failed", err)
	}
	claude := filepath.Join(home, ".claude", "skills", "relay")
	if err := os.Remove(claude); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claude, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claude, "SKILL.md"), []byte("User-owned skill"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("Updated managed skill"); err == nil {
		t.Fatal("missing conflict warning")
	}
	b, _ := os.ReadFile(filepath.Join(claude, "SKILL.md"))
	if string(b) != "User-owned skill" {
		t.Fatal("overwrote user skill")
	}
	b, _ = os.ReadFile(filepath.Join(home, ".agents", "skills", "relay", "SKILL.md"))
	if string(b) != "Updated managed skill" {
		t.Fatal("non-conflicting registration failed to update")
	}
	canonical := filepath.Join(home, ".local/share/relay/skills/relay/SKILL.md")
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(home, "unrelated")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "keep"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unrelated, canonical); err != nil {
		t.Fatal(err)
	}
	if err := run("must not be moved into a directory"); err == nil {
		t.Fatal("accepted linked managed skill destination")
	}
	if entries, err := os.ReadDir(unrelated); err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatal("wrote through managed skill symlink", entries, err)
	}
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(canonical, 0700); err != nil {
		t.Fatal(err)
	}
	if err := run("must not be moved into a directory"); err == nil {
		t.Fatal("accepted managed skill directory destination")
	}
	if entries, err := os.ReadDir(canonical); err != nil || len(entries) != 0 {
		t.Fatal("moved skill into existing directory", entries, err)
	}
}

// The production allowlist contains the digest of an earlier shipped bundle.
// Substitute exactly that entry with this harmless fixture's digest so the
// regression exercises the migration branch without publishing old branding.
func previousSkillFixture(t *testing.T) (script, content string) {
	t.Helper()
	const shippedDigest = "bc488ebc360d3ff64d8d5c6b04a1b45df07c4786aaa9dd2a331a3063f34528eb"
	if strings.Count(agentSkillInstallScript, shippedDigest) != 1 {
		t.Fatal("expected exactly one previous-bundle allowlist entry")
	}
	content = "---\nname: relay\n---\n# Relay previous shipped fixture\nThese exact bytes represent a known bundled skill.\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	return strings.Replace(agentSkillInstallScript, shippedDigest, digest, 1), content
}

func runSkillFixture(t *testing.T, home, script, content string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeSkillFixture(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func skillRegistration(t *testing.T, home, harness, destination string) string {
	t.Helper()
	parent := filepath.Join(home, harness, "skills")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "relay")
	if err := os.Symlink(destination, target); err != nil {
		t.Fatal(err)
	}
	return target
}

func skillBackups(t *testing.T, home string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, ".local", "share", "relay", "skills", ".registration.*", "previous"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestAgentSkillMigratesOnlyKnownPreviousRegistrations(t *testing.T) {
	script, previousContent := previousSkillFixture(t)
	home := t.TempDir()
	previous := filepath.Join(home, ".local", "share", "previous release", "skills", "relay")
	writeSkillFixture(t, previous, previousContent)
	originals := map[string]int{}
	for _, harness := range []string{".agents", ".claude"} {
		destination := previous
		if harness == ".claude" {
			var err error
			destination, err = filepath.Rel(filepath.Join(home, harness, "skills"), previous)
			if err != nil {
				t.Fatal(err)
			}
		}
		skillRegistration(t, home, harness, destination)
		originals[destination]++
	}
	if out, err := runSkillFixture(t, home, script, agentskill.Content); err != nil {
		t.Fatalf("migration failed: %v: %s", err, out)
	}
	current := filepath.Join(home, ".local", "share", "relay", "skills", "relay")
	for _, harness := range []string{".agents", ".claude"} {
		target := filepath.Join(home, harness, "skills", "relay")
		if value, err := os.Readlink(target); err != nil || value != current {
			t.Fatalf("registration not migrated: %q, %v", value, err)
		}
		if data, err := os.ReadFile(filepath.Join(target, "SKILL.md")); err != nil || string(data) != agentskill.Content {
			t.Fatal("registration did not resolve to new bundled instructions", err)
		}
		entries, err := os.ReadDir(filepath.Dir(target))
		if err != nil || len(entries) != 1 || entries[0].Name() != "relay" {
			t.Fatal("temporary registration staging was not cleaned", entries, err)
		}
	}
	backups := skillBackups(t, home)
	if len(backups) != 2 {
		t.Fatalf("expected both original links backed up, got %d", len(backups))
	}
	for _, backup := range backups {
		value, err := os.Readlink(backup)
		if err != nil || originals[value] == 0 {
			t.Fatalf("backup lost original readlink value: %q, %v", value, err)
		}
		originals[value]--
		info, err := os.Stat(filepath.Dir(backup))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatal("link backup must be private", err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(previous, "SKILL.md")); err != nil || string(data) != previousContent {
		t.Fatal("migration modified the previous installation", err)
	}
	entries, err := os.ReadDir(previous)
	if err != nil || len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatal("migration changed previous installation contents", entries, err)
	}
	if out, err := runSkillFixture(t, home, script, agentskill.Content); err != nil {
		t.Fatalf("repeat migration failed: %v: %s", err, out)
	}
	if len(skillBackups(t, home)) != 2 {
		t.Fatal("idempotent registration created additional backups")
	}
}

func TestAgentSkillPreservesEquivalentCurrentRegistration(t *testing.T) {
	for _, form := range []string{"relative", "absolute normalized"} {
		t.Run(form, func(t *testing.T) {
			home := t.TempDir()
			current := filepath.Join(home, ".local", "share", "relay", "skills", "relay")
			writeSkillFixture(t, current, "Old managed content")
			destination := filepath.Dir(current) + "/./relay"
			if form == "relative" {
				var err error
				destination, err = filepath.Rel(filepath.Join(home, ".agents", "skills"), current)
				if err != nil {
					t.Fatal(err)
				}
			}
			target := skillRegistration(t, home, ".agents", destination)
			if out, err := runSkillFixture(t, home, agentSkillInstallScript, agentskill.Content); err != nil {
				t.Fatalf("equivalent registration rejected: %v: %s", err, out)
			}
			if value, err := os.Readlink(target); err != nil || value != destination {
				t.Fatal("equivalent current symlink needlessly replaced", value, err)
			}
			if len(skillBackups(t, home)) != 0 {
				t.Fatal("equivalent registration created a migration backup")
			}
			if data, err := os.ReadFile(filepath.Join(target, "SKILL.md")); err != nil || string(data) != agentskill.Content {
				t.Fatal("equivalent registration missed managed update", err)
			}
		})
	}
}

func TestAgentSkillCanAdoptUnchangedCurrentBundleFromPreviousNamespace(t *testing.T) {
	home := t.TempDir()
	previous := filepath.Join(home, ".local", "share", "earlier-relay", "skills", "relay")
	writeSkillFixture(t, previous, agentskill.Content)
	target := skillRegistration(t, home, ".agents", previous)
	if out, err := runSkillFixture(t, home, agentSkillInstallScript, agentskill.Content); err != nil {
		t.Fatalf("current identical bundle rejected: %v: %s", err, out)
	}
	if value, err := os.Readlink(target); err != nil || value == previous {
		t.Fatal("current identical bundle was not migrated", value, err)
	}
	if data, err := os.ReadFile(filepath.Join(previous, "SKILL.md")); err != nil || string(data) != agentskill.Content {
		t.Fatal("previous copy changed", err)
	}
	if len(skillBackups(t, home)) != 1 {
		t.Fatal("missing link backup")
	}
}

func TestAgentSkillPreservesAmbiguousAndCustomizedRegistrations(t *testing.T) {
	for _, scenario := range []string{"modified", "extra file", "hidden file", "custom directory", "custom file", "broken", "foreign", "nested namespace", "traversal outside", "namespace link outside", "linked skill file"} {
		t.Run(scenario, func(t *testing.T) {
			script, content := previousSkillFixture(t)
			home := t.TempDir()
			previous := filepath.Join(home, ".local", "share", "previous-relay", "skills", "relay")
			parent := filepath.Join(home, ".agents", "skills")
			target := filepath.Join(parent, "relay")
			if err := os.MkdirAll(parent, 0700); err != nil {
				t.Fatal(err)
			}
			destination := previous
			switch scenario {
			case "modified":
				content += "User customization.\n"
			case "foreign", "traversal outside", "namespace link outside":
				previous = filepath.Join(home, "projects", "skills", "relay")
				destination = previous
			case "nested namespace":
				previous = filepath.Join(home, ".local", "share", "vendor", "nested", "skills", "relay")
				destination = previous
			}
			writeSkillFixture(t, previous, content)
			switch scenario {
			case "extra file", "hidden file":
				name := "custom.txt"
				if scenario == "hidden file" {
					name = ".custom"
				}
				if err := os.WriteFile(filepath.Join(previous, name), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "custom directory":
				writeSkillFixture(t, target, content)
			case "custom file":
				if err := os.WriteFile(target, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			case "broken":
				destination += "-missing"
			case "traversal outside":
				if err := os.MkdirAll(filepath.Join(home, ".local", "share"), 0700); err != nil {
					t.Fatal(err)
				}
				destination = home + "/.local/share/../../projects/skills/relay"
			case "namespace link outside":
				if err := os.MkdirAll(filepath.Join(home, ".local", "share"), 0700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(home, ".local", "share", "linked-namespace")
				if err := os.Symlink(filepath.Join(home, "projects"), alias); err != nil {
					t.Fatal(err)
				}
				destination = filepath.Join(alias, "skills", "relay")
			case "linked skill file":
				if err := os.Rename(filepath.Join(previous, "SKILL.md"), filepath.Join(home, "outside.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(home, "outside.md"), filepath.Join(previous, "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "custom directory" && scenario != "custom file" {
				if err := os.Symlink(destination, target); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			out, err := runSkillFixture(t, home, script, agentskill.Content)
			if err == nil || strings.Count(out, "Existing skill preserved: "+target) != 1 {
				t.Fatalf("missing exact conflict warning: %v: %s", err, out)
			}
			after, err := os.Lstat(target)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("ambiguous registration was replaced", err)
			}
			if before.Mode()&os.ModeSymlink != 0 {
				if value, err := os.Readlink(target); err != nil || value != destination {
					t.Fatal("ambiguous link changed", value, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(previous, "SKILL.md")); err != nil || string(data) != content {
				t.Fatal("ambiguous source content changed", err)
			}
			if len(skillBackups(t, home)) != 0 {
				t.Fatal("ambiguous link was classified for migration")
			}
			if data, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "relay", "SKILL.md")); err != nil || string(data) != agentskill.Content {
				t.Fatal("one conflict prevented independent harness registration", err)
			}
		})
	}
}
