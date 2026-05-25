package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"github.com/qdrant/go-client/qdrant"
)

var Version = "1.0.0"

func Start(version string) {
	Version = version

	// Preprocess CLI flags and auto-discover configuration files
	PreprocessConfig()

	// Setup localized logs redirected away from stdout to keep MCP channel clean
	log.SetOutput(os.Stderr)

	// Configure physical log file if option enabled
	cfg := LoadConfig()
	if cfg.LogToFile {
		dirPath := ".qdrant-mcp-server"
		if err := os.MkdirAll(dirPath, 0755); err != nil {
			log.Printf("Warning: Failed to create log directory '%s': %v", dirPath, err)
		}
		logFilePath := filepath.Join(dirPath, "qdrant-mcp-server.log")
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.Printf("Warning: Failed to open log file '%s': %v", logFilePath, err)
		} else {
			log.SetOutput(logFile)
			log.Println("--- Log Session Started ---")
		}
	}

	// Intercept command line arguments for skill generation
	if len(os.Args) > 1 {
		cmd := strings.ToLower(os.Args[1])
		switch cmd {
		case "list-skills", "-list", "--list":
			ListSkills()
			return
		case "install-skill", "-install", "--install", "install":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "Error: missing agent name.")
				fmt.Fprintln(os.Stderr, "Usage: qdrant-mcp-server install-skill <agent|all> [destination_directory]")
				os.Exit(1)
			}
			agent := os.Args[2]
			destDir := ""
			if len(os.Args) > 3 {
				destDir = os.Args[3]
			}
			if err := InstallSkill(agent, destDir); err != nil {
				fmt.Fprintf(os.Stderr, "Error installing skill: %v\n", err)
				os.Exit(1)
			}
			return
		case "ingest", "-ingest", "--ingest":
			cfg, client, worker := mustCreateWorker()
			defer client.Close()
			defer worker.Close()

			log.Println("Starting manual codebase ingestion...")
			count, err := worker.SyncWorkspace(context.Background())
			if err != nil {
				log.Fatalf("Error during manual ingestion: %v", err)
			}
			fmt.Printf("🎉 Success! Ingested %d files into collection '%s'.\n", count, cfg.CollectionName)
			return
		case "search", "-search", "--search":
			if len(os.Args) < 3 {
				fmt.Fprintln(os.Stderr, "Error: missing query.")
				fmt.Fprintln(os.Stderr, "Usage: qdrant-mcp-server search <query>")
				os.Exit(1)
			}
			_, client, worker := mustCreateWorker()
			defer client.Close()
			defer worker.Close()

			query := strings.Join(os.Args[2:], " ")
			results, err := worker.ExecuteVectorSearch(context.Background(), query, nil, "")
			if err != nil {
				log.Fatalf("Search failed: %v", err)
			}
			fmt.Println(results)
			return
		case "evaluate-search", "eval-search", "eval":
			cfg, client, worker := mustCreateWorker()
			defer client.Close()
			defer worker.Close()

			log.Printf("Running self-evaluation against workspace %s", cfg.WatchDirectory)
			if _, err := worker.SyncWorkspace(context.Background()); err != nil {
				log.Fatalf("Self-ingestion failed before evaluation: %v", err)
			}

			suites := defaultEvaluationQueries(cfg.WatchDirectory)
			passed := 0
			for _, suite := range suites {
				result, err := worker.ExecuteVectorSearch(context.Background(), suite.Query, suite.FileExtensions, suite.PathPrefix)
				if err != nil {
					log.Printf("Evaluation query failed for %q: %v", suite.Query, err)
					continue
				}
				ok := strings.Contains(strings.ToLower(result), strings.ToLower(suite.ExpectContains))
				if ok {
					passed++
				}
				fmt.Printf("\n=== Query: %s ===\nExpected: %s\nPass: %t\n\n%s\n", suite.Query, suite.ExpectContains, ok, result)
			}
			fmt.Printf("\nEvaluation summary: %d/%d queries matched expected fragments.\n", passed, len(suites))
			return
		case "help", "-h", "--help":
			printCLIHelp()
			return
		}
	}

	log.Println("Starting Go Qdrant-RAG MCP Server...")

	cfg = LoadConfig()
	if cfg.CollectionName == "" || cfg.WatchDirectory == "" || cfg.OllamaHost == "" {
		log.Fatal("Fatal: Missing required environment variables (QDRANT_COLLECTION, WATCH_DIRECTORY, OLLAMA_HOST)")
	}

	// Connect to home lab Qdrant via fast gRPC
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   cfg.QdrantHost,
		Port:                   cfg.QdrantPort,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		log.Fatalf("Failed to establish Qdrant connection: %v", err)
	}
	defer client.Close()

	gitIgnore := NewGitIgnoreMatcher(cfg.WatchDirectory)

	worker := NewIngestionWorker(cfg, client, gitIgnore)
	defer worker.Close()

	// Boot active structural watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to spin up filesystem notification systems: %v", err)
	}
	defer watcher.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancelHandle(cancel)

	// Spawn decoupled debounced file processor
	eventChan := make(chan string, 100)
	go worker.WatchLoop(ctx, watcher, eventChan)
	go worker.IngestionConsumer(ctx, eventChan)

	// Recursively monitor target codebase structure
	err = worker.addWatchRecursive(watcher, cfg.WatchDirectory)
	if err != nil {
		log.Printf("Warning: Directory traversal hit path restrictions: %v", err)
	}

	// Launch standard MCP protocol engine on main thread
	go worker.ListenToMCPClient(ctx)

	// Block gracefully until system signal caught
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down Go MCP Server cleanly.")
}

func cancelHandle(c context.CancelFunc) { c() }

type EvaluationQuery struct {
	Query          string
	ExpectContains string
	FileExtensions []string
	PathPrefix     string
}

func mustCreateWorker() (Config, *qdrant.Client, *IngestionWorker) {
	cfg := LoadConfig()
	if cfg.CollectionName == "" || cfg.WatchDirectory == "" || cfg.OllamaHost == "" {
		log.Fatal("Fatal: Missing required environment variables (QDRANT_COLLECTION, WATCH_DIRECTORY, OLLAMA_HOST)")
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   cfg.QdrantHost,
		Port:                   cfg.QdrantPort,
		SkipCompatibilityCheck: true,
	})
	if err != nil {
		log.Fatalf("Failed to establish Qdrant connection: %v", err)
	}

	gitIgnore := NewGitIgnoreMatcher(cfg.WatchDirectory)
	worker := NewIngestionWorker(cfg, client, gitIgnore)
	return cfg, client, worker
}

func defaultEvaluationQueries(watchDir string) []EvaluationQuery {
	return []EvaluationQuery{
		{Query: "recursive watcher for newly created directories", ExpectContains: "server/watcher.go", FileExtensions: []string{"go"}, PathPrefix: "server"},
		{Query: "vector search execution and reranking", ExpectContains: "server/worker.go", FileExtensions: []string{"go"}, PathPrefix: "server"},
		{Query: "auto discover mcp and codex config", ExpectContains: "server/config.go", FileExtensions: []string{"go"}, PathPrefix: "server"},
		{Query: "tree sitter parse code metadata and imports", ExpectContains: "ast/ast.go", FileExtensions: []string{"go"}, PathPrefix: "ast"},
		{Query: "worker tags and search tests", ExpectContains: "tests/worker_test.go", FileExtensions: []string{"go"}, PathPrefix: "tests"},
	}
}

func printCLIHelp() {
	fmt.Println("\n==================================================================")
	fmt.Println("🤖 Go Qdrant-RAG MCP Server CLI")
	fmt.Println("==================================================================")
	fmt.Println("A Model Context Protocol server that implements real-time codebase")
	fmt.Println("semantic search using Qdrant & Ollama.")
	fmt.Println()
	fmt.Println("\x1b[1;33mCommands:\x1b[0m")
	fmt.Println("  (no arguments)                 Starts the active MCP server.")
	fmt.Println("  ingest                         Recursively crawl and ingest the workspace into Qdrant immediately.")
	fmt.Println("  search <query>                 Execute a direct semantic search from the CLI.")
	fmt.Println("  evaluate-search                Ingest the workspace and run canned search quality checks.")
	fmt.Println("  list-skills                    List all available AI agent skills.")
	fmt.Println("  install-skill <agent> [dir]    Installs the rules file for the specified agent.")
	fmt.Println("                                 Options: cursor, windsurf, cline, copilot, generic, codex, all.")
	fmt.Println("  help, -h, --help               Show this help information.")
	fmt.Println()
	fmt.Println("\x1b[1;33mCommand-line Parameters / Flags:\x1b[0m")
	fmt.Println("  --collection, -c <name>        Qdrant collection name")
	fmt.Println("  --watch-dir, -w <path>         Directory to watch/index")
	fmt.Println("  --ollama, -o <url>             Ollama host/URL")
	fmt.Println("  --embedding, -e <model>        Ollama embedding model name")
	fmt.Println("  --qdrant-host, -qh <host>      Qdrant server hostname/IP")
	fmt.Println("  --qdrant-port, -qp <port>      Qdrant server gRPC port")
	fmt.Println("  --exclude-dirs, -xd <list>     Comma-separated directories to skip")
	fmt.Println("  --include-hidden-dirs, -ihd    Comma-separated hidden directories to watch")
	fmt.Println("  --parser-mode, -pm <mode>      Parsing mode: 'code', 'doc', or 'full'")
	fmt.Println("  --max-workers, -mw <num>       Maximum concurrent embedding threads")
	fmt.Println("  --batch-size, -bs <num>        Batch size for vector upserts (default: 100)")
	fmt.Println("  --batch-timeout, -bt <dur>     Batch timeout for vector upserts (default: 200ms)")
	fmt.Println("  --log-to-file, -lf <bool>      Log output to physical file option (default: false)")
	fmt.Println("  --search-mode, -sm <mode>      Search mode: 'dense', 'sparse', or 'hybrid' (default: 'dense')")
	fmt.Println()
	fmt.Println("\x1b[1;33mAuto-Discovery Feature:\x1b[0m")
	fmt.Println("  The server automatically searches upwards from the current directory for")
	fmt.Println("  configuration settings in `.mcp.json`, `.claude/settings.local.json`, or")
	fmt.Println("  `.codex/config.toml` (and user-level `~/.claude/settings.json`), auto-loading")
	fmt.Println("  the configured environment variables for a seamless CLI experience.")
	fmt.Println()
	fmt.Println("\x1b[1;33mConfiguration via Environment Variables:\x1b[0m")
	fmt.Println("  QDRANT_HOST         Qdrant hostname/IP (default: 172.20.0.5)")
	fmt.Println("  QDRANT_PORT         Qdrant gRPC port (default: 6334)")
	fmt.Println("  QDRANT_COLLECTION   Collection name for vectors (Required)")
	fmt.Println("  WATCH_DIRECTORY     Directory to recursively monitor & index (Required)")
	fmt.Println("  OLLAMA_HOST         Ollama API URL (Required)")
	fmt.Println("  EMBEDDING_MODEL     Ollama model for embeddings (Required)")
	fmt.Println("  PARSER_MODE         Parsing mode: code, doc, or full (default: full)")
	fmt.Println("  MAX_EMBEDDING_WORKERS  Max concurrent embedding worker threads (default: 5)")
	fmt.Println("  BATCH_SIZE          Batch size for vector upserts (default: 100)")
	fmt.Println("  BATCH_TIMEOUT       Batch timeout for vector upserts (default: 200ms)")
	fmt.Println("  LOG_TO_FILE         Log output to physical file option (default: false)")
	fmt.Println("  SEARCH_MODE         Search mode: dense, sparse, or hybrid (default: dense)")
	fmt.Println("================================================================================")
}
