package server

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type IgnorePattern struct {
	raw       string
	regex     *regexp.Regexp
	isDirOnly bool
	negate    bool
}

type GitIgnore struct {
	dirPath  string
	patterns []IgnorePattern
}

type GitIgnoreMatcher struct {
	ignores []GitIgnore
	baseDir string
}

func NewGitIgnoreMatcher(baseDir string) *GitIgnoreMatcher {
	matcher := &GitIgnoreMatcher{
		baseDir: baseDir,
	}
	matcher.loadIgnores(baseDir)
	return matcher
}

func (m *GitIgnoreMatcher) loadIgnores(dir string) {
	// Walk the workspace and find all .gitignore files
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == ".gitignore" {
			m.parseGitIgnoreFile(path)
		}
		return nil
	})
}

func (m *GitIgnoreMatcher) parseGitIgnoreFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	dirPath := filepath.Dir(path)
	relDirPath, err := filepath.Rel(m.baseDir, dirPath)
	if err != nil || relDirPath == "." {
		relDirPath = ""
	}

	var patterns []IgnorePattern
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if p, ok := compilePattern(line, relDirPath); ok {
			patterns = append(patterns, p)
		}
	}

	m.ignores = append(m.ignores, GitIgnore{
		dirPath:  relDirPath,
		patterns: patterns,
	})
}

func compilePattern(pattern string, baseDir string) (IgnorePattern, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.HasPrefix(pattern, "#") {
		return IgnorePattern{}, false
	}

	negate := false
	if strings.HasPrefix(pattern, "!") {
		negate = true
		pattern = pattern[1:]
	}

	isDirOnly := false
	if strings.HasSuffix(pattern, "/") {
		isDirOnly = true
		pattern = pattern[:len(pattern)-1]
	}

	// Slashes in the pattern determine if it is anchored to the baseDir or relative
	anchored := false
	if strings.Contains(pattern, "/") {
		anchored = true
		if strings.HasPrefix(pattern, "/") {
			pattern = pattern[1:]
		}
	}

	var sb strings.Builder
	
	if anchored {
		sb.WriteString("^" + regexp.QuoteMeta(baseDir))
		if baseDir != "" {
			sb.WriteString("/")
		}
	} else {
		sb.WriteString("(^|/)")
	}

	// Translate glob to regex
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '*' {
			if i+1 < len(runes) && runes[i+1] == '*' {
				sb.WriteString(".*")
				i++
				if i+1 < len(runes) && runes[i+1] == '/' {
					i++
				}
			} else {
				sb.WriteString("[^/]*")
			}
		} else if r == '?' {
			sb.WriteString("[^/]")
		} else if r == '[' || r == ']' || r == '^' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}

	sb.WriteString("(/|$)")

	re, err := regexp.Compile(sb.String())
	if err != nil {
		return IgnorePattern{}, false
	}

	return IgnorePattern{
		raw:       pattern,
		regex:     re,
		isDirOnly: isDirOnly,
		negate:    negate,
	}, true
}

func (m *GitIgnoreMatcher) IsIgnored(path string, isDir bool) bool {
	// First check the path itself
	if m.isIgnoredDirect(path, isDir) {
		return true
	}

	// If the path itself is not directly ignored, check if any of its parent directories are ignored!
	dir := filepath.Dir(path)
	for dir != "." && dir != "/" && dir != "" {
		// Break if we went above baseDir
		rel, err := filepath.Rel(m.baseDir, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			break
		}

		if m.isIgnoredDirect(dir, true) {
			return true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return false
}

func (m *GitIgnoreMatcher) isIgnoredDirect(path string, isDir bool) bool {
	relPath, err := filepath.Rel(m.baseDir, path)
	if err != nil {
		return false
	}
	relPath = filepath.ToSlash(relPath)

	// Always ignore .git directory itself
	parts := strings.Split(relPath, "/")
	for _, p := range parts {
		if p == ".git" {
			return true
		}
	}

	ignored := false

	// Match against all loaded ignores
	for _, gi := range m.ignores {
		// Only apply .gitignore patterns if the file is inside that gitignore's directory tree
		if gi.dirPath != "" && !strings.HasPrefix(relPath, gi.dirPath+"/") && relPath != gi.dirPath {
			continue
		}

		for _, p := range gi.patterns {
			if p.isDirOnly && !isDir {
				continue
			}

			if p.regex.MatchString(relPath) {
				if p.negate {
					ignored = false
				} else {
					ignored = true
				}
			}
		}
	}

	return ignored
}
