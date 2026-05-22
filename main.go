package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
)

// --- Configuration Struct ---
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

// --- Ingestion Structural Blocks ---
type IngestionWorker struct {
	cfg          Config
	qdrantClient *qdrant.Client
	httpClient   *http.Client
	mu           sync.Mutex
	pendingFiles map[string]time.Time
	activeSyncs  int
	totalSynced  int
}

type OllamaEmbedReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type OllamaEmbedResp struct {
	Embedding []float32 `json:"embedding"`
}

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

func (iw *IngestionWorker) syncFileState(ctx context.Context, path string) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		log.Printf("File removed on dev machine, purging remote vectors: %s", path)
		_ = iw.purgeFileVectors(ctx, path)
		return
	} else if err != nil {
		return
	}

	if info.IsDir() {
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Unable to read local content block: %v", err)
		return
	}

	// Purge historical offsets right before re-indexing to ensure stale lines wipe out
	_ = iw.purgeFileVectors(ctx, path)

	if len(content) == 0 {
		return
	}

	// Segment structural boundaries line-by-line or by chunk blocks
	chunks := iw.chunkText(string(content), 1000)
	var points []*qdrant.PointStruct

	for idx, chunk := range chunks {
		vector, err := iw.fetchRemoteEmbedding(ctx, chunk)
		if err != nil {
			log.Printf("Ollama vectorization failed on remote endpoint: %v", err)
			continue
		}

		// Ensure unique points across workspace updates
		deterministicSeed := fmt.Sprintf("%s_chunk_%d", path, idx)
		hash := sha1.Sum([]byte(deterministicSeed))
		id, _ := uuid.FromBytes(hash[:16])

		points = append(points, &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(id.String()),
			Vectors: qdrant.NewVectors(vector...),
			Payload: qdrant.NewValueMap(map[string]interface{}{
				"file_path": path,
				"content":   chunk,
				"updated":   time.Now().Unix(),
			}),
		})
	}

	if len(points) > 0 {
		_, err = iw.qdrantClient.Upsert(ctx, &qdrant.UpsertPoints{
			CollectionName: iw.cfg.CollectionName,
			Points:         points,
		})
		if err != nil {
			log.Printf("gRPC Batch Upsert onto collection '%s' failed: %v", iw.cfg.CollectionName, err)
		} else {
			log.Printf("Successfully synchronized %d vectors for %s", len(points), path)
		}
	}
}

func (iw *IngestionWorker) purgeFileVectors(ctx context.Context, path string) error {
	_, err := iw.qdrantClient.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: iw.cfg.CollectionName,
		// The client uses Points directly now, which maps to a points selector.
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchKeyword("file_path", path),
			},
		}),
	})
	return err
}

func (iw *IngestionWorker) SyncWorkspace(ctx context.Context) (int, error) {
	// 1. Ensure the dedicated collection exists
	exists, err := iw.qdrantClient.CollectionExists(ctx, iw.cfg.CollectionName)
	if err != nil {
		return 0, fmt.Errorf("failed to check collection existence: %w", err)
	}

	if !exists {
		// Call Ollama to determine embedding dimension size dynamically
		dummyVector, err := iw.fetchRemoteEmbedding(ctx, "hello")
		if err != nil {
			return 0, fmt.Errorf("failed to fetch dummy embedding from Ollama model '%s' at %s: %w", 
				iw.cfg.EmbeddingModel, iw.cfg.OllamaHost, err)
		}
		dimension := uint64(len(dummyVector))
		log.Printf("Collection '%s' does not exist. Creating it with dimension size %d...", iw.cfg.CollectionName, dimension)

		err = iw.qdrantClient.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: iw.cfg.CollectionName,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     dimension,
				Distance: qdrant.Distance_Cosine,
			}),
		})
		if err != nil {
			return 0, fmt.Errorf("failed to create Qdrant collection '%s': %w", iw.cfg.CollectionName, err)
		}
		log.Printf("Collection '%s' successfully created.", iw.cfg.CollectionName)
	}

	// 2. Discover files
	var filesToIngest []string
	err = filepath.WalkDir(iw.cfg.WatchDirectory, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if sliceContains(iw.cfg.ExcludeDirs, base) {
				return filepath.SkipDir
			}
			if strings.HasPrefix(base, ".") && base != "." {
				if !sliceContains(iw.cfg.IncludeHiddenDirs, base) {
					return filepath.SkipDir
				}
			}
		} else {
			baseName := d.Name()
			isAllowedHiddenPath := false
			for _, allowedDir := range iw.cfg.IncludeHiddenDirs {
				if strings.Contains(path, "/"+allowedDir+"/") {
					isAllowedHiddenPath = true
					break
				}
			}
			if (strings.HasPrefix(baseName, ".") && !isAllowedHiddenPath) ||
				strings.HasPrefix(baseName, "~") ||
				strings.HasSuffix(baseName, ".tmp") {
				return nil
			}
			filesToIngest = append(filesToIngest, path)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("error walking watch directory: %w", err)
	}

	log.Printf("Found %d files to ingest. Starting ingestion...", len(filesToIngest))
	for i, path := range filesToIngest {
		log.Printf("[%d/%d] Ingesting %s...", i+1, len(filesToIngest), path)
		iw.syncFileState(ctx, path)
	}

	return len(filesToIngest), nil
}

func (iw *IngestionWorker) fetchRemoteEmbedding(ctx context.Context, text string) ([]float32, error) {
	payload, _ := json.Marshal(OllamaEmbedReq{Model: iw.cfg.EmbeddingModel, Prompt: text})
	req, _ := http.NewRequestWithContext(ctx, "POST", iw.cfg.OllamaHost+"/api/embeddings", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := iw.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out OllamaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Embedding, nil
}

func (iw *IngestionWorker) chunkText(text string, size int) []string {
	// Basic sliding/line chunking engine
	lines := strings.Split(text, "\n")
	var chunks []string
	var currentChunk strings.Builder

	for _, line := range lines {
		if currentChunk.Len()+len(line) > size {
			chunks = append(chunks, currentChunk.String())
			currentChunk.Reset()
		}
		currentChunk.WriteString(line + "\n")
	}
	if currentChunk.Len() > 0 {
		chunks = append(chunks, currentChunk.String())
	}
	return chunks
}

// --- Basic MCP Transport Subsystem ---
// --- Enhanced MCP Protocol Structural Blocks ---
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      json.RawMessage `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type SearchArguments struct {
	Query string `json:"query"`
}

func (iw *IngestionWorker) listenToMCPClient(ctx context.Context) {
	dec := json.NewDecoder(os.Stdin)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			var req MCPRequest
			if err := dec.Decode(&req); err != nil {
				if err == io.EOF {
					return
				}
				continue
			}
			// Route base protocol signals (e.g. initialize, tools/list)
			iw.handleMCPMethod(req)
		}
	}
}

func (iw *IngestionWorker) handleMCPMethod(req MCPRequest) {
	// 1. Connection Handshake Protocol Block
	if req.Method == "initialize" {
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]string{
					"name":    "go-qdrant-sync-mcp",
					"version": "1.0.0",
				},
			},
		}
		out, _ := json.Marshal(response)
		fmt.Println(string(out))
		return
	}

	// 2. Capabilities Protocol Declaration Block
	if req.Method == "tools/list" {
		response := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]interface{}{
				"tools": []map[string]interface{}{
					{
						"name":        "qdrant_search",
						"description": "Search your local codebases via semantic vector queries hosted on your home lab server. Use this to find implementation patterns, look up technical definitions, or trace structural business logic context.",
						"inputSchema": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"query": map[string]interface{}{
									"type":        "string",
									"description": "The explicit semantic search query string (e.g., 'JWT authentication filter middleware' or 'WPF custom control XAML templates').",
								},
							},
							"required": []string{"query"},
						},
					},
					{
						"name":        "get_sync_status",
						"description": "Retrieve the real-time status of the codebase vector ingestion pipeline. Use this to check if files are still being indexed, how many files are queued for debouncing, and how many files have been successfully synchronized.",
						"inputSchema": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
					{
						"name":        "ingest_workspace",
						"description": "Trigger a full recursive scan and ingestion of all non-ignored files in the workspace directory. Use this to seed/index a new project or force a complete synchronization with Qdrant.",
						"inputSchema": map[string]interface{}{
							"type":       "object",
							"properties": map[string]interface{}{},
						},
					},
				},
			},
		}
		out, _ := json.Marshal(response)
		fmt.Println(string(out))
		return
	}

	// 3. Execution Processing Block (The Upgrade)
	if req.Method == "tools/call" {
		var params CallToolParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			iw.sendMCPError(req.ID, -32602, "Invalid tool call parameters")
			return
		}

		if params.Name == "qdrant_search" {
			var args SearchArguments
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				iw.sendMCPError(req.ID, -32602, "Invalid search arguments format")
				return
			}

			// Process the RAG search query across the local network interface
			go func() {
				resultsText, err := iw.executeVectorSearch(context.Background(), args.Query)
				if err != nil {
					log.Printf("Internal RAG search failed: %v", err)
					iw.sendMCPError(req.ID, -32603, fmt.Sprintf("Search execution error: %v", err))
					return
				}

				// Respond directly to the active IDE context stream window
				response := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]interface{}{
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": resultsText,
							},
						},
					},
				}
				out, _ := json.Marshal(response)
				fmt.Println(string(out))
			}()
		} else if params.Name == "get_sync_status" {
			iw.mu.Lock()
			status := "idle"
			if len(iw.pendingFiles) > 0 || iw.activeSyncs > 0 {
				status = "syncing"
			}

			pendingList := []string{}
			for p := range iw.pendingFiles {
				pendingList = append(pendingList, p)
			}

			pendingCount := len(iw.pendingFiles)
			activeCount := iw.activeSyncs
			totalCount := iw.totalSynced
			iw.mu.Unlock()

			// Format output nicely in Markdown
			var sb strings.Builder
			sb.WriteString("### 🔄 Code Ingestion Sync Status\n\n")
			sb.WriteString(fmt.Sprintf("- **Status:** `%s`\n", status))
			sb.WriteString(fmt.Sprintf("- **Queue Size (Debouncing):** `%d`\n", pendingCount))
			sb.WriteString(fmt.Sprintf("- **Active Indexing Threads:** `%d`\n", activeCount))
			sb.WriteString(fmt.Sprintf("- **Lifetime Synced Files:** `%d`\n", totalCount))

			if len(pendingList) > 0 {
				sb.WriteString("\n#### ⏳ Files Currently in Debounce Queue:\n")
				for _, p := range pendingList {
					sb.WriteString(fmt.Sprintf("- `%s`\n", p))
				}
			}

			response := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]interface{}{
					"content": []map[string]interface{}{
						{
							"type": "text",
							"text": sb.String(),
						},
					},
				},
			}
			out, _ := json.Marshal(response)
			fmt.Println(string(out))
		} else if params.Name == "ingest_workspace" {
			go func() {
				count, err := iw.SyncWorkspace(context.Background())
				if err != nil {
					log.Printf("Internal codebase ingestion failed: %v", err)
					iw.sendMCPError(req.ID, -32603, fmt.Sprintf("Ingestion error: %v", err))
					return
				}

				// Respond directly to the active IDE context stream window
				var sb strings.Builder
				sb.WriteString("### 🚀 Codebase Ingestion Complete\n\n")
				sb.WriteString(fmt.Sprintf("Successfully scanned and synchronized **%d** files into the Qdrant collection `%s`.\n", count, iw.cfg.CollectionName))

				response := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]interface{}{
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": sb.String(),
							},
						},
					},
				}
				out, _ := json.Marshal(response)
				fmt.Println(string(out))
			}()
		} else {
			iw.sendMCPError(req.ID, -32601, "Requested tool execution target not found")
		}
		return
	}
}

// Helper tool to safely write standardized JSON-RPC protocol error contexts
func (iw *IngestionWorker) sendMCPError(id json.RawMessage, code int, message string) {
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
	}
	out, _ := json.Marshal(response)
	fmt.Println(string(out))
}

func (iw *IngestionWorker) executeVectorSearch(ctx context.Context, query string) (string, error) {
	// Step A: Vectorize the search query using your home lab Ollama endpoint
	vector, err := iw.fetchRemoteEmbedding(ctx, query)
	if err != nil {
		return "", fmt.Errorf("failed to generate embedding for query: %w", err)
	}

	// Step B: Direct a high-speed gRPC Query request to your Qdrant collection
	// Retrieve top 5 closest matching context code sheets
	queryResponse, err := iw.qdrantClient.Query(ctx, &qdrant.QueryPoints{
		CollectionName: iw.cfg.CollectionName,
		Query:          qdrant.NewQueryDense(vector),
		Limit:          qdrant.PtrOf(uint64(5)),
		WithPayload:    qdrant.NewWithPayloadEnable(true), // Ensure code text comes back
	})
	if err != nil {
		return "", fmt.Errorf("qdrant search operation failed: %w", err)
	}

	if len(queryResponse) == 0 {
		return "No relevant structural code blocks or reference components were found matching your query scope.", nil
	}

	// Step C: Marshal points cleanly into an aggregate Markdown context layout
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Core Codebase Reference Snippets for: \"%s\"\n\n", query))

	for i, point := range queryResponse {
		payloadMap := point.Payload
		filePath := "Unknown Location"
		contentChunk := "Empty Source"

		if pathVal, exists := payloadMap["file_path"]; exists {
			filePath = pathVal.GetStringValue()
		}
		if contentVal, exists := payloadMap["content"]; exists {
			contentChunk = contentVal.GetStringValue()
		}

		// Detect target language parsing extensions for beautiful Markdown injection blocks
		lang := detectLanguage(filePath)

		sb.WriteString(fmt.Sprintf("#### [%d] Source File: %s (Match Score: %.2f)\n", i+1, filePath, point.Score))
		sb.WriteString(fmt.Sprintf("```%s\n", lang))
		sb.WriteString(contentChunk)
		if !strings.HasSuffix(contentChunk, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	return sb.String(), nil
}

func detectLanguage(filePath string) string {
	lexer := lexers.Match(filePath)
	if lexer != nil {
		config := lexer.Config()
		if len(config.Aliases) > 0 && config.Aliases[0] != "text" {
			return config.Aliases[0]
		}
	}

	// Custom fallbacks
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".xaml", ".xml", ".csproj", ".fsproj", ".sln":
		return "xml"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".md", ".markdown":
		return "markdown"
	case "":
		return "text"
	default:
		return strings.TrimPrefix(ext, ".")
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
	fmt.Println("==================================================================")
}
