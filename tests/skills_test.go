package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"qdrant-mcp-server/server"
)

func skillTarget(destDir string) string {
	return filepath.Join(destDir, ".roo", "rules", "qdrant-rag.md")
}

// ROO3 / ROO4: install-skill roo writes expected instructions and is idempotent.
func TestInstallSkill_Roo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "skill-roo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	if err := server.InstallSkill("roo", tmpDir); err != nil {
		t.Fatalf("install-skill roo failed: %v", err)
	}

	path := skillTarget(tmpDir)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected rules file at %s: %v", path, err)
	}
	if len(first) == 0 {
		t.Fatal("expected non-empty skill content")
	}
	lower := strings.ToLower(string(first))
	for _, needle := range []string{"qdrant_search", "filesystem", "source of truth"} {
		if !strings.Contains(lower, needle) {
			t.Errorf("skill content missing %q", needle)
		}
	}

	if err := server.InstallSkill("roo", tmpDir); err != nil {
		t.Fatalf("second install-skill roo failed: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("repeated install-skill roo must be idempotent (same content)")
	}
}

// ZOO3 / ZOO4: install-skill zoo writes the same verified .roo/rules path; idempotent.
func TestInstallSkill_Zoo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "skill-zoo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	if err := server.InstallSkill("zoo", tmpDir); err != nil {
		t.Fatalf("install-skill zoo failed: %v", err)
	}

	path := skillTarget(tmpDir)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected rules file at %s: %v", path, err)
	}

	if err := server.InstallSkill("zoo", tmpDir); err != nil {
		t.Fatalf("second install-skill zoo failed: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("repeated install-skill zoo must be idempotent")
	}
}

// ALL1: install-skill all includes Roo and Zoo (shared rules path).
func TestInstallSkill_AllIncludesRooZoo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "skill-all")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	if err := server.InstallSkill("all", tmpDir); err != nil {
		t.Fatalf("install-skill all failed: %v", err)
	}

	if _, err := os.Stat(skillTarget(tmpDir)); err != nil {
		t.Fatalf("install-skill all must install Roo/Zoo rules: %v", err)
	}
	for _, rel := range []string{".cursorrules", ".clinerules", ".codex/mcp-instructions.md"} {
		if _, err := os.Stat(filepath.Join(tmpDir, rel)); err != nil {
			t.Errorf("install-skill all missing %s: %v", rel, err)
		}
	}
}

func TestAvailableSkills_IncludesRooAndZoo(t *testing.T) {
	keys := map[string]bool{}
	for _, s := range server.AvailableSkills {
		keys[s.Key] = true
		if s.Key == "roo" || s.Key == "zoo" {
			if s.Filename != ".roo/rules/qdrant-rag.md" {
				t.Errorf("%s: expected .roo/rules/qdrant-rag.md, got %s", s.Key, s.Filename)
			}
			if s.EmbedPath != "skills/roo.md" {
				t.Errorf("%s: expected shared skills/roo.md, got %s", s.Key, s.EmbedPath)
			}
		}
	}
	if !keys["roo"] || !keys["zoo"] {
		t.Errorf("AvailableSkills missing roo/zoo: %+v", keys)
	}
}
