package tests

import (
	"os"
	"path/filepath"
	"testing"

	"qdrant-mcp-server/server"
)

func writeMCPJSON(t *testing.T, path, collection string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"mcpServers": {
			"qdrant-mcp-server": {
				"command": "qdrant-mcp-server",
				"args": [],
				"env": {
					"QDRANT_HOST": "127.0.0.1",
					"QDRANT_PORT": "6334",
					"QDRANT_COLLECTION": "` + collection + `",
					"WATCH_DIRECTORY": "/tmp/watch",
					"OLLAMA_HOST": "http://127.0.0.1:11434",
					"EMBEDDING_MODEL": "nomic-embed-text",
					"PARSER_MODE": "full",
					"SEARCH_MODE": "hybrid"
				}
			}
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// ROO1 / ROO2: .roo/mcp.json is recognized and env values extracted.
func TestLoadJsonConfig_RooMcpJson(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "roo-mcp-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	rooPath := filepath.Join(tmpDir, ".roo", "mcp.json")
	writeMCPJSON(t, rooPath, "roo-collection")

	vars := server.LoadJsonConfig(rooPath)
	if vars["QDRANT_COLLECTION"] != "roo-collection" {
		t.Errorf("Expected roo-collection, got %s", vars["QDRANT_COLLECTION"])
	}
	if vars["QDRANT_HOST"] != "127.0.0.1" {
		t.Errorf("Expected 127.0.0.1, got %s", vars["QDRANT_HOST"])
	}
	if vars["OLLAMA_HOST"] != "http://127.0.0.1:11434" {
		t.Errorf("Unexpected OLLAMA_HOST: %s", vars["OLLAMA_HOST"])
	}
	if vars["EMBEDDING_MODEL"] != "nomic-embed-text" {
		t.Errorf("Unexpected EMBEDDING_MODEL: %s", vars["EMBEDDING_MODEL"])
	}
	if vars["PARSER_MODE"] != "full" {
		t.Errorf("Unexpected PARSER_MODE: %s", vars["PARSER_MODE"])
	}
	if vars["SEARCH_MODE"] != "hybrid" {
		t.Errorf("Unexpected SEARCH_MODE: %s", vars["SEARCH_MODE"])
	}
}

// ZOO1 / ZOO2: Zoo uses the same verified .roo/mcp.json path.
func TestDiscoverProjectMCPEnv_RooPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zoo-mcp-discover")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	writeMCPJSON(t, filepath.Join(tmpDir, ".roo", "mcp.json"), "zoo-shared-collection")

	vars := server.DiscoverProjectMCPEnv(tmpDir)
	if vars["QDRANT_COLLECTION"] != "zoo-shared-collection" {
		t.Errorf("Expected zoo-shared-collection, got %s", vars["QDRANT_COLLECTION"])
	}
	if vars["WATCH_DIRECTORY"] != "/tmp/watch" {
		t.Errorf("Expected /tmp/watch, got %s", vars["WATCH_DIRECTORY"])
	}
}

// MULTI1: when multiple client configs exist, ordered precedence picks .mcp.json over Roo/Claude.
func TestDiscoverProjectMCPEnv_MultiClientPrecedence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "multi-mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	writeMCPJSON(t, filepath.Join(tmpDir, ".mcp.json"), "from-mcp-json")
	writeMCPJSON(t, filepath.Join(tmpDir, ".claude", "settings.local.json"), "from-claude")
	writeMCPJSON(t, filepath.Join(tmpDir, ".roo", "mcp.json"), "from-roo")

	vars := server.DiscoverProjectMCPEnv(tmpDir)
	if vars["QDRANT_COLLECTION"] != "from-mcp-json" {
		t.Errorf("Expected .mcp.json to win, got collection %q", vars["QDRANT_COLLECTION"])
	}
}

func TestDiscoverProjectMCPEnv_RooWinsWhenOnlyRoo(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "only-roo")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	writeMCPJSON(t, filepath.Join(tmpDir, ".roo", "mcp.json"), "only-roo")
	writeMCPJSON(t, filepath.Join(tmpDir, ".claude", "settings.local.json"), "from-claude")

	// Claude is checked before .roo/mcp.json, so Claude wins when both exist without .mcp.json
	vars := server.DiscoverProjectMCPEnv(tmpDir)
	if vars["QDRANT_COLLECTION"] != "from-claude" {
		t.Errorf("Expected Claude before Roo, got %q", vars["QDRANT_COLLECTION"])
	}
}

// MULTI2: explicit CLI config overrides discovered client configuration.
func TestFilterDiscoveredEnv_CLIOverrides(t *testing.T) {
	discovered := map[string]string{
		"QDRANT_COLLECTION": "from-roo",
		"OLLAMA_HOST":       "http://from-roo:11434",
		"PARSER_MODE":       "full",
	}
	cliEnv := map[string]string{
		"QDRANT_COLLECTION": "from-cli",
	}
	shellEnv := map[string]bool{
		"OLLAMA_HOST": true,
	}

	applied := server.FilterDiscoveredEnv(discovered, cliEnv, shellEnv)
	if _, ok := applied["QDRANT_COLLECTION"]; ok {
		t.Error("CLI-set QDRANT_COLLECTION must not be overridden by discovery")
	}
	if _, ok := applied["OLLAMA_HOST"]; ok {
		t.Error("Shell-set OLLAMA_HOST must not be overridden by discovery")
	}
	if applied["PARSER_MODE"] != "full" {
		t.Errorf("Expected PARSER_MODE from discovery, got %q", applied["PARSER_MODE"])
	}
}
