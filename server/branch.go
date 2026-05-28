package server

import (
	"os/exec"
	"strings"
)

// DetectBranches returns (currentBranch, defaultBranch) for the git repo at dir.
// Falls back to "main" for both if git is unavailable or dir is not a repo.
func DetectBranches(dir string) (string, string) {
	current := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if current == "" || current == "HEAD" {
		current = "main"
	}
	return current, detectDefaultBranch(dir)
}

func detectDefaultBranch(dir string) string {
	ref := gitOutput(dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if ref != "" {
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	if gitOutput(dir, "rev-parse", "--verify", "main") != "" {
		return "main"
	}
	if gitOutput(dir, "rev-parse", "--verify", "master") != "" {
		return "master"
	}
	return "main"
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
