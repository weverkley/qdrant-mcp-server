package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all the server configurations.
type Config struct {
	QdrantHost        string
	QdrantPort        int
	CollectionName    string
	WatchDirectory    string
	OllamaHost        string
	EmbeddingModel    string
	DebounceDuration  time.Duration
	ExcludeDirs       []string
	IncludeHiddenDirs []string
	ParserMode        string // "code", "doc", or "full" (default)
}

func loadConfig() Config {
	host := os.Getenv("QDRANT_HOST")
	if host == "" {
		host = "172.20.0.5"
	}

	port := 6334
	if portStr := os.Getenv("QDRANT_PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			port = p
		} else {
			log.Printf("Warning: QDRANT_PORT '%s' is not a valid integer, falling back to default 6334", portStr)
		}
	}

	// Helper function to turn comma-separated string arrays into Go slices cleanly
	parseEnvArray := func(key string) []string {
		val := os.Getenv(key)
		if val == "" {
			return []string{}
		}
		items := strings.Split(val, ",")
		for i, item := range items {
			items[i] = strings.TrimSpace(item)
		}
		return items
	}

	parserMode := strings.ToLower(strings.TrimSpace(os.Getenv("PARSER_MODE")))
	if parserMode == "" {
		parserMode = "full"
	} else if parserMode != "code" && parserMode != "doc" && parserMode != "full" {
		log.Printf("Warning: PARSER_MODE '%s' is not valid, falling back to default 'full'", parserMode)
		parserMode = "full"
	}

	return Config{
		QdrantHost:        host,
		QdrantPort:        port,
		CollectionName:    os.Getenv("QDRANT_COLLECTION"),
		WatchDirectory:    os.Getenv("WATCH_DIRECTORY"),
		OllamaHost:        os.Getenv("OLLAMA_HOST"),
		EmbeddingModel:    os.Getenv("EMBEDDING_MODEL"),
		DebounceDuration:  800 * time.Millisecond,
		ExcludeDirs:       parseEnvArray("EXCLUDE_DIRS"),
		IncludeHiddenDirs: parseEnvArray("INCLUDE_HIDDEN_DIRS"),
		ParserMode:        parserMode,
	}
}

// Helper to look up specific slice elements quickly
func sliceContains(slice []string, match string) bool {
	for _, item := range slice {
		if item == match {
			return true
		}
	}
	return false
}
