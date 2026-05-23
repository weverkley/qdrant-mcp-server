package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

	"qdrant-mcp-server/ast"
)

// --- Ingestion Structural Blocks ---
type IngestionWorker struct {
	cfg          Config
	qdrantClient *qdrant.Client
	httpClient   *http.Client
	mu           sync.Mutex
	pendingFiles map[string]time.Time
	activeSyncs  int
	totalSynced  int
	sem          chan struct{} // semaphore to rate limit concurrent embedding workers
}

type OllamaEmbedReq struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type OllamaEmbedResp struct {
	Embedding []float32 `json:"embedding"`
}

func (iw *IngestionWorker) syncFileState(ctx context.Context, path string) {
	if iw.sem != nil {
		select {
		case iw.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-iw.sem }()
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

	ext := strings.ToLower(filepath.Ext(path))
	extClean := strings.TrimPrefix(ext, ".")
	relPath, _ := filepath.Rel(iw.cfg.WatchDirectory, path)
	relDirs := getParentDirs(relPath)
	var points []*qdrant.PointStruct

	// Determine file categories based on ParserMode
	isSupportedCode := false
	isSupportedDoc := false
	switch ext {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".php", ".cs", ".py":
		if iw.cfg.ParserMode == "code" || iw.cfg.ParserMode == "full" {
			isSupportedCode = true
		}
	case ".pdf", ".md", ".txt", ".csv", ".xls", ".xlsx", ".doc", ".docx":
		if iw.cfg.ParserMode == "doc" || iw.cfg.ParserMode == "full" {
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
			// Compute embeddings of the function code body (held in fn.Signature)
			vector, err := iw.fetchRemoteEmbedding(ctx, fn.Signature)
			if err != nil {
				log.Printf("Ollama vectorization failed on AST function %s in %s: %v", fn.Name, path, err)
				continue
			}

			// Ensure unique points across workspace updates
			deterministicSeed := fmt.Sprintf("%s_func_%s_%d", path, fn.Name, idx)
			hash := sha1.Sum([]byte(deterministicSeed))
			id, _ := uuid.FromBytes(hash[:16])

			payload := map[string]interface{}{
				"file_path":     path,
				"content":       fn.Signature,
				"type":          "function",
				"name":          fn.Name,
				"start_line":    int64(fn.StartLine),
				"end_line":      int64(fn.EndLine),
				"language":      fn.Language,
				"extension":     extClean,
				"relative_path": relPath,
				"relative_dirs": relDirs,
				"updated":       time.Now().Unix(),
			}
			if fn.Receiver != "" {
				payload["receiver"] = fn.Receiver
			}

			points = append(points, &qdrant.PointStruct{
				Id:      qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectors(vector...),
				Payload: qdrant.NewValueMap(payload),
			})
		}
	} else if isDocParsed {
		for idx, chunk := range docChunks {
			vector, err := iw.fetchRemoteEmbedding(ctx, chunk.Content)
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
				"updated":       time.Now().Unix(),
			}
			if chunk.PageNumber > 0 {
				payload["page_number"] = int64(chunk.PageNumber)
			}

			points = append(points, &qdrant.PointStruct{
				Id:      qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectors(vector...),
				Payload: qdrant.NewValueMap(payload),
			})
		}
	} else {
		// Fall back to basic sliding window line chunking
		chunks := iw.chunkText(string(content), 1000)
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

			payload := map[string]interface{}{
				"file_path":     path,
				"content":       chunk,
				"type":          "chunk",
				"extension":     extClean,
				"relative_path": relPath,
				"relative_dirs": relDirs,
				"updated":       time.Now().Unix(),
			}

			points = append(points, &qdrant.PointStruct{
				Id:      qdrant.NewIDUUID(id.String()),
				Vectors: qdrant.NewVectors(vector...),
				Payload: qdrant.NewValueMap(payload),
			})
		}
	}

	if len(points) > 0 {
		_, err = iw.qdrantClient.Upsert(ctx, &qdrant.UpsertPoints{
			CollectionName: iw.cfg.CollectionName,
			Points:         points,
		})
		if err != nil {
			log.Printf("gRPC Batch Upsert onto collection '%s' failed: %v", iw.cfg.CollectionName, err)
		} else {
			log.Printf("Successfully synchronized %d vectors for %s (AST parsed: %t)", len(points), path, len(functions) > 0)
		}
	}
}

func (iw *IngestionWorker) purgeFileVectors(ctx context.Context, path string) error {
	_, err := iw.qdrantClient.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: iw.cfg.CollectionName,
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

	log.Printf("Found %d files to ingest. Starting concurrent ingestion (max workers: %d)...", len(filesToIngest), iw.cfg.MaxEmbeddingWorkers)
	var wg sync.WaitGroup
	for i, path := range filesToIngest {
		wg.Add(1)
		go func(idx int, p string) {
			defer wg.Done()
			log.Printf("[%d/%d] Ingesting %s...", idx+1, len(filesToIngest), p)
			iw.syncFileState(ctx, p)
		}(i, path)
	}
	wg.Wait()

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

func (iw *IngestionWorker) executeVectorSearch(ctx context.Context, query string, fileExtensions []string, pathPrefix string) (string, error) {
	// Step A: Vectorize the search query using your home lab Ollama endpoint
	vector, err := iw.fetchRemoteEmbedding(ctx, query)
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
	queryResponse, err := iw.qdrantClient.Query(ctx, &qdrant.QueryPoints{
		CollectionName: iw.cfg.CollectionName,
		Query:          qdrant.NewQueryDense(vector),
		Limit:          qdrant.PtrOf(uint64(5)),
		Filter:         qdrantFilter,
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
