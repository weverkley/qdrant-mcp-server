package tests

import (
	"os"
	"path/filepath"
	"testing"

	"qdrant-mcp-server/server"
)

func TestParseCLIFlags(t *testing.T) {
	args := []string{"ingest", "--collection", "test-collection", "-w", "/some/path", "--ollama=http://ollama:11434"}
	flags := server.ParseCLIFlags(args)

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

	vars := server.LoadJsonConfig(jsonPath)
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

	vars := server.LoadTomlConfig(tomlPath)
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

func TestConfig_LogToFileParsing(t *testing.T) {
	os.Setenv("LOG_TO_FILE", "true")
	defer os.Unsetenv("LOG_TO_FILE")

	cfg := server.LoadConfig()
	if !cfg.LogToFile {
		t.Errorf("Expected LogToFile to be true when LOG_TO_FILE env is true")
	}

	args := []string{"ingest", "--log-to-file", "false"}
	flags := server.ParseCLIFlags(args)
	if flags["LOG_TO_FILE"] != "false" {
		t.Errorf("Expected flags[LOG_TO_FILE] to be false, got %s", flags["LOG_TO_FILE"])
	}

	args2 := []string{"ingest", "-lf", "true"}
	flags2 := server.ParseCLIFlags(args2)
	if flags2["LOG_TO_FILE"] != "true" {
		t.Errorf("Expected flags2[LOG_TO_FILE] to be true, got %s", flags2["LOG_TO_FILE"])
	}
}

func TestConfig_SearchModeParsing(t *testing.T) {
	os.Setenv("SEARCH_MODE", "hybrid")
	defer os.Unsetenv("SEARCH_MODE")

	cfg := server.LoadConfig()
	if cfg.SearchMode != "hybrid" {
		t.Errorf("Expected SearchMode to be hybrid, got %s", cfg.SearchMode)
	}

	args := []string{"ingest", "--search-mode", "sparse"}
	flags := server.ParseCLIFlags(args)
	if flags["SEARCH_MODE"] != "sparse" {
		t.Errorf("Expected flags[SEARCH_MODE] to be sparse, got %s", flags["SEARCH_MODE"])
	}

	args2 := []string{"ingest", "-sm", "dense"}
	flags2 := server.ParseCLIFlags(args2)
	if flags2["SEARCH_MODE"] != "dense" {
		t.Errorf("Expected flags2[SEARCH_MODE] to be dense, got %s", flags2["SEARCH_MODE"])
	}
}
