package tests

import (
	"os"
	"path/filepath"
	"testing"

	"qdrant-mcp-server/server"
)

func TestGitIgnoreMatcher(t *testing.T) {
	// Create a temp workspace directory
	tmpDir, err := os.MkdirTemp("", "gitignore-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a root .gitignore
	rootIgnore := `
# Comments and empty lines
*.log
node_modules/
/dist
!important.log
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(rootIgnore), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a nested directory with its own .gitignore
	nestedDir := filepath.Join(tmpDir, "src", "nested")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}

	nestedIgnore := `
nested_ignored.txt
`
	if err := os.WriteFile(filepath.Join(nestedDir, ".gitignore"), []byte(nestedIgnore), 0644); err != nil {
		t.Fatal(err)
	}

	// Instantiate the matcher
	matcher := server.NewGitIgnoreMatcher(tmpDir)

	tests := []struct {
		path    string
		isDir   bool
		ignored bool
	}{
		// Root-level matches
		{filepath.Join(tmpDir, "app.log"), false, true},
		{filepath.Join(tmpDir, "src", "app.log"), false, true}, // unanchored glob
		{filepath.Join(tmpDir, "important.log"), false, false}, // negated rule
		{filepath.Join(tmpDir, "node_modules"), true, true},
		{filepath.Join(tmpDir, "node_modules", "module"), false, true}, // inside directory
		{filepath.Join(tmpDir, "dist"), true, true},                   // anchored directory match
		{filepath.Join(tmpDir, "src", "dist"), true, false},           // not at root, /dist is anchored

		// Nested matches
		{filepath.Join(nestedDir, "nested_ignored.txt"), false, true},
		{filepath.Join(tmpDir, "nested_ignored.txt"), false, false}, // nested rule only applies to subdirectories

		// Git folder check
		{filepath.Join(tmpDir, ".git"), true, true},
		{filepath.Join(tmpDir, ".git", "config"), false, true},
	}

	for _, tt := range tests {
		res := matcher.IsIgnored(tt.path, tt.isDir)
		if res != tt.ignored {
			t.Errorf("Path %s (isDir=%t): expected ignored=%t, got %t", tt.path, tt.isDir, tt.ignored, res)
		}
	}
}
