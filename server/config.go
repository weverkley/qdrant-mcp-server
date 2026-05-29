package server

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
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
	MaxEmbeddingWorkers int    // maximum concurrent embedding workers (default: 5)
	BatchSize         int           // batch size for vector upserts (default: 100)
	BatchTimeout      time.Duration // batch timeout for vector upserts (default: 200ms)
	LogToFile           bool          // log output to physical file option (default: false)
	SearchMode          string        // search mode: "dense", "sparse", or "hybrid" (default: "dense")
	ExcludeExtensions   []string      // file extensions to exclude from indexing (default: .sql)
	MaxFileSize         int64         // maximum file size in bytes to index (default: 1MB)
	OllamaNumCtx        int           // Ollama num_ctx override (default: 8192)
	Branch        string // current git branch, auto-detected at startup
	DefaultBranch string // repo default branch, auto-detected at startup
}


func LoadConfig() Config {
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

	maxWorkers := 5
	if workerStr := os.Getenv("MAX_EMBEDDING_WORKERS"); workerStr != "" {
		if w, err := strconv.Atoi(workerStr); err == nil && w > 0 {
			maxWorkers = w
		} else {
			log.Printf("Warning: MAX_EMBEDDING_WORKERS '%s' is not a valid positive integer, falling back to default 5", workerStr)
		}
	}

	batchSize := 100
	if batchSizeStr := os.Getenv("BATCH_SIZE"); batchSizeStr != "" {
		if b, err := strconv.Atoi(batchSizeStr); err == nil && b > 0 {
			batchSize = b
		} else {
			log.Printf("Warning: BATCH_SIZE '%s' is not a valid positive integer, falling back to default 100", batchSizeStr)
		}
	}

	batchTimeout := 200 * time.Millisecond
	if batchTimeoutStr := os.Getenv("BATCH_TIMEOUT"); batchTimeoutStr != "" {
		if d, err := time.ParseDuration(batchTimeoutStr); err == nil && d > 0 {
			batchTimeout = d
		} else {
			log.Printf("Warning: BATCH_TIMEOUT '%s' is not a valid duration, falling back to default 200ms", batchTimeoutStr)
		}
	}

	logToFile := false
	if logToFileStr := os.Getenv("LOG_TO_FILE"); logToFileStr != "" {
		if b, err := strconv.ParseBool(logToFileStr); err == nil {
			logToFile = b
		} else {
			log.Printf("Warning: LOG_TO_FILE '%s' is not a valid boolean, falling back to default false", logToFileStr)
		}
	}

	searchMode := "dense"
	if searchModeStr := strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_MODE"))); searchModeStr != "" {
		if searchModeStr == "dense" || searchModeStr == "sparse" || searchModeStr == "hybrid" {
			searchMode = searchModeStr
		} else {
			log.Printf("Warning: SEARCH_MODE '%s' is not valid (must be dense, sparse, or hybrid), falling back to default 'dense'", searchModeStr)
		}
	}

	excludeExts := parseEnvArray("EXCLUDE_EXTENSIONS")
	if os.Getenv("EXCLUDE_EXTENSIONS") == "" {
		// Provide .sql as a safe out-of-the-box default
		excludeExts = []string{".sql"}
	} else {
		// Normalize extension formats (ensure leading dot exists)
		for i, ext := range excludeExts {
			if ext != "" && !strings.HasPrefix(ext, ".") {
				excludeExts[i] = "." + ext
			}
		}
	}

	maxFileSize := int64(1024 * 1024) // 1MB default
	if maxFileStr := os.Getenv("MAX_FILE_SIZE_BYTES"); maxFileStr != "" {
		if m, err := strconv.ParseInt(maxFileStr, 10, 64); err == nil && m > 0 {
			maxFileSize = m
		} else {
			log.Printf("Warning: MAX_FILE_SIZE_BYTES '%s' is not a valid positive integer, falling back to default 1MB", maxFileStr)
		}
	}

	ollamaNumCtx := 8192
	if numCtxStr := os.Getenv("OLLAMA_NUM_CTX"); numCtxStr != "" {
		if n, err := strconv.Atoi(numCtxStr); err == nil && n > 0 {
			ollamaNumCtx = n
		} else {
			log.Printf("Warning: OLLAMA_NUM_CTX '%s' is not a valid positive integer, falling back to default 8192", numCtxStr)
		}
	}

	cfg := Config{
		QdrantHost:          host,
		QdrantPort:          port,
		CollectionName:      os.Getenv("QDRANT_COLLECTION"),
		WatchDirectory:      os.Getenv("WATCH_DIRECTORY"),
		OllamaHost:          os.Getenv("OLLAMA_HOST"),
		EmbeddingModel:      os.Getenv("EMBEDDING_MODEL"),
		DebounceDuration:    800 * time.Millisecond,
		ExcludeDirs:         parseEnvArray("EXCLUDE_DIRS"),
		IncludeHiddenDirs:   parseEnvArray("INCLUDE_HIDDEN_DIRS"),
		ParserMode:          parserMode,
		MaxEmbeddingWorkers: maxWorkers,
		BatchSize:           batchSize,
		BatchTimeout:        batchTimeout,
		LogToFile:           logToFile,
		SearchMode:          searchMode,
		ExcludeExtensions:   excludeExts,
		MaxFileSize:         maxFileSize,
		OllamaNumCtx:        ollamaNumCtx,
	}
	cfg.Branch, cfg.DefaultBranch = DetectBranches(cfg.WatchDirectory)
	log.Printf("Branch context: current=%q default=%q", cfg.Branch, cfg.DefaultBranch)
	return cfg
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

// PreprocessConfig parses command-line arguments for server configuration parameters
// and auto-discovers configuration settings in files (.mcp.json, settings.local.json, etc.)
// upward from the current directory, and user-level Claude settings.
// Variables set through CLI flags override shell environment variables, which in turn
// override auto-discovered configuration file settings.
func PreprocessConfig() {
	// 1. Keep track of what environment variables are already set by the terminal shell.
	shellEnvVars := make(map[string]bool)
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			shellEnvVars[parts[0]] = true
		}
	}

	// 2. Parse explicit CLI flags/parameters first.
	cliEnvVars := ParseCLIFlags(os.Args[1:])

	// Set CLI variables inside the process environment.
	// We also record them so we know they were explicitly specified by the user.
	for k, v := range cliEnvVars {
		os.Setenv(k, v)
	}

	// 3. Auto-discover configuration files if any variables are still missing.
	requiredKeys := []string{
		"QDRANT_HOST", "QDRANT_PORT", "QDRANT_COLLECTION", "WATCH_DIRECTORY",
		"OLLAMA_HOST", "EMBEDDING_MODEL", "EXCLUDE_DIRS", "INCLUDE_HIDDEN_DIRS",
		"PARSER_MODE", "MAX_EMBEDDING_WORKERS", "OLLAMA_NUM_CTX",
	}

	hasMissing := false
	for _, key := range requiredKeys {
		if _, byCLI := cliEnvVars[key]; !byCLI {
			if _, byShell := shellEnvVars[key]; !byShell {
				hasMissing = true
				break
			}
		}
	}

	if hasMissing {
		discovered := discoverConfigs(cliEnvVars, shellEnvVars)
		// Apply discovered variables to the environment ONLY if not set by CLI or Shell
		for k, v := range discovered {
			if _, byCLI := cliEnvVars[k]; !byCLI {
				if _, byShell := shellEnvVars[k]; !byShell {
					os.Setenv(k, v)
				}
			}
		}
	}
}

func ParseCLIFlags(args []string) map[string]string {
	flags := make(map[string]string)
	
	// Map of flag names to environment variable names
	flagMap := map[string]string{
		"--collection":          "QDRANT_COLLECTION",
		"-c":                   "QDRANT_COLLECTION",
		"--watch-dir":           "WATCH_DIRECTORY",
		"-w":                   "WATCH_DIRECTORY",
		"--ollama":             "OLLAMA_HOST",
		"-o":                   "OLLAMA_HOST",
		"--embedding":          "EMBEDDING_MODEL",
		"-e":                   "EMBEDDING_MODEL",
		"--qdrant-host":        "QDRANT_HOST",
		"-qh":                  "QDRANT_HOST",
		"--qdrant-port":        "QDRANT_PORT",
		"-qp":                  "QDRANT_PORT",
		"--exclude-dirs":       "EXCLUDE_DIRS",
		"-xd":                  "EXCLUDE_DIRS",
		"--include-hidden-dirs": "INCLUDE_HIDDEN_DIRS",
		"-ihd":                 "INCLUDE_HIDDEN_DIRS",
		"--parser-mode":        "PARSER_MODE",
		"-pm":                  "PARSER_MODE",
		"--max-workers":        "MAX_EMBEDDING_WORKERS",
		"-mw":                  "MAX_EMBEDDING_WORKERS",
		"--batch-size":         "BATCH_SIZE",
		"-bs":                  "BATCH_SIZE",
		"--batch-timeout":      "BATCH_TIMEOUT",
		"-bt":                  "BATCH_TIMEOUT",
		"--log-to-file":        "LOG_TO_FILE",
		"-lf":                  "LOG_TO_FILE",
		"--search-mode":        "SEARCH_MODE",
		"-sm":                  "SEARCH_MODE",
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if envKey, exists := flagMap[arg]; exists {
			if i+1 < len(args) {
				flags[envKey] = args[i+1]
				// We don't remove from os.Args, just advance loop to consume the flag value
				i++
			}
		} else if strings.Contains(arg, "=") {
			// support --flag=value format
			parts := strings.SplitN(arg, "=", 2)
			if envKey, exists := flagMap[parts[0]]; exists {
				flags[envKey] = parts[1]
			}
		}
	}

	return flags
}

func discoverConfigs(cliEnv map[string]string, shellEnv map[string]bool) map[string]string {
	discovered := make(map[string]string)

	// 1. Check user-level Claude settings (~/.claude/settings.json)
	home, err := os.UserHomeDir()
	if err == nil {
		userClaudePath := filepath.Join(home, ".claude", "settings.json")
		if vars := LoadJsonConfig(userClaudePath); len(vars) > 0 {
			mergeMaps(discovered, vars)
		}
	}

	// 2. Discover configs going upwards from current directory
	cwd, err := os.Getwd()
	if err == nil {
		dir := cwd
		for {
			// Look for .mcp.json or mcp.json
			mcpJsonPath := filepath.Join(dir, ".mcp.json")
			if vars := LoadJsonConfig(mcpJsonPath); len(vars) > 0 {
				mergeMaps(discovered, vars)
				break
			}
			mcpJsonPath2 := filepath.Join(dir, "mcp.json")
			if vars := LoadJsonConfig(mcpJsonPath2); len(vars) > 0 {
				mergeMaps(discovered, vars)
				break
			}

			// Look for .claude/settings.local.json
			claudeLocalPath := filepath.Join(dir, ".claude", "settings.local.json")
			if vars := LoadJsonConfig(claudeLocalPath); len(vars) > 0 {
				mergeMaps(discovered, vars)
				break
			}

			// Look for .codex/config.toml or config.toml
			tomlPath := filepath.Join(dir, ".codex", "config.toml")
			if vars := LoadTomlConfig(tomlPath); len(vars) > 0 {
				mergeMaps(discovered, vars)
				break
			}
			tomlPath2 := filepath.Join(dir, "config.toml")
			if vars := LoadTomlConfig(tomlPath2); len(vars) > 0 {
				mergeMaps(discovered, vars)
				break
			}

			// Go to parent directory
			parent := filepath.Dir(dir)
			if parent == dir {
				break // reached filesystem root
			}
			dir = parent
		}
	}

	return discovered
}

func mergeMaps(dest, src map[string]string) {
	for k, v := range src {
		dest[k] = v
	}
}

func LoadJsonConfig(path string) map[string]string {
	vars := make(map[string]string)

	file, err := os.Open(path)
	if err != nil {
		return vars
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return vars
	}

	// We use a generic interface to cleanly navigate arbitrary JSON shapes
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return vars
	}

	// Look for mcpServers or similar key case-insensitively
	var mcpServers interface{}
	for k, v := range raw {
		if strings.ToLower(k) == "mcpservers" || strings.ToLower(k) == "mcp_servers" {
			mcpServers = v
			break
		}
	}

	if mcpServers == nil {
		return vars
	}

	serversMap, ok := mcpServers.(map[string]interface{})
	if !ok {
		return vars
	}

	// Heuristic 1: Look for specific keys
	candidates := []string{"qdrant-memory-rag", "qdrant-mcp-server", "qdrant"}
	var targetServer interface{}

	for _, cand := range candidates {
		if s, exists := serversMap[cand]; exists {
			targetServer = s
			break
		}
	}

	// Heuristic 2: Look for a server with a command containing "qdrant-mcp-server"
	if targetServer == nil {
		for _, s := range serversMap {
			sMap, ok := s.(map[string]interface{})
			if !ok {
				continue
			}
			if cmd, exists := sMap["command"]; exists {
				if cmdStr, ok := cmd.(string); ok && strings.Contains(cmdStr, "qdrant-mcp-server") {
					targetServer = s
					break
				}
			}
		}
	}

	if targetServer == nil {
		return vars
	}

	sMap, ok := targetServer.(map[string]interface{})
	if !ok {
		return vars
	}

	if envObj, exists := sMap["env"]; exists {
		if envMap, ok := envObj.(map[string]interface{}); ok {
			for k, v := range envMap {
				if valStr, ok := v.(string); ok {
					vars[k] = valStr
				} else if valNum, ok := v.(float64); ok {
					vars[k] = strconv.FormatFloat(valNum, 'f', -1, 64)
				} else if valBool, ok := v.(bool); ok {
					vars[k] = strconv.FormatBool(valBool)
				}
			}
		}
	}

	if len(vars) > 0 {
		log.Printf("Auto-Discovery: Loaded %d environment variables from %s", len(vars), path)
	}

	return vars
}

func LoadTomlConfig(path string) map[string]string {
	vars := make(map[string]string)

	file, err := os.Open(path)
	if err != nil {
		return vars
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return vars
	}

	lines := strings.Split(string(data), "\n")
	inTargetSection := false

	// Target TOML headers (e.g. [mcp_servers.qdrant-memory-rag.env] or similar)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			header := strings.ToLower(trimmed)
			// Match headers like [mcp_servers.qdrant-memory-rag.env] or [mcp_servers.qdrant-mcp-server.env] or [mcp_servers.qdrant.env]
			if strings.HasPrefix(header, "[mcp_servers.") && strings.HasSuffix(header, ".env]") {
				serverName := header[len("[mcp_servers.") : len(header)-len(".env]")]
				if strings.Contains(serverName, "qdrant") {
					inTargetSection = true
				} else {
					inTargetSection = false
				}
			} else {
				inTargetSection = false
			}
			continue
		}

		if inTargetSection {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				// Strip quotes from string values
				val = strings.Trim(val, `"'`)
				vars[key] = val
			}
		}
	}

	if len(vars) > 0 {
		log.Printf("Auto-Discovery: Loaded %d environment variables from %s", len(vars), path)
	}

	return vars
}
