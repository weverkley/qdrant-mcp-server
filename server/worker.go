package server

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"qdrant-mcp-server/ast"
)

// --- Ingestion Structural Blocks ---
type QdrantClient interface {
	Upsert(ctx context.Context, in *qdrant.UpsertPoints) (*qdrant.UpdateResult, error)
	Delete(ctx context.Context, in *qdrant.DeletePoints) (*qdrant.UpdateResult, error)
	CollectionExists(ctx context.Context, collectionName string) (bool, error)
	CreateCollection(ctx context.Context, in *qdrant.CreateCollection) error
	Query(ctx context.Context, in *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error)
	Scroll(ctx context.Context, in *qdrant.ScrollPoints) ([]*qdrant.RetrievedPoint, error)
}

type BatchUpserter struct {
	qdrantClient   QdrantClient
	collectionName string
	batchSize      int
	timeout        time.Duration
	ch             chan *qdrant.PointStruct
	flushCh        chan chan struct{}
	wg             sync.WaitGroup
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
}

func NewBatchUpserter(qdrantClient QdrantClient, collectionName string, batchSize int, timeout time.Duration) *BatchUpserter {
	if batchSize <= 0 {
		batchSize = 100
	}
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &BatchUpserter{
		qdrantClient:   qdrantClient,
		collectionName: collectionName,
		batchSize:      batchSize,
		timeout:        timeout,
		ch:             make(chan *qdrant.PointStruct, batchSize*2),
		flushCh:        make(chan chan struct{}),
		done:           make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
	}
	b.start()
	return b
}

func (b *BatchUpserter) start() {
	go func() {
		defer close(b.done)

		ticker := time.NewTicker(b.timeout)
		defer ticker.Stop()

		var batch []*qdrant.PointStruct

		drainQueue := func() {
			for {
				select {
				case p, ok := <-b.ch:
					if !ok {
						return
					}
					batch = append(batch, p)
				default:
					return
				}
			}
		}

		flushBatch := func() {
			drainQueue()
			if len(batch) == 0 {
				return
			}

			_, err := b.qdrantClient.Upsert(context.Background(), &qdrant.UpsertPoints{
				CollectionName: b.collectionName,
				Points:         batch,
			})
			if err != nil {
				log.Printf("gRPC Batch Upsert onto collection '%s' failed: %v", b.collectionName, err)
			} else {
				log.Printf("Successfully batch upserted %d vectors", len(batch))
			}

			for range batch {
				b.wg.Done()
			}
			batch = nil
		}

		for {
			select {
			case <-b.ctx.Done():
				flushBatch()
				return
			case p, ok := <-b.ch:
				if !ok {
					flushBatch()
					return
				}
				batch = append(batch, p)
				if len(batch) >= b.batchSize {
					flushBatch()
					ticker.Reset(b.timeout)
				}
			case <-ticker.C:
				if len(batch) > 0 {
					flushBatch()
				}
			case reply := <-b.flushCh:
				flushBatch()
				close(reply)
			}
		}
	}()
}

func (b *BatchUpserter) Add(p *qdrant.PointStruct) {
	b.wg.Add(1)
	select {
	case b.ch <- p:
	case <-b.ctx.Done():
		b.wg.Done()
	}
}

func (b *BatchUpserter) Flush() {
	reply := make(chan struct{})
	select {
	case b.flushCh <- reply:
		<-reply
	case <-b.ctx.Done():
		return
	}
	b.wg.Wait()
}

func (b *BatchUpserter) Close() {
	b.cancel()
	<-b.done
}

// --- Adaptive Concurrency Control ---
type ConcurrencyController struct {
	mu            sync.Mutex
	maxLimit      int
	currentLimit  int
	activeCount   int
	waiters       []chan struct{}
	successStreak int
}

func NewConcurrencyController(maxLimit int) *ConcurrencyController {
	return &ConcurrencyController{
		maxLimit:     maxLimit,
		currentLimit: maxLimit,
	}
}

func (c *ConcurrencyController) GetLimit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentLimit
}

func (c *ConcurrencyController) Acquire(ctx context.Context) error {
	c.mu.Lock()
	if c.activeCount < c.currentLimit {
		c.activeCount++
		c.mu.Unlock()
		return nil
	}

	ch := make(chan struct{})
	c.waiters = append(c.waiters, ch)
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		for i, w := range c.waiters {
			if w == ch {
				c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		return ctx.Err()
	case <-ch:
		return nil
	}
}

func (c *ConcurrencyController) Release() {
	c.mu.Lock()
	c.activeCount--
	c.notifyWaiters()
	c.mu.Unlock()
}

func (c *ConcurrencyController) notifyWaiters() {
	for c.activeCount < c.currentLimit && len(c.waiters) > 0 {
		waiter := c.waiters[0]
		c.waiters = c.waiters[1:]
		c.activeCount++
		close(waiter)
	}
}

func (c *ConcurrencyController) RecordSuccess(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if duration < 1500*time.Millisecond {
		c.successStreak++
		if c.successStreak >= 5 {
			if c.currentLimit < c.maxLimit {
				c.currentLimit++
				log.Printf("Ollama embedding responds fast (%v). Scaling concurrency limit up to %d", duration, c.currentLimit)
			}
			c.successStreak = 0
		}
	} else {
		c.successStreak = 0
		c.decreaseLimit(fmt.Sprintf("slow latency (%v)", duration))
	}
}

func (c *ConcurrencyController) RecordFailure(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.successStreak = 0
	c.decreaseLimit(reason)
}

func (c *ConcurrencyController) decreaseLimit(reason string) {
	if c.currentLimit > 1 {
		c.currentLimit = c.currentLimit / 2
		if c.currentLimit < 1 {
			c.currentLimit = 1
		}
		log.Printf("Halving concurrency limit to %d due to: %s", c.currentLimit, reason)
		c.notifyWaiters()
	}
}

type IngestionWorker struct {
	Cfg                  Config
	QdrantClient         QdrantClient
	HTTPClient           *http.Client
	Mu                   sync.Mutex
	PendingFiles         map[string]time.Time
	ActiveSyncs          int
	TotalSynced          int
	Sem                  chan struct{} // semaphore to rate limit concurrent embedding workers
	GitignoreMatcher     *GitIgnoreMatcher
	BatchUpserter        *BatchUpserter
	ConcurrencyController *ConcurrencyController
	CustomStopWords      map[string]struct{}
}

func NewIngestionWorker(cfg Config, qdrantClient QdrantClient, gitIgnore *GitIgnoreMatcher) *IngestionWorker {
	// Look for .mcp-stopwords in cfg.WatchDirectory/.qdrant-mcp-server/ or cfg.WatchDirectory
	customStopWords := make(map[string]struct{})
	if cfg.WatchDirectory != "" {
		stopWordsPath := filepath.Join(cfg.WatchDirectory, ".qdrant-mcp-server", ".mcp-stopwords")
		data, err := os.ReadFile(stopWordsPath)
		if err != nil {
			// Fallback to legacy root location
			stopWordsPath = filepath.Join(cfg.WatchDirectory, ".mcp-stopwords")
			data, err = os.ReadFile(stopWordsPath)
		}
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				token := strings.ToLower(strings.TrimSpace(line))
				// Skip empty lines or comments starting with #
				if token == "" || strings.HasPrefix(token, "#") {
					continue
				}
				customStopWords[token] = struct{}{}
			}
			log.Printf("Loaded %d custom stop-words from %s", len(customStopWords), stopWordsPath)
		}
	}

	maxWorkers := cfg.MaxEmbeddingWorkers
	if maxWorkers <= 0 {
		maxWorkers = 5
	}

	iw := &IngestionWorker{
		Cfg:              cfg,
		QdrantClient:     qdrantClient,
		HTTPClient:       &http.Client{Timeout: 15 * time.Second},
		PendingFiles:     make(map[string]time.Time),
		Sem:              make(chan struct{}, maxWorkers),
		GitignoreMatcher: gitIgnore,
		CustomStopWords:  customStopWords,
	}
	iw.BatchUpserter = NewBatchUpserter(qdrantClient, cfg.CollectionName, cfg.BatchSize, cfg.BatchTimeout)
	iw.ConcurrencyController = NewConcurrencyController(maxWorkers)
	return iw
}

func (iw *IngestionWorker) Close() {
	if iw.BatchUpserter != nil {
		iw.BatchUpserter.Close()
	}
}

func (iw *IngestionWorker) ShouldIgnoreFile(path string, isDir bool) bool {
	// Normalize path for check
	normalized := filepath.ToSlash(path)
	lowerPath := strings.ToLower(normalized)

	// Check if it's the qdrant mcp server directory itself
	if strings.Contains(normalized, ".qdrant-mcp-server") {
		return true
	}

	// Respect .gitignore
	if iw.GitignoreMatcher != nil {
		if iw.GitignoreMatcher.IsIgnored(path, isDir) {
			return true
		}
	}

	// Common autogenerated, designer, or minified files to skip (can be files or dirs)
	if strings.HasSuffix(lowerPath, ".designer.cs") ||
		strings.HasSuffix(lowerPath, "dbcontextmodelsnapshot.cs") ||
		strings.HasSuffix(lowerPath, "dbcontextsnapshot.cs") ||
		strings.Contains(lowerPath, "/migrations/") ||
		strings.HasSuffix(lowerPath, ".min.js") ||
		strings.HasSuffix(lowerPath, ".min.css") ||
		strings.HasSuffix(lowerPath, ".js.map") ||
		strings.HasSuffix(lowerPath, ".css.map") {
		return true
	}

	if isDir {
		base := filepath.Base(path)
		if sliceContains(iw.Cfg.ExcludeDirs, base) {
			return true
		}
		if strings.HasPrefix(base, ".") && base != "." {
			if !sliceContains(iw.Cfg.IncludeHiddenDirs, base) {
				return true
			}
		}
		if base == "Migrations" || base == "migrations" {
			return true
		}
	} else {
		// Exclude extensions matching config ExcludeExtensions
		ext := strings.ToLower(filepath.Ext(path))
		if sliceContains(iw.Cfg.ExcludeExtensions, ext) || sliceContains(iw.Cfg.ExcludeExtensions, strings.TrimPrefix(ext, ".")) {
			return true
		}

		baseName := filepath.Base(path)
		isAllowedHiddenPath := false
		for _, allowedDir := range iw.Cfg.IncludeHiddenDirs {
			if strings.Contains(normalized, "/"+allowedDir+"/") {
				isAllowedHiddenPath = true
				break
			}
		}
		if (strings.HasPrefix(baseName, ".") && !isAllowedHiddenPath) ||
			strings.HasPrefix(baseName, "~") ||
			strings.HasSuffix(baseName, ".tmp") {
			return true
		}
	}

	return false
}

type OllamaEmbedReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type OllamaEmbedResp struct {
	Embedding []float32 `json:"embedding"`
}

func (iw *IngestionWorker) SyncFileState(ctx context.Context, path string) {
	if iw.ShouldIgnoreFile(path, false) {
		return
	}
	if iw.Sem != nil {
		select {
		case iw.Sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-iw.Sem }()
	}

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

	// Guard against files that exceed MaxFileSize limit
	if iw.Cfg.MaxFileSize > 0 && info.Size() > iw.Cfg.MaxFileSize {
		log.Printf("Skipping large file %s (%d bytes) as it exceeds MaxFileSize (%d bytes).", path, info.Size(), iw.Cfg.MaxFileSize)
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Unable to read local content block: %v", err)
		return
	}

	if isBinaryContent(content) {
		log.Printf("Skipping binary file: %s", path)
		return
	}

	if len(content) == 0 {
		return
	}

	localHash := fmt.Sprintf("%x", sha256.Sum256(content))

	// Scroll Qdrant to check if the file hash matches
	scrollResult, err := iw.QdrantClient.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: iw.Cfg.CollectionName,
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchKeyword("file_path", path),
			},
		},
		Limit:       qdrant.PtrOf(uint32(1)),
		WithPayload: qdrant.NewWithPayloadEnable(true),
	})
	if err == nil && len(scrollResult) > 0 {
		if storedHashVal, exists := scrollResult[0].Payload["file_hash"]; exists {
			if storedHashVal.GetStringValue() == localHash {
				log.Printf("File content hash matches for %s, skipping re-indexing.", path)
				return
			}
		}
	}

	// Purge historical offsets right before re-indexing to ensure stale lines wipe out
	_ = iw.purgeFileVectors(ctx, path)

	ext := strings.ToLower(filepath.Ext(path))
	extClean := strings.TrimPrefix(ext, ".")
	relPath, _ := filepath.Rel(iw.Cfg.WatchDirectory, path)
	relDirs := convertStringSlice(getParentDirs(relPath))
	var points []*qdrant.PointStruct

	// Determine file categories based on ParserMode
	isSupportedCode := false
	isSupportedDoc := false
	switch ext {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".php", ".cs", ".py":
		if iw.Cfg.ParserMode == "code" || iw.Cfg.ParserMode == "full" {
			isSupportedCode = true
		}
	case ".pdf", ".md", ".txt", ".csv", ".xls", ".xlsx", ".doc", ".docx":
		if iw.Cfg.ParserMode == "doc" || iw.Cfg.ParserMode == "full" {
			isSupportedDoc = true
		}
	}

	var functions []ast.FunctionNode
	var docChunks []ast.ChunkNode
	var isDocParsed = false

	if isSupportedCode {
		var err error
		functions, err = ast.ParseCodeToDocs(ctx, path, content)
		if err != nil {
			log.Printf("AST parsing failed for %s: %v, falling back to simple chunking", path, err)
		}
	} else if isSupportedDoc {
		_, chunks, err := ast.ParseTextToChunks(path)
		if err == nil && len(chunks) > 0 {
			docChunks = chunks
			isDocParsed = true
		} else {
			log.Printf("Document parsing failed for %s: %v, falling back to simple chunking", path, err)
		}
	}

	if len(functions) > 0 {
		for idx, fn := range functions {
			// Chunk very large functions to prevent Ollama context failures
			chunks := []string{fn.Signature}
			if len(fn.Signature) > 8000 {
				chunks = iw.chunkText(fn.Signature, 8000)
				log.Printf("AST function %s in %s is very large (%d chars). Chunked into %d parts for embedding.", fn.Name, path, len(fn.Signature), len(chunks))
			}

			for chunkIdx, chunk := range chunks {
				// Compute embeddings of the function code body chunk
				vector, err := iw.FetchRemoteEmbedding(ctx, chunk)
				if err != nil {
					log.Printf("Ollama vectorization failed on AST function %s (chunk %d/%d) in %s: %v", fn.Name, chunkIdx+1, len(chunks), path, err)
					continue
				}

				// Ensure unique points across workspace updates
				deterministicSeed := fmt.Sprintf("%s_func_%s_%d", path, fn.Name, idx)
				if len(chunks) > 1 {
					deterministicSeed = fmt.Sprintf("%s_func_%s_%d_chunk_%d", path, fn.Name, idx, chunkIdx)
				}
				hash := sha1.Sum([]byte(deterministicSeed))
				id, _ := uuid.FromBytes(hash[:16])

				payload := map[string]interface{}{
					"file_path":     path,
					"content":       chunk,
					"type":          "function",
					"name":          fn.Name,
					"start_line":    int64(fn.StartLine),
					"end_line":      int64(fn.EndLine),
					"language":      fn.Language,
					"extension":     extClean,
					"relative_path": relPath,
					"relative_dirs": relDirs,
					"file_hash":     localHash,
					"updated":       time.Now().Unix(),
				}
				if fn.Receiver != "" {
					payload["receiver"] = fn.Receiver
				}

				sIndices, sValues := ComputeSparseVector(chunk, iw.CustomStopWords)

				points = append(points, &qdrant.PointStruct{
					Id:      qdrant.NewIDUUID(id.String()),
					Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
						"":       qdrant.NewVector(vector...),
						"sparse": qdrant.NewVectorSparse(sIndices, sValues),
					}),
					Payload: qdrant.NewValueMap(sanitizePayload(payload)),
				})
			}
		}
	} else if isDocParsed {
		for idx, chunk := range docChunks {
			vector, err := iw.FetchRemoteEmbedding(ctx, chunk.Content)
			if err != nil {
				log.Printf("Ollama vectorization failed on document chunk %d of %s: %v", idx, path, err)
				continue
			}

			// Ensure unique points across updates
			deterministicSeed := fmt.Sprintf("%s_doc_chunk_%d", path, idx)
			hash := sha1.Sum([]byte(deterministicSeed))
			id, _ := uuid.FromBytes(hash[:16])

			payload := map[string]interface{}{
				"file_path":     path,
				"content":       chunk.Content,
				"type":          "doc_chunk",
				"hash":          chunk.Hash,
				"extension":     extClean,
				"relative_path": relPath,
				"relative_dirs": relDirs,
				"file_hash":     localHash,
				"updated":       time.Now().Unix(),
			}
			if chunk.PageNumber > 0 {
				payload["page_number"] = int64(chunk.PageNumber)
			}

			sIndices, sValues := ComputeSparseVector(chunk.Content, iw.CustomStopWords)

			points = append(points, &qdrant.PointStruct{
				Id:      qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
					"":       qdrant.NewVector(vector...),
					"sparse": qdrant.NewVectorSparse(sIndices, sValues),
				}),
				Payload: qdrant.NewValueMap(sanitizePayload(payload)),
			})
		}
	} else {
		// Fall back to basic sliding window line chunking
		chunks := iw.chunkText(string(content), 1000)
		for idx, chunk := range chunks {
			vector, err := iw.FetchRemoteEmbedding(ctx, chunk)
			if err != nil {
				log.Printf("Ollama vectorization failed on remote endpoint: %v", err)
				continue
			}

			// Ensure unique points across workspace updates
			deterministicSeed := fmt.Sprintf("%s_chunk_%d", path, idx)
			hash := sha1.Sum([]byte(deterministicSeed))
			id, _ := uuid.FromBytes(hash[:16])

			payload := map[string]interface{}{
				"file_path":     path,
				"content":       chunk,
				"type":          "chunk",
				"extension":     extClean,
				"relative_path": relPath,
				"relative_dirs": relDirs,
				"file_hash":     localHash,
				"updated":       time.Now().Unix(),
			}

			sIndices, sValues := ComputeSparseVector(chunk, iw.CustomStopWords)

			points = append(points, &qdrant.PointStruct{
				Id:      qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
					"":       qdrant.NewVector(vector...),
					"sparse": qdrant.NewVectorSparse(sIndices, sValues),
				}),
				Payload: qdrant.NewValueMap(sanitizePayload(payload)),
			})
		}
	}

	if len(points) > 0 {
		for _, p := range points {
			iw.BatchUpserter.Add(p)
		}
		log.Printf("Successfully queued %d vectors for %s (AST parsed: %t)", len(points), path, len(functions) > 0)
	}
}

func (iw *IngestionWorker) purgeFileVectors(ctx context.Context, path string) error {
	_, err := iw.QdrantClient.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: iw.Cfg.CollectionName,
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
	exists, err := iw.QdrantClient.CollectionExists(ctx, iw.Cfg.CollectionName)
	if err != nil {
		return 0, fmt.Errorf("failed to check collection existence: %w", err)
	}

	if !exists {
		// Call Ollama to determine embedding dimension size dynamically
		dummyVector, err := iw.FetchRemoteEmbedding(ctx, "hello")
		if err != nil {
			return 0, fmt.Errorf("failed to fetch dummy embedding from Ollama model '%s' at %s: %w",
				iw.Cfg.EmbeddingModel, iw.Cfg.OllamaHost, err)
		}
		dimension := uint64(len(dummyVector))
		log.Printf("Collection '%s' does not exist. Creating it with dimension size %d...", iw.Cfg.CollectionName, dimension)

		err = iw.QdrantClient.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: iw.Cfg.CollectionName,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     dimension,
				Distance: qdrant.Distance_Cosine,
			}),
			SparseVectorsConfig: qdrant.NewSparseVectorsConfig(map[string]*qdrant.SparseVectorParams{
				"sparse": {},
			}),
		})
		if err != nil {
			return 0, fmt.Errorf("failed to create Qdrant collection '%s': %w", iw.Cfg.CollectionName, err)
		}
		log.Printf("Collection '%s' successfully created.", iw.Cfg.CollectionName)
	}

	// 2. Discover files
	var filesToIngest []string
	err = filepath.WalkDir(iw.Cfg.WatchDirectory, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		isDir := d.IsDir()
		if iw.ShouldIgnoreFile(path, isDir) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !isDir {
			filesToIngest = append(filesToIngest, path)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("error walking watch directory: %w", err)
	}

	log.Printf("Found %d files to ingest. Starting concurrent ingestion (max workers: %d)...", len(filesToIngest), iw.Cfg.MaxEmbeddingWorkers)
	var wg sync.WaitGroup
	for i, path := range filesToIngest {
		wg.Add(1)
		go func(idx int, p string) {
			defer wg.Done()
			log.Printf("[%d/%d] Ingesting %s...", idx+1, len(filesToIngest), p)
			iw.SyncFileState(ctx, p)
		}(i, path)
	}
	wg.Wait()

	log.Println("Flushing remaining vector batches to Qdrant...")
	iw.BatchUpserter.Flush()

	return len(filesToIngest), nil
}

func (iw *IngestionWorker) FetchRemoteEmbedding(ctx context.Context, text string) ([]float32, error) {
	var lastErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < 3; attempt++ {
		if err := iw.ConcurrencyController.Acquire(ctx); err != nil {
			return nil, err
		}

		startTime := time.Now()
		payload, _ := json.Marshal(OllamaEmbedReq{Model: iw.Cfg.EmbeddingModel, Prompt: text})
		req, _ := http.NewRequestWithContext(ctx, "POST", iw.Cfg.OllamaHost+"/api/embeddings", bytes.NewBuffer(payload))
		req.Header.Set("Content-Type", "application/json")

		resp, err := iw.HTTPClient.Do(req)
		duration := time.Since(startTime)

		if err != nil {
			iw.ConcurrencyController.RecordFailure("HTTP client error: " + err.Error())
			iw.ConcurrencyController.Release()
			lastErr = err

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}

		defer resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			iw.ConcurrencyController.RecordFailure(fmt.Sprintf("Ollama overloaded: HTTP %d", resp.StatusCode))
			iw.ConcurrencyController.Release()
			lastErr = fmt.Errorf("ollama overloaded: HTTP %d", resp.StatusCode)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}

		if resp.StatusCode != http.StatusOK {
			iw.ConcurrencyController.RecordFailure(fmt.Sprintf("HTTP error status: %d", resp.StatusCode))
			iw.ConcurrencyController.Release()
			lastErr = fmt.Errorf("unexpected status: %d", resp.StatusCode)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}

		var out OllamaEmbedResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			iw.ConcurrencyController.RecordFailure("JSON decode error")
			iw.ConcurrencyController.Release()
			lastErr = err

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			continue
		}

		// Success!
		iw.ConcurrencyController.RecordSuccess(duration)
		iw.ConcurrencyController.Release()
		return out.Embedding, nil
	}

	return nil, fmt.Errorf("failed to fetch embedding after 3 attempts: %w", lastErr)
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

func (iw *IngestionWorker) ExecuteVectorSearch(ctx context.Context, query string, fileExtensions []string, pathPrefix string) (string, error) {
	// Step A: Vectorize the search query using your home lab Ollama endpoint
	vector, err := iw.FetchRemoteEmbedding(ctx, query)
	if err != nil {
		return "", fmt.Errorf("failed to generate embedding for query: %w", err)
	}

	var filterConditions []*qdrant.Condition

	// 1. File Extensions Filter (OR match across extensions)
	if len(fileExtensions) > 0 {
		var shouldMatch []*qdrant.Condition
		for _, ext := range fileExtensions {
			cleanExt := strings.TrimPrefix(strings.ToLower(ext), ".")
			shouldMatch = append(shouldMatch, qdrant.NewMatchKeyword("extension", cleanExt))
		}
		filterConditions = append(filterConditions, qdrant.NewFilterAsCondition(&qdrant.Filter{
			Should: shouldMatch,
		}))
	}

	// 2. Path Prefix Filter (matches relative path or any parent dir)
	if pathPrefix != "" {
		cleanPrefix := filepath.ToSlash(strings.TrimPrefix(pathPrefix, "/"))
		filterConditions = append(filterConditions, qdrant.NewFilterAsCondition(&qdrant.Filter{
			Should: []*qdrant.Condition{
				qdrant.NewMatchKeyword("relative_path", cleanPrefix),
				qdrant.NewMatchKeyword("relative_dirs", cleanPrefix),
			},
		}))
	}

	var qdrantFilter *qdrant.Filter
	if len(filterConditions) > 0 {
		qdrantFilter = &qdrant.Filter{
			Must: filterConditions,
		}
	}

	// Step B: Direct a high-speed gRPC Query request to your Qdrant collection
	// Retrieve top 5 closest matching context code sheets
	var queryResponse []*qdrant.ScoredPoint
	var queryResponseErr error

	searchMode := strings.ToLower(strings.TrimSpace(iw.Cfg.SearchMode))
	if searchMode == "" {
		searchMode = "dense"
	}

	switch searchMode {
	case "sparse":
		sIndices, sValues := ComputeSparseVector(query, iw.CustomStopWords)
		queryResponse, queryResponseErr = iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Query:          qdrant.NewQuerySparse(sIndices, sValues),
			Limit:          qdrant.PtrOf(uint64(5)),
			Filter:         qdrantFilter,
			WithPayload:    qdrant.NewWithPayloadEnable(true),
			Using:          qdrant.PtrOf("sparse"),
		})
	case "hybrid":
		sIndices, sValues := ComputeSparseVector(query, iw.CustomStopWords)
		queryResponse, queryResponseErr = iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Prefetch: []*qdrant.PrefetchQuery{
				{
					Query:  qdrant.NewQueryDense(vector),
					Limit:  qdrant.PtrOf(uint64(20)),
					Filter: qdrantFilter,
				},
				{
					Query:  qdrant.NewQuerySparse(sIndices, sValues),
					Using:  qdrant.PtrOf("sparse"),
					Limit:  qdrant.PtrOf(uint64(20)),
					Filter: qdrantFilter,
				},
			},
			Query:       qdrant.NewQueryFusion(qdrant.Fusion_RRF),
			Limit:       qdrant.PtrOf(uint64(5)),
			WithPayload: qdrant.NewWithPayloadEnable(true),
		})
	default: // "dense" or fallback
		queryResponse, queryResponseErr = iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Query:          qdrant.NewQueryDense(vector),
			Limit:          qdrant.PtrOf(uint64(5)),
			Filter:         qdrantFilter,
			WithPayload:    qdrant.NewWithPayloadEnable(true),
		})
	}

	if queryResponseErr != nil {
		return "", fmt.Errorf("qdrant search operation failed: %w", queryResponseErr)
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

		// Detect if point is an AST function node
		var typeStr string
		if typeVal, exists := payloadMap["type"]; exists {
			typeStr = typeVal.GetStringValue()
		}

		// Detect target language parsing extensions for beautiful Markdown injection blocks
		lang := detectLanguage(filePath)

		var lastSyncedStr string
		if uVal, exists := payloadMap["updated"]; exists {
			lastSyncedStr = time.Unix(uVal.GetIntegerValue(), 0).Format("2006-01-02 15:04:05")
		} else {
			lastSyncedStr = "Unknown"
		}

		if typeStr == "function" {
			nameVal := ""
			if nVal, exists := payloadMap["name"]; exists {
				nameVal = nVal.GetStringValue()
			}
			startLine := int64(0)
			if sVal, exists := payloadMap["start_line"]; exists {
				startLine = sVal.GetIntegerValue()
			}
			endLine := int64(0)
			if eVal, exists := payloadMap["end_line"]; exists {
				endLine = eVal.GetIntegerValue()
			}
			receiverVal := ""
			if rVal, exists := payloadMap["receiver"]; exists {
				receiverVal = rVal.GetStringValue()
			}

			if receiverVal != "" {
				sb.WriteString(fmt.Sprintf("#### [%d] Function: `(%s).%s` in %s (Lines %d-%d) (Match Score: %.2f | Last Synced: %s)\n", i+1, receiverVal, nameVal, filePath, startLine, endLine, point.Score, lastSyncedStr))
			} else {
				sb.WriteString(fmt.Sprintf("#### [%d] Function: `%s` in %s (Lines %d-%d) (Match Score: %.2f | Last Synced: %s)\n", i+1, nameVal, filePath, startLine, endLine, point.Score, lastSyncedStr))
			}
		} else if typeStr == "doc_chunk" {
			pageVal := int64(0)
			if pVal, exists := payloadMap["page_number"]; exists {
				pageVal = pVal.GetIntegerValue()
			}
			if pageVal > 0 {
				sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk (Page/Section %d) in %s (Match Score: %.2f | Last Synced: %s)\n", i+1, pageVal, filePath, point.Score, lastSyncedStr))
			} else {
				sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk in %s (Match Score: %.2f | Last Synced: %s)\n", i+1, filePath, point.Score, lastSyncedStr))
			}
		} else {
			sb.WriteString(fmt.Sprintf("#### [%d] File Chunk: %s (Match Score: %.2f | Last Synced: %s)\n", i+1, filePath, point.Score, lastSyncedStr))
		}

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

func getParentDirs(relPath string) []string {
	var dirs []string
	dir := filepath.Dir(relPath)
	for dir != "." && dir != "/" && dir != "" {
		dirs = append(dirs, filepath.ToSlash(dir))
		dir = filepath.Dir(dir)
	}
	return dirs
}

func convertStringSlice(slice []string) []interface{} {
	res := make([]interface{}, len(slice))
	for i, v := range slice {
		res[i] = v
	}
	return res
}

func isBinaryContent(content []byte) bool {
	limit := 1024
	if len(content) < limit {
		limit = len(content)
	}
	for i := 0; i < limit; i++ {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

func sanitizePayload(payload map[string]interface{}) map[string]interface{} {
	for k, v := range payload {
		switch val := v.(type) {
		case string:
			payload[k] = strings.ToValidUTF8(val, "")
		case []interface{}:
			for i, elem := range val {
				if s, ok := elem.(string); ok {
					val[i] = strings.ToValidUTF8(s, "")
				}
			}
		}
	}
	return payload
}

var tokenRegex = regexp.MustCompile(`[a-zA-Z0-9_]+`)

var stopWords = map[string]struct{}{
	// === English ===
	"a": {}, "about": {}, "above": {}, "after": {}, "again": {}, "against": {}, "all": {}, "am": {}, "an": {}, "and": {}, "any": {}, "are": {}, "arent": {}, "as": {}, "at": {},
	"be": {}, "because": {}, "been": {}, "before": {}, "being": {}, "below": {}, "between": {}, "both": {}, "but": {}, "by": {},
	"cant": {}, "cannot": {}, "could": {}, "couldnt": {},
	"did": {}, "didnt": {}, "do": {}, "does": {}, "doesnt": {}, "doing": {}, "dont": {}, "down": {}, "during": {},
	"each": {},
	"few": {}, "for": {}, "from": {}, "further": {},
	"had": {}, "hadnt": {}, "has": {}, "hasnt": {}, "have": {}, "havent": {}, "having": {}, "he": {}, "hed": {}, "hell": {}, "hes": {}, "her": {}, "here": {}, "heres": {}, "hers": {}, "herself": {}, "him": {}, "himself": {}, "his": {}, "how": {}, "hows": {},
	"i": {}, "id": {}, "ill": {}, "im": {}, "ive": {}, "if": {}, "in": {}, "into": {}, "is": {}, "isnt": {}, "it": {}, "its": {}, "itself": {},
	"lets": {},
	"me": {}, "more": {}, "most": {}, "mustnt": {}, "my": {}, "myself": {},
	"no": {}, "nor": {}, "not": {},
	"of": {}, "off": {}, "on": {}, "once": {}, "only": {}, "or": {}, "other": {}, "ought": {}, "our": {}, "ours": {}, "ourselves": {}, "out": {}, "over": {}, "own": {},
	"same": {}, "shant": {}, "she": {}, "shed": {}, "shell": {}, "shes": {}, "should": {}, "shouldnt": {}, "so": {}, "some": {}, "such": {},
	"than": {}, "that": {}, "thats": {}, "the": {}, "their": {}, "theirs": {}, "them": {}, "themselves": {}, "then": {}, "there": {}, "theres": {}, "these": {}, "they": {}, "theyd": {}, "theyll": {}, "theyre": {}, "theyve": {}, "this": {}, "those": {}, "through": {}, "to": {}, "too": {}, "under": {}, "until": {}, "up": {}, "very": {},
	"was": {}, "wasnt": {}, "we": {}, "wed": {}, "well": {}, "were": {}, "weve": {}, "werent": {}, "what": {}, "whats": {}, "when": {}, "whens": {}, "where": {}, "wheres": {}, "which": {}, "while": {}, "who": {}, "whos": {}, "whom": {}, "why": {}, "whys": {}, "with": {}, "wont": {}, "would": {}, "wouldnt": {},
	"you": {}, "youd": {}, "youll": {}, "youre": {}, "youve": {}, "your": {}, "yours": {}, "yourself": {}, "yourselves": {},

	// === Spanish ===
	"el": {}, "la": {}, "los": {}, "las": {}, "un": {}, "una": {}, "unos": {}, "unas": {}, "de": {}, "del": {}, "al": {}, "en": {}, "con": {}, "por": {}, "para": {}, "y": {}, "pero": {}, "mas": {}, "que": {}, "se": {}, "lo": {}, "le": {}, "les": {}, "te": {}, "nos": {}, "os": {}, "su": {}, "sus": {}, "mis": {}, "tus": {}, "este": {}, "esta": {}, "esto": {}, "estos": {}, "estas": {}, "ese": {}, "esa": {}, "eso": {}, "esos": {}, "esas": {}, "aquel": {}, "aquella": {}, "aquello": {}, "aquellos": {}, "aquellas": {}, "yo": {}, "ella": {}, "nosotros": {}, "nosotras": {}, "vosotros": {}, "vosotras": {}, "ellos": {}, "ellas": {}, "ti": {}, "si": {}, "como": {}, "sin": {}, "sobre": {}, "tras": {}, "durante": {}, "mediante": {},

	// === Portuguese ===
	"uma": {}, "da": {}, "dos": {}, "das": {}, "ao": {}, "aos": {}, "na": {}, "nas": {}, "ou": {}, "mais": {}, "lhe": {}, "lhes": {}, "seu": {}, "sua": {}, "seus": {}, "suas": {}, "meu": {}, "minha": {}, "meus": {}, "minhas": {}, "teu": {}, "tua": {}, "teus": {}, "tuas": {}, "isto": {}, "isso": {}, "aquilo": {}, "eu": {}, "ele": {}, "elea": {}, "eles": {}, "elas": {}, "mim": {}, "sem": {}, "sob": {}, "com": {},

	// === German ===
	"der": {}, "die": {}, "des": {}, "dem": {}, "den": {}, "ein": {}, "eine": {}, "einer": {}, "eines": {}, "einem": {}, "einen": {}, "und": {}, "oder": {}, "aber": {}, "denn": {}, "doch": {}, "mit": {}, "von": {}, "zu": {}, "auf": {}, "aus": {}, "bei": {}, "fur": {}, "gegen": {}, "ohne": {}, "um": {}, "nach": {}, "uber": {}, "unter": {}, "vor": {}, "zwischen": {}, "ich": {}, "er": {}, "sie": {}, "es": {}, "wir": {}, "ihr": {}, "mein": {}, "dein": {}, "sein": {}, "unser": {}, "euer": {}, "dieses": {}, "diese": {}, "dieser": {}, "jener": {}, "jene": {}, "jenes": {}, "mir": {}, "dir": {}, "ihm": {}, "euch": {}, "ihnen": {}, "sich": {}, "wie": {}, "als": {}, "weil": {}, "dass": {}, "wenn": {},

	// === French ===
	"une": {}, "du": {}, "au": {}, "aux": {}, "dans": {}, "par": {}, "pour": {}, "avec": {}, "car": {}, "donc": {}, "ni": {}, "lui": {}, "leur": {}, "son": {}, "sa": {}, "ses": {}, "mon": {}, "ma": {}, "mes": {}, "ton": {}, "ta": {}, "tes": {}, "ce": {}, "cette": {}, "ces": {}, "ceci": {}, "cela": {}, "ca": {}, "celui": {}, "celle": {}, "ceux": {}, "celles": {}, "je": {}, "il": {}, "ils": {}, "elles": {}, "moi": {}, "toi": {}, "soi": {}, "plus": {}, "sans": {}, "sous": {}, "sur": {}, "chez": {}, "pendant": {},

	// === Italian ===
	"gli": {}, "dello": {}, "della": {}, "dei": {}, "degli": {}, "delle": {}, "allo": {}, "alla": {}, "ai": {}, "agli": {}, "alle": {}, "nel": {}, "nello": {}, "nella": {}, "nei": {}, "negli": {}, "nelle": {}, "col": {}, "dallo": {}, "dalla": {}, "dai": {}, "dagli": {}, "dalle": {}, "perche": {}, "li": {}, "ci": {}, "vi": {}, "suoi": {}, "sue": {}, "mio": {}, "mia": {}, "miei": {}, "mie": {}, "tuo": {}, "tuoi": {}, "tue": {}, "questo": {}, "questa": {}, "questi": {}, "queste": {}, "quello": {}, "quella": {}, "quelli": {}, "quelle": {}, "egli": {}, "essi": {}, "esse": {}, "fra": {}, "sopra": {},
}

func ComputeSparseVector(text string, customStopWords map[string]struct{}) ([]uint32, []float32) {
	text = strings.ToLower(text)
	matches := tokenRegex.FindAllString(text, -1)

	tfMap := make(map[string]int)
	for _, match := range matches {
		if _, isStop := stopWords[match]; isStop {
			continue
		}
		if customStopWords != nil {
			if _, isCustom := customStopWords[match]; isCustom {
				continue
			}
		}
		if len(match) == 0 {
			continue
		}
		tfMap[match]++
	}

	if len(tfMap) == 0 {
		return []uint32{}, []float32{}
	}

	type hashWeight struct {
		index  uint32
		weight float32
	}

	hwMap := make(map[uint32]float32)
	for token, tf := range tfMap {
		h := fnv.New32a()
		h.Write([]byte(token))
		idx := h.Sum32()

		weight := float32(tf) * float32(math.Log(1+float64(len(token))))
		hwMap[idx] += weight
	}

	list := make([]hashWeight, 0, len(hwMap))
	for idx, val := range hwMap {
		list = append(list, hashWeight{index: idx, weight: val})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].index < list[j].index
	})

	indices := make([]uint32, len(list))
	values := make([]float32, len(list))
	for i, item := range list {
		indices[i] = item.index
		values[i] = item.weight
	}

	return indices, values
}
