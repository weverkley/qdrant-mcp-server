package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"qdrant-mcp-server/server"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"checkout", "-b", "main"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestDetectBranches_CurrentBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	current, _ := server.DetectBranches(dir)
	if current != "main" {
		t.Fatalf("expected current branch 'main', got %q", current)
	}
}

func TestDetectBranches_FeatureBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "init").Run()
	exec.Command("git", "-C", dir, "checkout", "-b", "feature/test").Run()

	current, _ := server.DetectBranches(dir)
	if current != "feature/test" {
		t.Fatalf("expected 'feature/test', got %q", current)
	}
}

func TestDetectBranches_FallbackNonGit(t *testing.T) {
	dir := t.TempDir()
	current, def := server.DetectBranches(dir)
	if current != "main" {
		t.Fatalf("expected fallback 'main', got %q", current)
	}
	if def != "main" {
		t.Fatalf("expected fallback default 'main', got %q", def)
	}
}

func TestDetectBranches_DefaultFromLocal(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	_, def := server.DetectBranches(dir)
	if def != "main" {
		t.Fatalf("expected default branch 'main', got %q", def)
	}
}
