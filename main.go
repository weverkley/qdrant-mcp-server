package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/qdrant/go-client/qdrant"
)

// Version is the current version of the MCP server, injected during the build.
var Version = "1.0.0"

func main() {
	// Setup localized logs redirected away from stdout to keep MCP channel clean
	log.SetOutput(os.Stderr)

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
			cfg := loadConfig()
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
			defer client.Close()

			worker := &IngestionWorker{
				cfg:          cfg,
				qdrantClient: client,
				httpClient:   &http.Client{Timeout: 15 * time.Second},
				pendingFiles: make(map[string]time.Time),
			}

			log.Println("Starting manual codebase ingestion...")
			count, err := worker.SyncWorkspace(context.Background())
			if err != nil {
				log.Fatalf("Error during manual ingestion: %v", err)
			}
			fmt.Printf("🎉 Success! Ingested %d files into collection '%s'.\n", count, cfg.CollectionName)
			return
		case "help", "-h", "--help":
			printCLIHelp()
			return
		}
	}

	log.Println("Starting Go Qdrant-RAG MCP Server...")

	cfg := loadConfig()
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

	worker := &IngestionWorker{
		cfg:          cfg,
		qdrantClient: client,
		httpClient:   &http.Client{Timeout: 15 * time.Second},
		pendingFiles: make(map[string]time.Time),
	}

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
	go worker.watchLoop(ctx, watcher, eventChan)
	go worker.ingestionConsumer(ctx, eventChan)

	// Recursively monitor target codebase structure
	err = filepath.WalkDir(cfg.WatchDirectory, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			base := d.Name()

			// 1. Drop standard target blacklisted directory paths (e.g. node_modules)
			if sliceContains(cfg.ExcludeDirs, base) {
				return filepath.SkipDir
			}

			// 2. Evaluate general hidden folder directory paths
			if strings.HasPrefix(base, ".") && base != "." {
				// If it explicitly lives in your inclusion array, step in and watch it
				if sliceContains(cfg.IncludeHiddenDirs, base) {
					log.Printf("Dynamic Filter: Watching explicit config directory: %s", path)
					return watcher.Add(path)
				}
				// Otherwise skip it (.git, .obsidian, .codex)
				return filepath.SkipDir
			}

			return watcher.Add(path)
		}
		return nil
	})
	if err != nil {
		log.Printf("Warning: Directory traversal hit path restrictions: %v", err)
	}

	// Launch standard MCP protocol engine on main thread
	go worker.listenToMCPClient(ctx)

	// Block gracefully until system signal caught
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down Go MCP Server cleanly.")
}

func cancelHandle(c context.CancelFunc) { c() }

// --- File Watcher & Ingestion Subsystems ---
func (iw *IngestionWorker) watchLoop(ctx context.Context, watcher *fsnotify.Watcher, eventChan chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			baseName := filepath.Base(event.Name)

			// Dynamically determine if the event path matches any allowed hidden folder patterns
			isAllowedHiddenPath := false
			for _, allowedDir := range iw.cfg.IncludeHiddenDirs {
				if strings.Contains(event.Name, "/"+allowedDir+"/") {
					isAllowedHiddenPath = true
					break
				}
			}

			// Guard against general hidden files unless they belong to an explicitly allowed tree
			if (strings.HasPrefix(baseName, ".") && !isAllowedHiddenPath) ||
				strings.HasPrefix(baseName, "~") ||
				strings.HasSuffix(baseName, ".tmp") {
				continue
			}

			// Focus explicitly on functional mutation blocks
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) {
				eventChan <- event.Name
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Fsnotify interface raised exception: %v", err)
		}
	}
}

func (iw *IngestionWorker) ingestionConsumer(ctx context.Context, eventChan <-chan string) {
	timers := make(map[string]*time.Timer)

	for {
		select {
		case <-ctx.Done():
			return
		case path := <-eventChan:
			// Debounce frequent chunks to avoid thrashing remote CPU/Network resources
			if timer, exists := timers[path]; exists {
				timer.Stop()
			}

			iw.mu.Lock()
			iw.pendingFiles[path] = time.Now()
			iw.mu.Unlock()

			timers[path] = time.AfterFunc(iw.cfg.DebounceDuration, func() {
				iw.mu.Lock()
				delete(iw.pendingFiles, path)
				iw.activeSyncs++
				iw.mu.Unlock()

				defer func() {
					iw.mu.Lock()
					iw.activeSyncs--
					iw.totalSynced++
					iw.mu.Unlock()
				}()

				iw.syncFileState(context.Background(), path)
			})
		}
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
	fmt.Println("  list-skills                    List all available AI agent skills.")
	fmt.Println("  install-skill <agent> [dir]    Installs the rules file for the specified agent.")
	fmt.Println("                                 Options: cursor, windsurf, cline, copilot, generic, codex, all.")
	fmt.Println("  help, -h, --help               Show this help information.")
	fmt.Println()
	fmt.Println("\x1b[1;33mConfiguration via Environment Variables:\x1b[0m")
	fmt.Println("  QDRANT_HOST         Qdrant hostname/IP (default: 172.20.0.5)")
	fmt.Println("  QDRANT_PORT         Qdrant gRPC port (default: 6334)")
	fmt.Println("  QDRANT_COLLECTION   Collection name for vectors (Required)")
	fmt.Println("  WATCH_DIRECTORY     Directory to recursively monitor & index (Required)")
	fmt.Println("  OLLAMA_HOST         Ollama API URL (Required)")
	fmt.Println("  EMBEDDING_MODEL     Ollama model for embeddings (Required)")
	fmt.Println("  PARSER_MODE         Parsing mode: code, doc, or full (default: full)")
	fmt.Println("================================================================================")
}
