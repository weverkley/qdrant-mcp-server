package server

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills/*.md
var skillsFS embed.FS

type Skill struct {
	Key         string
	Filename    string
	Description string
	EmbedPath   string
}

var AvailableSkills = []Skill{
	{
		Key:         "cursor",
		Filename:    ".cursorrules",
		Description: "Cursor rules file (.cursorrules)",
		EmbedPath:   "skills/cursor.md",
	},
	{
		Key:         "windsurf",
		Filename:    ".windsurfrules",
		Description: "Windsurf rules file (.windsurfrules)",
		EmbedPath:   "skills/windsurf.md",
	},
	{
		Key:         "cline",
		Filename:    ".clinerules",
		Description: "Cline / Roo Cline rules file (.clinerules)",
		EmbedPath:   "skills/cline.md",
	},
	{
		Key:         "copilot",
		Filename:    ".github/copilot-instructions.md",
		Description: "GitHub Copilot instructions (.github/copilot-instructions.md)",
		EmbedPath:   "skills/copilot.md",
	},
	{
		Key:         "generic",
		Filename:    "qdrant-rag-instructions.md",
		Description: "Generic markdown instructions (qdrant-rag-instructions.md)",
		EmbedPath:   "skills/generic.md",
	},
	{
		Key:         "codex",
		Filename:    ".codex/mcp-instructions.md",
		Description: "Codex instructions (.codex/mcp-instructions.md)",
		EmbedPath:   "skills/codex.md",
	},
}

// ListSkills outputs the supported agents and files they generate in a styled, premium layout
func ListSkills() {
	fmt.Println("\n==================================================================")
	fmt.Println("🚀 Available Agent Skills for Qdrant MCP Server")
	fmt.Println("==================================================================")
	fmt.Println("Install these rule files in your project directory so your favorite")
	fmt.Println("AI coding agents know when and how to leverage semantic search.")
	fmt.Println()

	for _, skill := range AvailableSkills {
		fmt.Printf("• \x1b[1;36m%-10s\x1b[0m -> %-32s (%s)\n", skill.Key, skill.Filename, skill.Description)
	}

	fmt.Println()
	fmt.Println("\x1b[1;33mUsage examples:\x1b[0m")
	fmt.Println("  qdrant-mcp-server install-skill cursor")
	fmt.Println("  qdrant-mcp-server install-skill copilot /absolute/path/to/project")
	fmt.Println("  qdrant-mcp-server install-skill all")
	fmt.Println("==================================================================")
}

// InstallSkill extracts the embedded skill rules and writes them to the target directory
func InstallSkill(agent string, destDir string) error {
	if destDir == "" {
		destDir = "."
	}

	agentLower := strings.ToLower(agent)

	if agentLower == "all" {
		fmt.Printf("\n📦 Installing \x1b[1;32mALL\x1b[0m skill files to directory: '%s'\n", destDir)
		for _, s := range AvailableSkills {
			if err := installOneSkill(s, destDir); err != nil {
				return err
			}
		}
		fmt.Println("\n✨ All skills successfully installed!")
		return nil
	}

	var matchSkill *Skill
	for i := range AvailableSkills {
		if AvailableSkills[i].Key == agentLower {
			matchSkill = &AvailableSkills[i]
			break
		}
	}

	if matchSkill == nil {
		return fmt.Errorf("unknown agent skill '%s'. Run 'list-skills' to see supported agents", agent)
	}

	return installOneSkill(*matchSkill, destDir)
}

func installOneSkill(skill Skill, destDir string) error {
	data, err := skillsFS.ReadFile(skill.EmbedPath)
	if err != nil {
		return fmt.Errorf("failed to read embedded resource %s: %w", skill.EmbedPath, err)
	}

	targetPath := filepath.Join(destDir, skill.Filename)

	// Ensure parent directory exists (e.g. .github/)
	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory tree %s: %w", parentDir, err)
	}

	// Write skill file
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write skill file %s: %w", targetPath, err)
	}

	fmt.Printf("✅ Installed \x1b[1;32m%s\x1b[0m -> \x1b[1;34m%s\x1b[0m\n", skill.Key, targetPath)
	return nil
}
