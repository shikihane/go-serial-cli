package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-serial-cli/internal/skill"
)

func TestInstallCopiesSkillToExplicitDirectory(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), "# Serial CLI\n")
	mustWrite(t, filepath.Join(src, "skill.json"), `{"name":"serial-cli"}`)

	result, err := skill.Install(skill.InstallOptions{Source: src, To: dst})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	want := filepath.Join(dst, "serial-cli", "SKILL.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected SKILL.md at %s: %v", want, err)
	}
	if len(result.Installed) != 1 || result.Installed[0] != filepath.Join(dst, "serial-cli") {
		t.Fatalf("unexpected install result: %#v", result.Installed)
	}
}

func TestInstallDefaultsToBundledSkill(t *testing.T) {
	dst := t.TempDir()

	result, err := skill.Install(skill.InstallOptions{To: dst})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	want := filepath.Join(dst, "serial-cli", "SKILL.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected bundled SKILL.md at %s: %v", want, err)
	}
	if len(result.Installed) != 1 || result.Installed[0] != filepath.Join(dst, "serial-cli") {
		t.Fatalf("unexpected install result: %#v", result.Installed)
	}
}

func TestBundledSkillExplainsStoppedSessionsBeforeReading(t *testing.T) {
	dst := t.TempDir()

	if _, err := skill.Install(skill.InstallOptions{To: dst}); err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	path := filepath.Join(dst, "serial-cli", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) returned error: %v", path, err)
	}
	text := string(data)
	for _, want := range []string{
		"Run `gs status <session>` before expecting new output",
		"`stopped` and `stale` sessions are not live serial readers",
		"Do not keep polling `gs read` or `gs check` expecting new device output",
		"run `gs open <session> <port> -b <baud>` first",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("bundled skill does not contain %q", want)
		}
	}
}

func TestInstallUsesSkillFrontmatterName(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), `---
name: serial-cli
description: Use gs for serial workflows.
---

# Agent Serial Workflows
`)

	result, err := skill.Install(skill.InstallOptions{Source: src, To: dst})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	want := filepath.Join(dst, "serial-cli", "SKILL.md")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected SKILL.md at %s: %v", want, err)
	}
	if len(result.Installed) != 1 || result.Installed[0] != filepath.Join(dst, "serial-cli") {
		t.Fatalf("unexpected install result: %#v", result.Installed)
	}
}

func TestInstallAutoTargetsCodexAndClaude(t *testing.T) {
	src := t.TempDir()
	home := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), "# Serial CLI\n")

	result, err := skill.Install(skill.InstallOptions{Source: src, HomeDir: home})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	expected := []string{
		filepath.Join(home, ".codex", "skills", "serial-cli", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "serial-cli", "SKILL.md"),
	}
	for _, path := range expected {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected installed file %s: %v", path, err)
		}
	}
	if len(result.Installed) != 2 {
		t.Fatalf("expected two install targets, got %#v", result.Installed)
	}
}

func TestInstallSkipsTargetInsideSource(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "SKILL.md"), "# Serial CLI\n")

	target := filepath.Join(src, ".tmp-skills")
	if _, err := skill.Install(skill.InstallOptions{Source: src, To: target}); err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	nested := filepath.Join(target, "serial-cli", ".tmp-skills", "serial-cli")
	if _, err := os.Stat(nested); err == nil {
		t.Fatalf("target was copied recursively into %s", nested)
	}
}

func mustWrite(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
