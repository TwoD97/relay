//go:build linux

package transport

import (
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
