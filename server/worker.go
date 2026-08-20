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
	"sync/atomic"
	"time"
	"unicode"

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
	SetPayload(ctx context.Context, in *qdrant.SetPayloadPoints) (*qdrant.UpdateResult, error)
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

			upsertCtx, upsertCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := b.qdrantClient.Upsert(upsertCtx, &qdrant.UpsertPoints{
				CollectionName: b.collectionName,
				Points:         batch,
			})
			upsertCancel()
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
	case <-b.done:
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
	Cfg                   Config
	QdrantClient          QdrantClient
	HTTPClient            *http.Client
	Mu                    sync.Mutex
	PendingFiles          map[string]time.Time
	ActiveSyncs           int
	TotalSynced           int
	Sem                   chan struct{} // semaphore to rate limit concurrent embedding workers
	GitignoreMatcher      *GitIgnoreMatcher
	BatchUpserter         *BatchUpserter
	ConcurrencyController *ConcurrencyController
	CustomStopWords       map[string]struct{}
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
		HTTPClient:       &http.Client{Timeout: 120 * time.Second},
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
			strings.HasSuffix(baseName, ".tmp") ||
			strings.Contains(baseName, ".tmp.") {
			return true
		}
	}

	return false
}

type OllamaEmbedReq struct {
	Model   string                 `json:"model"`
	Prompt  string                 `json:"prompt"`
	Options map[string]interface{} `json:"options,omitempty"`
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

	relPath, _ := filepath.Rel(iw.Cfg.WatchDirectory, path)
	relPath = filepath.ToSlash(relPath)

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		log.Printf("File removed on dev machine, purging remote vectors: %s", path)
		_ = iw.purgeFileVectors(ctx, relPath)
		ast.EvictTree(path)
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

	// Parseable document formats (PDF/Office) are binary on disk but handled by
	// dedicated parsers, so the binary-content guard must not drop them.
	if !isParseableDocExt(strings.ToLower(filepath.Ext(path))) && isBinaryContent(content) {
		log.Printf("Skipping binary file: %s", path)
		return
	}

	if len(content) == 0 {
		return
	}

	localHash := fmt.Sprintf("%x", sha256.Sum256(content))
	localFingerprint := ast.IngestFingerprint(content, path, iw.Cfg.ParserMode, iw.Cfg.EmbeddingModel)

	// Scroll Qdrant to check if the ingest fingerprint matches (content + parser strategy).
	scrollResult, err := iw.QdrantClient.Scroll(ctx, &qdrant.ScrollPoints{
		CollectionName: iw.Cfg.CollectionName,
		Filter: &qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchKeyword("relative_path", relPath),
				qdrant.NewMatchKeyword("branch", iw.Cfg.Branch),
			},
		},
		Limit:       qdrant.PtrOf(uint32(1)),
		WithPayload: qdrant.NewWithPayloadEnable(true),
	})
	if err == nil && len(scrollResult) > 0 {
		if storedFP, exists := scrollResult[0].Payload["ingest_fingerprint"]; exists {
			if storedFP.GetStringValue() == localFingerprint {
				log.Printf("Ingest fingerprint matches for %s, skipping re-indexing.", path)
				return
			}
		}
		// Legacy points without ingest_fingerprint are reindexed once.
	}

	// Purge historical offsets right before re-indexing to ensure stale lines wipe out
	_ = iw.purgeFileVectors(ctx, relPath)

	ext := strings.ToLower(filepath.Ext(path))
	extClean := strings.TrimPrefix(ext, ".")
	relDirs := convertStringSlice(getParentDirs(relPath))
	modifiedUnix := info.ModTime().Unix()
	var points []*qdrant.PointStruct

	// Determine file categories based on ParserMode
	isSupportedCode := false
	isSupportedDoc := false
	switch ext {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".php", ".cs", ".py", ".rs",
		".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		if iw.Cfg.ParserMode == "code" || iw.Cfg.ParserMode == "full" {
			isSupportedCode = true
		}
	case ".pdf", ".md", ".markdown", ".txt", ".csv", ".xls", ".xlsx", ".doc", ".docx", ".feature":
		if iw.Cfg.ParserMode == "doc" || iw.Cfg.ParserMode == "full" {
			isSupportedDoc = true
		}
	}

	var functions []ast.FunctionNode
	var imports []ast.ImportRef
	var docChunks []ast.ChunkNode
	var isDocParsed = false
	var namespace string
	var typeNames []string

	if isSupportedCode {
		parseResult, err := ast.ParseCodeToDocsWithMeta(ctx, path, content)
		if err != nil {
			log.Printf("AST parsing failed for %s: %v, falling back to simple chunking", path, err)
		} else {
			functions = parseResult.Functions
			imports = parseResult.Imports
			namespace = parseResult.Namespace
			typeNames = parseResult.Types
		}
	} else if isSupportedDoc {
		_, chunks, err := ast.ParseTextToChunksFromContent(path, content)
		if err == nil && len(chunks) > 0 {
			docChunks = chunks
			isDocParsed = true
		} else {
			log.Printf("Document parsing failed for %s: %v, falling back to simple chunking", path, err)
		}
	}

	fileTags := buildFileTags(path, relPath, extClean)
	frameworkTags := inferImportTags(detectLanguage(path), imports)
	layerTags := collectLayerTags(relPath, namespace, typeNames)
	symbolNames := buildFileSymbolNames(path, relPath, namespace, typeNames)
	isTestFile := hasTag(fileTags, "test") || hasTag(layerTags, "test")
	testFramework := detectTestFramework(frameworkTags)

	if len(functions) > 0 {
		for idx, fn := range functions {
			// Chunk very large functions to prevent Ollama context failures
			chunks := []string{fn.Signature}
			if len(fn.Signature) > 4000 {
				chunks = iw.chunkText(fn.Signature, 4000)
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
				deterministicSeed := fmt.Sprintf("%s_func_%s_%d", relPath, fn.Name, idx)
				if len(chunks) > 1 {
					deterministicSeed = fmt.Sprintf("%s_func_%s_%d_chunk_%d", relPath, fn.Name, idx, chunkIdx)
				}
				hash := sha1.Sum([]byte(deterministicSeed))
				id, _ := uuid.FromBytes(hash[:16])

				pointTags := mergeTags(fileTags, frameworkTags, layerTags, buildFunctionTags(fn, imports))
				pointSymbols := mergeTags(symbolNames, buildFunctionSymbolNames(fn))
				payload := map[string]interface{}{
					"file_path":          relPath,
					"content":            chunk,
					"type":               "function",
					"name":               fn.Name,
					"start_line":         int64(fn.StartLine),
					"end_line":           int64(fn.EndLine),
					"language":           fn.Language,
					"extension":          extClean,
					"relative_path":      relPath,
					"relative_dirs":      relDirs,
					"namespace":          firstNonEmpty(fn.Namespace, namespace),
					"container":          fn.Container,
					"symbol_names":       convertStringSlice(pointSymbols),
					"framework_tags":     convertStringSlice(frameworkTags),
					"layer_tags":         convertStringSlice(layerTags),
					"tags":               convertStringSlice(pointTags),
					"is_test":            isTestFile,
					"test_framework":     testFramework,
					"file_hash":          localHash,
					"ingest_fingerprint": localFingerprint,
					"modified":           modifiedUnix,
					"updated":            time.Now().Unix(),
					"branch":             iw.Cfg.Branch,
					"default_branch":     iw.Cfg.DefaultBranch,
				}
				if fn.Receiver != "" {
					payload["receiver"] = fn.Receiver
				}
				payload["source_kind"] = "code"

				searchText := buildSearchText(chunk, pointTags, pointSymbols, firstNonEmpty(fn.Namespace, namespace), fn.Container, relPath)
				sIndices, sValues := ComputeSparseVector(searchText, iw.CustomStopWords)

				points = append(points, &qdrant.PointStruct{
					Id: qdrant.NewIDUUID(id.String()),
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
			if !ast.IsCleanText(chunk.Content) {
				continue
			}
			vector, err := iw.FetchRemoteEmbedding(ctx, chunk.Content)
			if err != nil {
				log.Printf("Ollama vectorization failed on document chunk %d of %s: %v", idx, path, err)
				continue
			}

			// Ensure unique points across updates
			deterministicSeed := fmt.Sprintf("%s_doc_chunk_%d", relPath, idx)
			hash := sha1.Sum([]byte(deterministicSeed))
			id, _ := uuid.FromBytes(hash[:16])

			pointTags := mergeTags(fileTags, layerTags, []string{"document"})
			sourceKind := chunk.SourceKind
			if sourceKind == "" {
				sourceKind = "document"
			}
			switch sourceKind {
			case "markdown":
				pointTags = mergeTags(pointTags, []string{"markdown"}, chunk.MetaTags)
			case "gherkin":
				pointTags = mergeTags(pointTags, []string{"gherkin"}, chunk.FeatureTags, chunk.ScenarioTags)
			}
			pointSymbols := mergeTags(symbolNames)
			if chunk.Heading != "" {
				pointSymbols = mergeTags(pointSymbols, []string{chunk.Heading})
			}
			if chunk.Scenario != "" {
				pointSymbols = mergeTags(pointSymbols, []string{chunk.Scenario})
			}
			if chunk.Feature != "" {
				pointSymbols = mergeTags(pointSymbols, []string{chunk.Feature})
			}

			payload := map[string]interface{}{
				"file_path":          relPath,
				"content":            chunk.Content,
				"type":               "doc_chunk",
				"hash":               chunk.Hash,
				"extension":          extClean,
				"relative_path":      relPath,
				"relative_dirs":      relDirs,
				"namespace":          namespace,
				"symbol_names":       convertStringSlice(pointSymbols),
				"framework_tags":     convertStringSlice(frameworkTags),
				"layer_tags":         convertStringSlice(layerTags),
				"tags":               convertStringSlice(pointTags),
				"is_test":            isTestFile,
				"test_framework":     testFramework,
				"file_hash":          localHash,
				"ingest_fingerprint": localFingerprint,
				"modified":           modifiedUnix,
				"updated":            time.Now().Unix(),
				"branch":             iw.Cfg.Branch,
				"default_branch":     iw.Cfg.DefaultBranch,
				"source_kind":        sourceKind,
			}
			// Always persist page_number for consistency; 0 means the format has
			// no physical pagination (docx/xlsx).
			payload["page_number"] = int64(chunk.PageNumber)
			if chunk.Heading != "" {
				payload["heading"] = chunk.Heading
			}
			if len(chunk.HeadingPath) > 0 {
				payload["heading_path"] = convertStringSlice(chunk.HeadingPath)
			}
			if chunk.HeadingLevel > 0 {
				payload["heading_level"] = int64(chunk.HeadingLevel)
			}
			if chunk.Title != "" {
				payload["doc_title"] = chunk.Title
			}
			if chunk.Feature != "" {
				payload["feature"] = chunk.Feature
			}
			if chunk.Rule != "" {
				payload["rule"] = chunk.Rule
			}
			if chunk.Scenario != "" {
				payload["scenario"] = chunk.Scenario
			}
			if len(chunk.FeatureTags) > 0 {
				payload["feature_tags"] = convertStringSlice(chunk.FeatureTags)
			}
			if len(chunk.ScenarioTags) > 0 {
				payload["scenario_tags"] = convertStringSlice(chunk.ScenarioTags)
			}
			if chunk.StartLine > 0 {
				payload["start_line"] = int64(chunk.StartLine)
			}
			if chunk.EndLine > 0 {
				payload["end_line"] = int64(chunk.EndLine)
			}

			searchText := buildSearchText(chunk.Content, pointTags, pointSymbols, namespace, chunk.Feature, relPath)
			sIndices, sValues := ComputeSparseVector(searchText, iw.CustomStopWords)

			points = append(points, &qdrant.PointStruct{
				Id: qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectorsMap(map[string]*qdrant.Vector{
					"":       qdrant.NewVector(vector...),
					"sparse": qdrant.NewVectorSparse(sIndices, sValues),
				}),
				Payload: qdrant.NewValueMap(sanitizePayload(payload)),
			})
		}
	} else {
		// Fall back to basic sliding window line chunking. Never raw-chunk binary
		// or parseable-document formats: a failed parse must not dump file bytes
		// (PDF streams, zip data) into embeddings as fake "text".
		if isParseableDocExt(ext) || isBinaryContent(content) {
			log.Printf("Skipping raw-byte fallback for %s: not plain text (parse failed or binary)", path)
			return
		}
		chunks := iw.chunkText(string(content), 1000)
		for idx, chunk := range chunks {
			if !ast.IsCleanText(chunk) {
				continue
			}
			vector, err := iw.FetchRemoteEmbedding(ctx, chunk)
			if err != nil {
				log.Printf("Ollama vectorization failed on remote endpoint: %v", err)
				continue
			}

			// Ensure unique points across workspace updates
			deterministicSeed := fmt.Sprintf("%s_chunk_%d", relPath, idx)
			hash := sha1.Sum([]byte(deterministicSeed))
			id, _ := uuid.FromBytes(hash[:16])

			pointTags := mergeTags(fileTags, frameworkTags, layerTags, []string{"file_chunk"})
			payload := map[string]interface{}{
				"file_path":          relPath,
				"content":            chunk,
				"type":               "chunk",
				"extension":          extClean,
				"relative_path":      relPath,
				"relative_dirs":      relDirs,
				"namespace":          namespace,
				"symbol_names":       convertStringSlice(symbolNames),
				"framework_tags":     convertStringSlice(frameworkTags),
				"layer_tags":         convertStringSlice(layerTags),
				"tags":               convertStringSlice(pointTags),
				"is_test":            isTestFile,
				"test_framework":     testFramework,
				"file_hash":          localHash,
				"ingest_fingerprint": localFingerprint,
				"modified":           modifiedUnix,
				"updated":            time.Now().Unix(),
				"branch":             iw.Cfg.Branch,
				"default_branch":     iw.Cfg.DefaultBranch,
				"source_kind":        "generic",
			}

			searchText := buildSearchText(chunk, pointTags, symbolNames, namespace, "", relPath)
			sIndices, sValues := ComputeSparseVector(searchText, iw.CustomStopWords)

			points = append(points, &qdrant.PointStruct{
				Id: qdrant.NewIDUUID(id.String()),
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

func (iw *IngestionWorker) purgeFileVectors(ctx context.Context, relPath string) error {
	_, err := iw.QdrantClient.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: iw.Cfg.CollectionName,
		Points: qdrant.NewPointsSelectorFilter(&qdrant.Filter{
			Must: []*qdrant.Condition{
				qdrant.NewMatchKeyword("relative_path", relPath),
				qdrant.NewMatchKeyword("branch", iw.Cfg.Branch),
			},
		}),
	})
	return err
}

// migrateUnbrandedVectors tags legacy vectors (missing or empty branch field)
// with the current branch and default_branch. Called once at the start of SyncWorkspace.
func (iw *IngestionWorker) migrateUnbrandedVectors(ctx context.Context) error {
	filter := &qdrant.Filter{
		Should: []*qdrant.Condition{
			qdrant.NewIsEmpty("branch"),
			qdrant.NewMatchKeyword("branch", ""),
		},
	}
	_, err := iw.QdrantClient.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: iw.Cfg.CollectionName,
		Payload: map[string]*qdrant.Value{
			"branch":         qdrant.NewValueString(iw.Cfg.Branch),
			"default_branch": qdrant.NewValueString(iw.Cfg.DefaultBranch),
		},
		PointsSelector: qdrant.NewPointsSelectorFilter(filter),
	})
	if err != nil {
		return fmt.Errorf("migration of legacy vectors failed: %w", err)
	}
	log.Printf("Branch migration complete: tagged legacy vectors with branch=%q default_branch=%q",
		iw.Cfg.Branch, iw.Cfg.DefaultBranch)
	return nil
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

	if err := iw.migrateUnbrandedVectors(ctx); err != nil {
		log.Printf("Warning: branch migration failed (non-fatal): %v", err)
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

	workerCount := iw.Cfg.MaxEmbeddingWorkers
	if workerCount <= 0 {
		workerCount = 5
	}
	total := len(filesToIngest)
	log.Printf("Found %d files to ingest. Starting concurrent ingestion (max workers: %d)...", total, workerCount)

	type job struct {
		idx  int
		path string
	}
	jobs := make(chan job, total)
	for i, p := range filesToIngest {
		jobs <- job{i, p}
	}
	close(jobs)

	var (
		wg       sync.WaitGroup
		syncMu   sync.Mutex
		synced   int
		progress int64
	)

	var stopProgress chan struct{}
	if iw.Cfg.LogToFile && total > 0 {
		fmt.Fprintf(os.Stderr, "[qdrant-mcp] Ingesting %d files...\n", total)
		stopProgress = make(chan struct{})
		go func() {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					n := atomic.LoadInt64(&progress)
					fmt.Fprintf(os.Stderr, "\r[qdrant-mcp] %d/%d files", n, total)
				case <-stopProgress:
					return
				}
			}
		}()
	}

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[%d/%d] Ingesting %s...", j.idx+1, total, j.path)
				iw.SyncFileState(ctx, j.path)
				atomic.AddInt64(&progress, 1)
				syncMu.Lock()
				synced++
				syncMu.Unlock()
			}
		}()
	}
	wg.Wait()

	if stopProgress != nil {
		close(stopProgress)
		fmt.Fprintf(os.Stderr, "\r[qdrant-mcp] Done: %d/%d files ingested\n", atomic.LoadInt64(&progress), total)
	}

	iw.Mu.Lock()
	iw.TotalSynced += synced
	iw.Mu.Unlock()

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
		payload, _ := json.Marshal(OllamaEmbedReq{
			Model:  iw.Cfg.EmbeddingModel,
			Prompt: text,
			Options: map[string]interface{}{
				"num_ctx": iw.Cfg.OllamaNumCtx,
			},
		})
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

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusInternalServerError {
			iw.ConcurrencyController.RecordFailure(fmt.Sprintf("Ollama overloaded: HTTP %d", resp.StatusCode))
			iw.ConcurrencyController.Release()
			lastErr = fmt.Errorf("ollama overloaded: HTTP %d", resp.StatusCode)
			resp.Body.Close()

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
			resp.Body.Close()

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
			resp.Body.Close()
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
		resp.Body.Close()
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

func (iw *IngestionWorker) ExecuteVectorSearch(ctx context.Context, query string, fileExtensions []string, pathPrefix string, branch string) (string, error) {
	intent := detectQueryIntent(query, fileExtensions, pathPrefix)
	baseFilter := buildSearchFilter(fileExtensions, pathPrefix)
	candidateLimit := uint64(40)
	finalLimit := 5
	variantQueries := buildQueryVariants(intent, pathPrefix)

	var ranked []RankedSearchResult

	if branch != "" {
		// Pass 1: branch-specific results
		branchFilter := addBranchFilter(baseFilter, branch)
		branchResults := make(map[string][]*qdrant.ScoredPoint, len(variantQueries))
		for _, variant := range variantQueries {
			results, err := iw.queryVariant(ctx, variant, branchFilter, candidateLimit)
			if err != nil {
				return "", fmt.Errorf("branch search failed for variant %q: %w", variant, err)
			}
			branchResults[variant] = results
		}
		branchRanked := rerankSearchResults(branchResults, intent, pathPrefix)

		coveredPaths := make(map[string]struct{}, len(branchRanked))
		defaultBranch := iw.Cfg.DefaultBranch
		for _, r := range branchRanked {
			if rp := payloadString(r.Point.Payload, "relative_path", ""); rp != "" {
				coveredPaths[rp] = struct{}{}
			}
			if db := payloadString(r.Point.Payload, "default_branch", ""); db != "" {
				defaultBranch = db
			}
		}

		// Pass 2: default branch fallback for files not in branch results
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		defaultFilter := addBranchFilter(baseFilter, defaultBranch)
		fallbackResults := make(map[string][]*qdrant.ScoredPoint, len(variantQueries))
		for _, variant := range variantQueries {
			results, err := iw.queryVariant(ctx, variant, defaultFilter, candidateLimit)
			if err != nil {
				return "", fmt.Errorf("fallback search failed for variant %q: %w", variant, err)
			}
			fallbackResults[variant] = results
		}
		fallbackRanked := rerankSearchResults(fallbackResults, intent, pathPrefix)

		ranked = branchRanked
		for _, r := range fallbackRanked {
			if rp := payloadString(r.Point.Payload, "relative_path", ""); rp != "" {
				if _, covered := coveredPaths[rp]; !covered {
					ranked = append(ranked, r)
				}
			}
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			if ranked[i].Score == ranked[j].Score {
				return ranked[i].Point.Score > ranked[j].Point.Score
			}
			return ranked[i].Score > ranked[j].Score
		})
	} else {
		variantResults := make(map[string][]*qdrant.ScoredPoint, len(variantQueries))
		for _, variant := range variantQueries {
			results, err := iw.queryVariant(ctx, variant, baseFilter, candidateLimit)
			if err != nil {
				return "", fmt.Errorf("qdrant search operation failed for variant %q: %w", variant, err)
			}
			variantResults[variant] = results
		}
		ranked = rerankSearchResults(variantResults, intent, pathPrefix)
	}

	if len(ranked) == 0 {
		return "No relevant structural code blocks or reference components were found matching your query scope.", nil
	}
	if len(ranked) > finalLimit {
		ranked = ranked[:finalLimit]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Core Codebase Reference Snippets for: \"%s\"\n\n", query))
	sb.WriteString(fmt.Sprintf("Search intent: tags=`%s`", strings.Join(intent.Tags, "`, `")))
	if len(intent.FrameworkTags) > 0 {
		sb.WriteString(fmt.Sprintf(" | frameworks=`%s`", strings.Join(intent.FrameworkTags, "`, `")))
	}
	if len(intent.LayerTags) > 0 {
		sb.WriteString(fmt.Sprintf(" | layers=`%s`", strings.Join(intent.LayerTags, "`, `")))
	}
	if branch != "" {
		sb.WriteString(fmt.Sprintf(" | branch=`%s`", branch))
	}
	sb.WriteString(fmt.Sprintf(" | variants=%d\n\n", len(variantQueries)))

	for i, rankedPoint := range ranked {
		point := rankedPoint.Point
		payloadMap := point.Payload
		filePath := payloadString(payloadMap, "file_path", "Unknown Location")
		contentChunk := payloadString(payloadMap, "content", "Empty Source")
		tagList := payloadStringList(payloadMap, "tags")
		typeStr := payloadString(payloadMap, "type", "")
		lang := detectLanguage(filePath)
		resultBranch := payloadString(payloadMap, "branch", "")
		lastSyncedStr := "Unknown"
		if uVal, exists := payloadMap["updated"]; exists {
			lastSyncedStr = time.Unix(uVal.GetIntegerValue(), 0).Format("2006-01-02 15:04:05")
		}

		switch typeStr {
		case "function":
			nameVal := payloadString(payloadMap, "name", "")
			startLine := payloadInt(payloadMap, "start_line")
			endLine := payloadInt(payloadMap, "end_line")
			receiverVal := payloadString(payloadMap, "receiver", "")
			containerVal := payloadString(payloadMap, "container", "")
			if receiverVal != "" {
				sb.WriteString(fmt.Sprintf("#### [%d] Function: `(%s).%s` in %s (Lines %d-%d) [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, receiverVal, nameVal, filePath, startLine, endLine, resultBranch, rankedPoint.Score, lastSyncedStr))
			} else if containerVal != "" {
				sb.WriteString(fmt.Sprintf("#### [%d] Method: `%s.%s` in %s (Lines %d-%d) [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, containerVal, nameVal, filePath, startLine, endLine, resultBranch, rankedPoint.Score, lastSyncedStr))
			} else {
				sb.WriteString(fmt.Sprintf("#### [%d] Function: `%s` in %s (Lines %d-%d) [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, nameVal, filePath, startLine, endLine, resultBranch, rankedPoint.Score, lastSyncedStr))
			}
		case "doc_chunk":
			sourceKind := payloadString(payloadMap, "source_kind", "")
			switch sourceKind {
			case "gherkin":
				featureVal := payloadString(payloadMap, "feature", "")
				scenarioVal := payloadString(payloadMap, "scenario", "")
				ruleVal := payloadString(payloadMap, "rule", "")
				startLine := payloadInt(payloadMap, "start_line")
				endLine := payloadInt(payloadMap, "end_line")
				label := "Scenario"
				if scenarioVal != "" {
					label = fmt.Sprintf("Scenario: `%s`", scenarioVal)
				}
				if ruleVal != "" {
					label = fmt.Sprintf("Rule `%s` / %s", ruleVal, label)
				}
				if featureVal != "" {
					label = fmt.Sprintf("Feature `%s` / %s", featureVal, label)
				}
				if startLine > 0 && endLine > 0 {
					sb.WriteString(fmt.Sprintf("#### [%d] %s in %s (Lines %d-%d) [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, label, filePath, startLine, endLine, resultBranch, rankedPoint.Score, lastSyncedStr))
				} else {
					sb.WriteString(fmt.Sprintf("#### [%d] %s in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, label, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
				}
			case "markdown":
				headingPath := payloadStringList(payloadMap, "heading_path")
				heading := payloadString(payloadMap, "heading", "")
				section := strings.Join(headingPath, " > ")
				if section == "" {
					section = heading
				}
				if section == "" {
					section = "Document preamble"
				}
				sb.WriteString(fmt.Sprintf("#### [%d] Markdown Section: `%s` in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, section, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
			default:
				pageVal := payloadInt(payloadMap, "page_number")
				if pageVal > 0 {
					sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk (Page/Section %d) in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, pageVal, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
				} else {
					sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
				}
			}
		default:
			sb.WriteString(fmt.Sprintf("#### [%d] File Chunk: %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
		}

		if namespace := payloadString(payloadMap, "namespace", ""); namespace != "" {
			sb.WriteString(fmt.Sprintf("Namespace: `%s`\n", namespace))
		}
		if len(tagList) > 0 {
			sb.WriteString(fmt.Sprintf("Tags: `%s`\n", strings.Join(tagList, "`, `")))
		}
		if len(rankedPoint.Reasons) > 0 {
			sb.WriteString(fmt.Sprintf("Signals: `%s`\n", strings.Join(rankedPoint.Reasons, "`, `")))
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

func (iw *IngestionWorker) queryVariant(ctx context.Context, query string, qdrantFilter *qdrant.Filter, candidateLimit uint64) ([]*qdrant.ScoredPoint, error) {
	searchMode := strings.ToLower(strings.TrimSpace(iw.Cfg.SearchMode))
	if searchMode == "" {
		searchMode = "dense"
	}

	vector, err := iw.FetchRemoteEmbedding(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding for query %q: %w", query, err)
	}

	switch searchMode {
	case "sparse":
		sIndices, sValues := ComputeSparseVector(query, iw.CustomStopWords)
		return iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Query:          qdrant.NewQuerySparse(sIndices, sValues),
			Limit:          qdrant.PtrOf(candidateLimit),
			Filter:         qdrantFilter,
			WithPayload:    qdrant.NewWithPayloadEnable(true),
			Using:          qdrant.PtrOf("sparse"),
		})
	case "hybrid":
		sIndices, sValues := ComputeSparseVector(query, iw.CustomStopWords)
		return iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Prefetch: []*qdrant.PrefetchQuery{
				{
					Query:  qdrant.NewQueryDense(vector),
					Limit:  qdrant.PtrOf(candidateLimit),
					Filter: qdrantFilter,
				},
				{
					Query:  qdrant.NewQuerySparse(sIndices, sValues),
					Using:  qdrant.PtrOf("sparse"),
					Limit:  qdrant.PtrOf(candidateLimit),
					Filter: qdrantFilter,
				},
			},
			Query:       qdrant.NewQueryFusion(qdrant.Fusion_RRF),
			Limit:       qdrant.PtrOf(candidateLimit),
			WithPayload: qdrant.NewWithPayloadEnable(true),
		})
	default:
		return iw.QdrantClient.Query(ctx, &qdrant.QueryPoints{
			CollectionName: iw.Cfg.CollectionName,
			Query:          qdrant.NewQueryDense(vector),
			Limit:          qdrant.PtrOf(candidateLimit),
			Filter:         qdrantFilter,
			WithPayload:    qdrant.NewWithPayloadEnable(true),
		})
	}
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
	case ".feature":
		return "gherkin"
	case "":
		return "text"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

type SearchIntent struct {
	Query         string
	Tags          []string
	Symbols       []string
	FrameworkTags []string
	LayerTags     []string
	PreferTests   bool
	PreferExact   bool
}

type RankedSearchResult struct {
	Point   *qdrant.ScoredPoint
	Score   float32
	Reasons []string
}

func buildSearchText(content string, tags, symbolNames []string, namespace, container, relPath string) string {
	var sections []string
	if len(tags) > 0 {
		sections = append(sections, strings.Join(tags, " "))
	}
	if len(symbolNames) > 0 {
		sections = append(sections, strings.Join(symbolNames, " "))
	}
	if namespace != "" {
		sections = append(sections, namespace)
	}
	if container != "" {
		sections = append(sections, container)
	}
	if relPath != "" {
		sections = append(sections, relPath)
	}
	sections = append(sections, content)
	return strings.Join(sections, "\n")
}

func buildFileTags(path, relPath, ext string) []string {
	var tags []string
	tags = append(tags, "file")
	if ext != "" {
		tags = append(tags, ext, detectLanguage(path))
	}

	normalizedRelPath := filepath.ToSlash(relPath)
	tags = append(tags, tokenizeForTags(normalizedRelPath)...)
	tags = append(tags, tokenizeForTags(filepath.Base(path))...)
	tags = append(tags, classifyRoleTags(normalizedRelPath)...)

	return uniqueSortedTags(tags)
}

func buildFunctionTags(fn ast.FunctionNode, imports []ast.ImportRef) []string {
	var tags []string
	tags = append(tags, "function", fn.Language)
	tags = append(tags, tokenizeForTags(fn.Name)...)
	tags = append(tags, tokenizeForTags(fn.Receiver)...)
	tags = append(tags, tokenizeForTags(fn.Container)...)
	tags = append(tags, tokenizeForTags(fn.Namespace)...)
	tags = append(tags, classifyRoleTags(fn.Name)...)
	tags = append(tags, classifyRoleTags(fn.Receiver)...)
	tags = append(tags, classifyRoleTags(fn.Container)...)
	tags = append(tags, classifyRoleTags(fn.Namespace)...)
	tags = append(tags, inferImportTags(fn.Language, imports)...)

	for _, imp := range imports {
		tags = append(tags, tokenizeForTags(imp.RawPath)...)
	}

	return uniqueSortedTags(tags)
}

func mergeTags(groups ...[]string) []string {
	var merged []string
	for _, group := range groups {
		merged = append(merged, group...)
	}
	return uniqueSortedTags(merged)
}

func tokenizeForTags(input string) []string {
	normalized := filepath.ToSlash(input)
	replacer := strings.NewReplacer(
		".", " ",
		"/", " ",
		"\\", " ",
		"-", " ",
		"_", " ",
		":", " ",
		"(", " ",
		")", " ",
		"[", " ",
		"]", " ",
		"{", " ",
		"}", " ",
		",", " ",
	)
	normalized = replacer.Replace(normalized)

	var expanded []string
	for _, part := range strings.Fields(normalized) {
		for _, token := range splitCamelCase(part) {
			token = strings.TrimSpace(strings.ToLower(token))
			if token == "" || len(token) < 2 {
				continue
			}
			if _, isStopWord := stopWords[token]; isStopWord {
				continue
			}
			expanded = append(expanded, token)
		}
	}
	return uniqueSortedTags(expanded)
}

func splitCamelCase(s string) []string {
	if s == "" {
		return nil
	}

	var tokens []string
	var current []rune
	for i, r := range []rune(s) {
		if i > 0 && unicode.IsUpper(r) && len(current) > 0 {
			prev := current[len(current)-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				tokens = append(tokens, string(current))
				current = current[:0]
			}
		}
		current = append(current, r)
	}
	if len(current) > 0 {
		tokens = append(tokens, string(current))
	}
	return tokens
}

func classifyRoleTags(input string) []string {
	tokens := tokenizeForTags(input)
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		tokenSet[t] = struct{}{}
	}
	roleMatchers := map[string][]string{
		"test":           {"test", "tests", "spec", "fixture", "mock"},
		"service":        {"service", "services"},
		"controller":     {"controller", "controllers"},
		"handler":        {"handler", "handlers", "endpoint"},
		"repository":     {"repository", "repositories", "repo"},
		"dto":            {"dto", "request", "response"},
		"entity":         {"entity", "entities", "model"},
		"domain":         {"domain"},
		"application":    {"application", "app"},
		"infrastructure": {"infrastructure", "infra"},
		"api":            {"api", "http"},
		"web":            {"web", "frontend", "ui"},
		"command":        {"command", "commands"},
		"query":          {"query", "queries"},
		"event":          {"event", "events"},
		"validator":      {"validator", "validation"},
		"config":         {"config", "configuration", "settings"},
		"migration":      {"migration", "migrations"},
	}

	var tags []string
	for tag, patterns := range roleMatchers {
		for _, pattern := range patterns {
			if _, ok := tokenSet[pattern]; ok {
				tags = append(tags, tag)
				break
			}
		}
	}
	return uniqueSortedTags(tags)
}

func inferImportTags(language string, imports []ast.ImportRef) []string {
	var tags []string
	for _, imp := range imports {
		raw := strings.ToLower(imp.RawPath)
		switch language {
		case "csharp":
			tags = append(tags, matchCanonicalTags(raw, map[string][]string{
				"xunit":            {"xunit"},
				"nunit":            {"nunit"},
				"mstest":           {"microsoft.visualstudio.testtools.unittesting"},
				"efcore":           {"microsoft.entityframeworkcore", "entityframeworkcore"},
				"mediatr":          {"mediatr"},
				"fluentvalidation": {"fluentvalidation"},
				"automapper":       {"automapper"},
				"aspnetcore":       {"microsoft.aspnetcore", "aspnetcore"},
				"serilog":          {"serilog"},
				"dapper":           {"dapper"},
				"hangfire":         {"hangfire"},
				"massTransit":      {"masstransit"},
			})...)
		case "go":
			tags = append(tags, matchCanonicalTags(raw, map[string][]string{
				"gin":      {"github.com/gin-gonic/gin"},
				"echo":     {"github.com/labstack/echo"},
				"fiber":    {"github.com/gofiber/fiber"},
				"chi":      {"github.com/go-chi/chi"},
				"grpc":     {"google.golang.org/grpc", "grpc"},
				"gorm":     {"gorm.io/gorm", "gorm.io"},
				"sqlx":     {"github.com/jmoiron/sqlx"},
				"cobra":    {"github.com/spf13/cobra"},
				"viper":    {"github.com/spf13/viper"},
				"testify":  {"github.com/stretchr/testify"},
				"gqlgen":   {"github.com/99designs/gqlgen"},
				"protobuf": {"google.golang.org/protobuf", "github.com/golang/protobuf"},
			})...)
		case "javascript", "typescript":
			tags = append(tags, matchCanonicalTags(raw, map[string][]string{
				"react":      {"react"},
				"nextjs":     {"next", "nextjs"},
				"express":    {"express"},
				"nestjs":     {"@nestjs"},
				"jest":       {"jest", "@jest"},
				"vitest":     {"vitest"},
				"playwright": {"playwright", "@playwright"},
				"cypress":    {"cypress"},
				"prisma":     {"prisma", "@prisma"},
				"mongoose":   {"mongoose"},
				"redux":      {"redux", "@reduxjs/toolkit"},
				"vue":        {"vue"},
				"nuxt":       {"nuxt"},
			})...)
		case "python":
			tags = append(tags, matchCanonicalTags(raw, map[string][]string{
				"django":     {"django"},
				"flask":      {"flask"},
				"fastapi":    {"fastapi"},
				"pytest":     {"pytest"},
				"sqlalchemy": {"sqlalchemy"},
				"pydantic":   {"pydantic"},
				"pandas":     {"pandas"},
				"numpy":      {"numpy"},
				"celery":     {"celery"},
				"requests":   {"requests"},
			})...)
		case "php":
			tags = append(tags, matchCanonicalTags(raw, map[string][]string{
				"laravel":  {"illuminate", "laravel"},
				"symfony":  {"symfony"},
				"phpunit":  {"phpunit"},
				"doctrine": {"doctrine"},
				"livewire": {"livewire"},
				"pest":     {"pest"},
			})...)
		}
	}
	return uniqueSortedTags(tags)
}

func matchCanonicalTags(raw string, patterns map[string][]string) []string {
	var tags []string
	for tag, candidates := range patterns {
		for _, candidate := range candidates {
			if strings.Contains(raw, strings.ToLower(candidate)) {
				tags = append(tags, tag)
				if tag == "xunit" || tag == "nunit" || tag == "mstest" || tag == "jest" || tag == "vitest" || tag == "playwright" || tag == "cypress" || tag == "pytest" || tag == "phpunit" || tag == "pest" || tag == "testify" {
					tags = append(tags, "test")
				}
				break
			}
		}
	}
	return uniqueSortedTags(tags)
}

func uniqueSortedTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(strings.ToLower(tag))
		if tag == "" {
			continue
		}
		if _, exists := seen[tag]; exists {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

func detectQueryIntent(query string, fileExtensions []string, pathPrefix string) SearchIntent {
	tags := tokenizeForTags(query)
	tags = append(tags, classifyRoleTags(query)...)
	tags = append(tags, inferFreeformFrameworkTags(query)...)
	for _, ext := range fileExtensions {
		cleanExt := strings.TrimPrefix(strings.ToLower(ext), ".")
		if cleanExt != "" {
			tags = append(tags, cleanExt)
		}
	}
	if pathPrefix != "" {
		tags = append(tags, tokenizeForTags(pathPrefix)...)
		tags = append(tags, classifyRoleTags(pathPrefix)...)
	}

	symbols := extractSymbolCandidates(query)
	frameworkTags := inferFreeformFrameworkTags(query)
	layerTags := collectLayerTags(query, "", nil)

	return SearchIntent{
		Query:         query,
		Tags:          uniqueSortedTags(tags),
		Symbols:       uniqueSortedTags(symbols),
		FrameworkTags: frameworkTags,
		LayerTags:     layerTags,
		PreferTests:   containsString(uniqueSortedTags(tags), "test"),
		PreferExact:   looksLikeExactSymbolQuery(query),
	}
}

func buildQueryVariants(intent SearchIntent, pathPrefix string) []string {
	var variants []string
	variants = append(variants, strings.TrimSpace(intent.Query))
	if len(intent.Symbols) > 0 {
		variants = append(variants, strings.Join(intent.Symbols, " "))
	}
	if intent.PreferTests {
		variants = append(variants, intent.Query+" unit test")
	}
	if len(intent.FrameworkTags) > 0 {
		variants = append(variants, intent.Query+" "+strings.Join(intent.FrameworkTags, " "))
	}
	if pathPrefix != "" {
		variants = append(variants, intent.Query+" "+pathPrefix)
	}
	return uniqueNonEmptyStrings(variants)
}

// addBranchFilter clones the base filter and appends a branch match condition.
func addBranchFilter(base *qdrant.Filter, branch string) *qdrant.Filter {
	branchCond := qdrant.NewMatchKeyword("branch", branch)
	if base == nil {
		return &qdrant.Filter{Must: []*qdrant.Condition{branchCond}}
	}
	existing := make([]*qdrant.Condition, len(base.Must))
	copy(existing, base.Must)
	return &qdrant.Filter{Must: append(existing, branchCond)}
}

func buildSearchFilter(fileExtensions []string, pathPrefix string) *qdrant.Filter {
	var filterConditions []*qdrant.Condition
	if len(fileExtensions) > 0 {
		var shouldMatch []*qdrant.Condition
		for _, ext := range fileExtensions {
			cleanExt := strings.TrimPrefix(strings.ToLower(ext), ".")
			shouldMatch = append(shouldMatch, qdrant.NewMatchKeyword("extension", cleanExt))
		}
		filterConditions = append(filterConditions, qdrant.NewFilterAsCondition(&qdrant.Filter{Should: shouldMatch}))
	}
	if pathPrefix != "" {
		cleanPrefix := filepath.ToSlash(strings.TrimPrefix(pathPrefix, "/"))
		filterConditions = append(filterConditions, qdrant.NewFilterAsCondition(&qdrant.Filter{
			Should: []*qdrant.Condition{
				qdrant.NewMatchKeyword("relative_path", cleanPrefix),
				qdrant.NewMatchKeyword("relative_dirs", cleanPrefix),
			},
		}))
	}
	if len(filterConditions) == 0 {
		return nil
	}
	return &qdrant.Filter{Must: filterConditions}
}

func rerankSearchResults(variantResults map[string][]*qdrant.ScoredPoint, intent SearchIntent, pathPrefix string) []RankedSearchResult {
	if len(variantResults) == 0 {
		return nil
	}

	combined := make(map[string]*RankedSearchResult)
	maxModified := int64(0)
	for variant, points := range variantResults {
		for idx, point := range points {
			key := pointIdentity(point)
			entry, exists := combined[key]
			if !exists {
				entry = &RankedSearchResult{Point: point}
				combined[key] = entry
			}
			entry.Score += float32(1.0 / float64(idx+10))
			entry.Reasons = append(entry.Reasons, "variant:"+variant)
			if point.Score > entry.Point.Score {
				entry.Point = point
			}
			modified := payloadInt(point.Payload, "modified")
			if modified > maxModified {
				maxModified = modified
			}
		}
	}

	var ranked []RankedSearchResult
	cleanPrefix := filepath.ToSlash(strings.Trim(strings.ToLower(pathPrefix), "/"))
	queryTokens := tokenizeForTags(strings.Join(append(intent.Tags, intent.Symbols...), " "))
	queryTagSet := make(map[string]struct{}, len(intent.Tags))
	for _, tag := range intent.Tags {
		queryTagSet[tag] = struct{}{}
	}
	frameworkSet := sliceToSet(intent.FrameworkTags)
	layerSet := sliceToSet(intent.LayerTags)
	symbolSet := sliceToSet(intent.Symbols)

	for _, entry := range combined {
		entry.Score, entry.Reasons = boostedResultScore(entry, queryTagSet, frameworkSet, layerSet, symbolSet, queryTokens, cleanPrefix, intent, maxModified)
		ranked = append(ranked, *entry)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score == ranked[j].Score {
			return ranked[i].Point.Score > ranked[j].Point.Score
		}
		return ranked[i].Score > ranked[j].Score
	})

	return ranked
}

func boostedResultScore(entry *RankedSearchResult, queryTags, frameworkSet, layerSet, symbolSet map[string]struct{}, queryTokens []string, pathPrefix string, intent SearchIntent, maxModified int64) (float32, []string) {
	score := entry.Score
	reasons := uniqueNonEmptyStrings(entry.Reasons)
	payload := entry.Point.Payload

	tagMatches := intersectCount(payloadStringList(payload, "tags"), queryTags)
	if tagMatches > 0 {
		score += float32(tagMatches) * 0.25
		reasons = append(reasons, fmt.Sprintf("tag-match:%d", tagMatches))
	}

	frameworkMatches := intersectCount(payloadStringList(payload, "framework_tags"), frameworkSet)
	if frameworkMatches > 0 {
		score += float32(frameworkMatches) * 0.45
		reasons = append(reasons, fmt.Sprintf("framework-match:%d", frameworkMatches))
	}

	layerMatches := intersectCount(payloadStringList(payload, "layer_tags"), layerSet)
	if layerMatches > 0 {
		score += float32(layerMatches) * 0.45
		reasons = append(reasons, fmt.Sprintf("layer-match:%d", layerMatches))
	}

	symbolNames := payloadStringList(payload, "symbol_names")
	exactSymbolMatches := intersectCount(symbolNames, symbolSet)
	if exactSymbolMatches > 0 {
		score += float32(exactSymbolMatches) * 0.9
		reasons = append(reasons, fmt.Sprintf("symbol-match:%d", exactSymbolMatches))
	}

	name := strings.ToLower(payloadString(payload, "name", ""))
	container := strings.ToLower(payloadString(payload, "container", ""))
	namespace := strings.ToLower(payloadString(payload, "namespace", ""))
	filePath := strings.ToLower(filepath.ToSlash(payloadString(payload, "file_path", "")))
	relPath := strings.ToLower(filepath.ToSlash(payloadString(payload, "relative_path", "")))

	for _, token := range queryTokens {
		if token == "" {
			continue
		}
		if name != "" && strings.Contains(name, token) {
			score += 0.4
			reasons = append(reasons, "name:"+token)
		}
		if container != "" && strings.Contains(container, token) {
			score += 0.35
			reasons = append(reasons, "container:"+token)
		}
		if namespace != "" && strings.Contains(namespace, token) {
			score += 0.25
			reasons = append(reasons, "namespace:"+token)
		}
		if relPath != "" && strings.Contains(relPath, token) {
			score += 0.15
			reasons = append(reasons, "path:"+token)
		}
	}

	if intent.PreferTests && payloadBool(payload, "is_test") {
		score += 0.75
		reasons = append(reasons, "intent:test")
	}

	if pathPrefix != "" && (strings.Contains(relPath, pathPrefix) || strings.Contains(filePath, pathPrefix)) {
		score += 0.5
		reasons = append(reasons, "path-prefix")
	}

	if maxModified > 0 {
		modified := payloadInt(payload, "modified")
		if modified > 0 {
			recencyBoost := float32(modified) / float32(maxModified)
			score += recencyBoost * 0.15
			reasons = append(reasons, "freshness")
		}
	}

	if intent.PreferExact && exactSymbolMatches > 0 {
		score += 0.75
		reasons = append(reasons, "intent:exact")
	}

	return score, uniqueNonEmptyStrings(reasons)
}

func payloadStringList(payload map[string]*qdrant.Value, key string) []string {
	val, exists := payload[key]
	if !exists || val == nil {
		return nil
	}

	listVal := val.GetListValue()
	if listVal == nil {
		return nil
	}

	out := make([]string, 0, len(listVal.Values))
	for _, item := range listVal.Values {
		if item == nil {
			continue
		}
		str := strings.TrimSpace(strings.ToLower(item.GetStringValue()))
		if str != "" {
			out = append(out, str)
		}
	}
	return out
}

func payloadString(payload map[string]*qdrant.Value, key, fallback string) string {
	val, exists := payload[key]
	if !exists || val == nil {
		return fallback
	}
	str := val.GetStringValue()
	if str == "" {
		return fallback
	}
	return str
}

func payloadInt(payload map[string]*qdrant.Value, key string) int64 {
	val, exists := payload[key]
	if !exists || val == nil {
		return 0
	}
	return val.GetIntegerValue()
}

func payloadBool(payload map[string]*qdrant.Value, key string) bool {
	val, exists := payload[key]
	if !exists || val == nil {
		return false
	}
	return val.GetBoolValue()
}

func buildFileSymbolNames(path, relPath, namespace string, typeNames []string) []string {
	var symbols []string
	symbols = append(symbols, tokenizeForTags(filepath.Base(path))...)
	symbols = append(symbols, tokenizeForTags(relPath)...)
	symbols = append(symbols, tokenizeForTags(namespace)...)
	for _, typeName := range typeNames {
		symbols = append(symbols, tokenizeForTags(typeName)...)
		symbols = append(symbols, normalizeSymbolVariants(typeName)...)
	}
	return uniqueSortedTags(symbols)
}

func buildFunctionSymbolNames(fn ast.FunctionNode) []string {
	var symbols []string
	symbols = append(symbols, tokenizeForTags(fn.Name)...)
	symbols = append(symbols, tokenizeForTags(fn.Container)...)
	symbols = append(symbols, tokenizeForTags(fn.Namespace)...)
	symbols = append(symbols, normalizeSymbolVariants(fn.Name)...)
	if fn.Container != "" {
		symbols = append(symbols, normalizeSymbolVariants(fn.Container)...)
		symbols = append(symbols, normalizeSymbolVariants(fn.Container+"."+fn.Name)...)
	}
	return uniqueSortedTags(symbols)
}

func normalizeSymbolVariants(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	base := filepath.Base(strings.ReplaceAll(input, "\\", "/"))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	variants := []string{
		strings.ToLower(base),
		strings.ToLower(strings.ReplaceAll(base, "_", "")),
		strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(base, "_", " "), ".", " ")),
	}
	return uniqueSortedTags(append(variants, tokenizeForTags(base)...))
}

func collectLayerTags(inputs ...interface{}) []string {
	var tags []string
	for _, input := range inputs {
		switch v := input.(type) {
		case string:
			tags = append(tags, classifyRoleTags(v)...)
		case []string:
			for _, item := range v {
				tags = append(tags, classifyRoleTags(item)...)
			}
		}
	}
	return uniqueSortedTags(tags)
}

func inferFreeformFrameworkTags(text string) []string {
	lower := strings.ToLower(text)
	return matchCanonicalTags(lower, map[string][]string{
		"xunit": {"xunit"}, "nunit": {"nunit"}, "mstest": {"mstest"}, "efcore": {"efcore", "entity framework"},
		"mediatr": {"mediatr"}, "fluentvalidation": {"fluentvalidation"}, "automapper": {"automapper"},
		"aspnetcore": {"aspnetcore", "asp.net core"}, "serilog": {"serilog"}, "dapper": {"dapper"},
		"hangfire": {"hangfire"}, "masstransit": {"masstransit"}, "gin": {"gin"}, "echo": {"echo"},
		"fiber": {"fiber"}, "chi": {"chi"}, "grpc": {"grpc"}, "gorm": {"gorm"}, "sqlx": {"sqlx"},
		"cobra": {"cobra"}, "viper": {"viper"}, "testify": {"testify"}, "gqlgen": {"gqlgen"},
		"protobuf": {"protobuf"}, "react": {"react"}, "nextjs": {"nextjs", "next.js"}, "express": {"express"},
		"nestjs": {"nestjs", "nest.js"}, "jest": {"jest"}, "vitest": {"vitest"}, "playwright": {"playwright"},
		"cypress": {"cypress"}, "prisma": {"prisma"}, "mongoose": {"mongoose"}, "redux": {"redux"},
		"vue": {"vue"}, "nuxt": {"nuxt"}, "django": {"django"}, "flask": {"flask"}, "fastapi": {"fastapi"},
		"pytest": {"pytest"}, "sqlalchemy": {"sqlalchemy"}, "pydantic": {"pydantic"}, "pandas": {"pandas"},
		"numpy": {"numpy"}, "celery": {"celery"}, "requests": {"requests"}, "laravel": {"laravel"},
		"symfony": {"symfony"}, "phpunit": {"phpunit"}, "doctrine": {"doctrine"}, "livewire": {"livewire"}, "pest": {"pest"},
	})
}

func detectTestFramework(tags []string) string {
	for _, tag := range []string{"xunit", "nunit", "mstest", "jest", "vitest", "playwright", "cypress", "pytest", "phpunit", "pest", "testify"} {
		if containsString(tags, tag) {
			return tag
		}
	}
	return ""
}

func hasTag(tags []string, target string) bool {
	return containsString(tags, target)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func extractSymbolCandidates(query string) []string {
	re := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_./-]*`)
	matches := re.FindAllString(query, -1)
	var symbols []string
	for _, match := range matches {
		symbols = append(symbols, normalizeSymbolVariants(match)...)
	}
	return uniqueSortedTags(symbols)
}

func looksLikeExactSymbolQuery(query string) bool {
	return strings.ContainsAny(query, "/._") || regexp.MustCompile(`[a-z][A-Z]|[A-Z][a-z]+[A-Z]`).MatchString(query)
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func pointIdentity(point *qdrant.ScoredPoint) string {
	if point == nil {
		return ""
	}
	if point.Id != nil {
		if uuid := point.Id.GetUuid(); uuid != "" {
			return uuid
		}
		if num := point.Id.GetNum(); num != 0 {
			return fmt.Sprintf("num:%d", num)
		}
	}
	filePath := payloadString(point.Payload, "file_path", "")
	name := payloadString(point.Payload, "name", "")
	startLine := payloadInt(point.Payload, "start_line")
	return fmt.Sprintf("%s|%s|%d", filePath, name, startLine)
}

func intersectCount(values []string, set map[string]struct{}) int {
	count := 0
	seen := map[string]struct{}{}
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		if _, ok := set[value]; ok {
			count++
		}
	}
	return count
}

func sliceToSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
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

// isParseableDocExt reports whether the extension is a binary document format
// that has a dedicated parser (PDF/Office), so it should bypass the binary guard.
func isParseableDocExt(ext string) bool {
	switch ext {
	case ".pdf", ".doc", ".docx", ".xls", ".xlsx":
		return true
	}
	return false
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
	"few":  {}, "for": {}, "from": {}, "further": {},
	"had": {}, "hadnt": {}, "has": {}, "hasnt": {}, "have": {}, "havent": {}, "having": {}, "he": {}, "hed": {}, "hell": {}, "hes": {}, "her": {}, "here": {}, "heres": {}, "hers": {}, "herself": {}, "him": {}, "himself": {}, "his": {}, "how": {}, "hows": {},
	"i": {}, "id": {}, "ill": {}, "im": {}, "ive": {}, "if": {}, "in": {}, "into": {}, "is": {}, "isnt": {}, "it": {}, "its": {}, "itself": {},
	"lets": {},
	"me":   {}, "more": {}, "most": {}, "mustnt": {}, "my": {}, "myself": {},
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
		h := fnv.New64a()
		h.Write([]byte(token))
		v := h.Sum64()
		idx := uint32(v ^ (v >> 32))

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
