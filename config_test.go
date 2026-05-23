package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCLIFlags(t *testing.T) {
	args := []string{"ingest", "--collection", "test-collection", "-w", "/some/path", "--ollama=http://ollama:11434"}
	flags := parseCLIFlags(args)

	if flags["QDRANT_COLLECTION"] != "test-collection" {
		t.Errorf("Expected test-collection, got %s", flags["QDRANT_COLLECTION"])
	}
	if flags["WATCH_DIRECTORY"] != "/some/path" {
		t.Errorf("Expected /some/path, got %s", flags["WATCH_DIRECTORY"])
	}
	if flags["OLLAMA_HOST"] != "http://ollama:11434" {
		t.Errorf("Expected http://ollama:11434, got %s", flags["OLLAMA_HOST"])
	}
}

func TestLoadJsonConfig(t *testing.T) {
	// Create a temp .mcp.json file
	tmpDir, err := os.MkdirTemp("", "mcp-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	jsonPath := filepath.Join(tmpDir, ".mcp.json")
	content := `{
		"mcpServers": {
			"qdrant-memory-rag": {
				"command": "qdrant-mcp-server",
				"env": {
					"QDRANT_COLLECTION": "json-collection",
					"WATCH_DIRECTORY": "/json/path",
					"MAX_EMBEDDING_WORKERS": 12,
					"QDRANT_PORT": 6334
				}
			}
		}
	}`

	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := loadJsonConfig(jsonPath)
	if vars["QDRANT_COLLECTION"] != "json-collection" {
		t.Errorf("Expected json-collection, got %s", vars["QDRANT_COLLECTION"])
	}
	if vars["WATCH_DIRECTORY"] != "/json/path" {
		t.Errorf("Expected /json/path, got %s", vars["WATCH_DIRECTORY"])
	}
	if vars["MAX_EMBEDDING_WORKERS"] != "12" {
		t.Errorf("Expected 12, got %s", vars["MAX_EMBEDDING_WORKERS"])
	}
	if vars["QDRANT_PORT"] != "6334" {
		t.Errorf("Expected 6334, got %s", vars["QDRANT_PORT"])
	}
}

func TestLoadTomlConfig(t *testing.T) {
	// Create a temp config.toml
	tmpDir, err := os.MkdirTemp("", "toml-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tomlPath := filepath.Join(tmpDir, "config.toml")
	content := `
[mcp_servers.qdrant-memory-rag.env]
QDRANT_COLLECTION = "toml-collection"
WATCH_DIRECTORY = "/toml/path"
OLLAMA_HOST = "http://toml-ollama:11434"
`

	if err := os.WriteFile(tomlPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := loadTomlConfig(tomlPath)
	if vars["QDRANT_COLLECTION"] != "toml-collection" {
		t.Errorf("Expected toml-collection, got %s", vars["QDRANT_COLLECTION"])
	}
	if vars["WATCH_DIRECTORY"] != "/toml/path" {
		t.Errorf("Expected /toml/path, got %s", vars["WATCH_DIRECTORY"])
	}
	if vars["OLLAMA_HOST"] != "http://toml-ollama:11434" {
		t.Errorf("Expected http://toml-ollama:11434, got %s", vars["OLLAMA_HOST"])
	}
}
