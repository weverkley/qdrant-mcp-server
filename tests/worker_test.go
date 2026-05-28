package tests

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/qdrant/go-client/qdrant"

	"qdrant-mcp-server/server"
)

type MockQdrantClient struct {
	mu                 sync.Mutex
	upsertCalls        [][]*qdrant.PointStruct
	upsertErr          error
	scrollResp         []*qdrant.RetrievedPoint
	scrollErr          error
	deleteCalls        []string
	deletePointsCalls  []*qdrant.DeletePoints
	queryCalls         []*qdrant.QueryPoints
	queryResp          []*qdrant.ScoredPoint
	queryErr           error
	setPayloadCalls    []*qdrant.SetPayloadPoints
}

func (m *MockQdrantClient) Upsert(ctx context.Context, in *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertCalls = append(m.upsertCalls, in.Points)
	return &qdrant.UpdateResult{}, m.upsertErr
}

func (m *MockQdrantClient) Delete(ctx context.Context, in *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, in.CollectionName)
	m.deletePointsCalls = append(m.deletePointsCalls, in)
	return &qdrant.UpdateResult{}, nil
}

func (m *MockQdrantClient) CollectionExists(ctx context.Context, collectionName string) (bool, error) {
	return true, nil
}

func (m *MockQdrantClient) CreateCollection(ctx context.Context, in *qdrant.CreateCollection) error {
	return nil
}

func (m *MockQdrantClient) Query(ctx context.Context, in *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queryCalls = append(m.queryCalls, in)
	return m.queryResp, m.queryErr
}

func (m *MockQdrantClient) Scroll(ctx context.Context, in *qdrant.ScrollPoints) ([]*qdrant.RetrievedPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scrollResp, m.scrollErr
}

func (m *MockQdrantClient) SetPayload(ctx context.Context, in *qdrant.SetPayloadPoints) (*qdrant.UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setPayloadCalls = append(m.setPayloadCalls, in)
	return &qdrant.UpdateResult{}, nil
}

func TestBatchUpserter_BatchSizeTrigger(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 3
	timeout := 10 * time.Second // large timeout so it doesn't trigger by timeout
	b := server.NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
	defer b.Close()

	// Add 2 points (less than batch size)
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(1)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(2)})

	// Verify no upsert calls yet
	mockClient.mu.Lock()
	callsCount := len(mockClient.upsertCalls)
	mockClient.mu.Unlock()
	if callsCount != 0 {
		t.Fatalf("Expected 0 upsert calls, got %d", callsCount)
	}

	// Add the 3rd point (reaches batch size)
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(3)})

	// Wait a tiny bit for the goroutine to process the size-based flush
	time.Sleep(50 * time.Millisecond)

	mockClient.mu.Lock()
	callsCount = len(mockClient.upsertCalls)
	var firstBatchSize int
	if callsCount > 0 {
		firstBatchSize = len(mockClient.upsertCalls[0])
	}
	mockClient.mu.Unlock()

	if callsCount != 1 {
		t.Fatalf("Expected 1 upsert call, got %d", callsCount)
	}
	if firstBatchSize != 3 {
		t.Fatalf("Expected batch size of 3, got %d", firstBatchSize)
	}
}

func TestBatchUpserter_TimeoutTrigger(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 10 // large batch size
	timeout := 50 * time.Millisecond
	b := server.NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
	defer b.Close()

	// Add 2 points
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(1)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(2)})

	// Wait for timeout to trigger
	time.Sleep(150 * time.Millisecond)

	mockClient.mu.Lock()
	callsCount := len(mockClient.upsertCalls)
	var firstBatchSize int
	if callsCount > 0 {
		firstBatchSize = len(mockClient.upsertCalls[0])
	}
	mockClient.mu.Unlock()

	if callsCount != 1 {
		t.Fatalf("Expected 1 upsert call due to timeout, got %d", callsCount)
	}
	if firstBatchSize != 2 {
		t.Fatalf("Expected batch size of 2, got %d", firstBatchSize)
	}
}

func TestBatchUpserter_ExplicitFlush(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 10
	timeout := 10 * time.Second
	b := server.NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
	defer b.Close()

	// Add 3 points
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(1)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(2)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(3)})

	// Call Flush explicitly
	b.Flush()

	mockClient.mu.Lock()
	callsCount := len(mockClient.upsertCalls)
	var firstBatchSize int
	if callsCount > 0 {
		firstBatchSize = len(mockClient.upsertCalls[0])
	}
	mockClient.mu.Unlock()

	if callsCount != 1 {
		t.Fatalf("Expected 1 upsert call after explicit Flush, got %d", callsCount)
	}
	if firstBatchSize != 3 {
		t.Fatalf("Expected batch size of 3, got %d", firstBatchSize)
	}
}

func TestBatchUpserter_Concurrency(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 5
	timeout := 50 * time.Millisecond
	b := server.NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
	defer b.Close()

	numWorkers := 10
	pointsPerWorker := 3 // Total points = 30
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < pointsPerWorker; j++ {
				b.Add(&qdrant.PointStruct{
					Id: qdrant.NewIDNum(uint64(workerID*100 + j)),
				})
			}
		}(i)
	}

	wg.Wait()
	b.Flush()

	mockClient.mu.Lock()
	totalPointsUpserted := 0
	for _, call := range mockClient.upsertCalls {
		totalPointsUpserted += len(call)
	}
	mockClient.mu.Unlock()

	if totalPointsUpserted != 30 {
		t.Fatalf("Expected 30 total points upserted, got %d", totalPointsUpserted)
	}
}

func TestBatchUpserter_Close(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 10
	timeout := 10 * time.Second
	b := server.NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)

	// Add 3 points
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(1)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(2)})
	b.Add(&qdrant.PointStruct{Id: qdrant.NewIDNum(3)})

	// Close immediately (should flush remaining points)
	b.Close()

	mockClient.mu.Lock()
	callsCount := len(mockClient.upsertCalls)
	var firstBatchSize int
	if callsCount > 0 {
		firstBatchSize = len(mockClient.upsertCalls[0])
	}
	mockClient.mu.Unlock()

	if callsCount != 1 {
		t.Fatalf("Expected 1 upsert call after Close, got %d", callsCount)
	}
	if firstBatchSize != 3 {
		t.Fatalf("Expected batch size of 3, got %d", firstBatchSize)
	}
}

type MockRoundTripper struct {
	RoundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.RoundTripFunc(req)
}

func TestSyncFileState_ContentHashing(t *testing.T) {
	// Create temporary test file
	tmpFile, err := os.CreateTemp("", "test_hash_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())

	content := []byte("hello world")
	if _, err := tmpFile.Write(content); err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()

	// Set up mock client
	mockClient := &MockQdrantClient{}

	Cfg := server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		BatchSize:           1,
		BatchTimeout:        1 * time.Second,
	}

	worker := server.NewIngestionWorker(Cfg, mockClient, nil)
	defer worker.Close()

	mockHTTP := &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			respBody := `{"embedding": [0.1, 0.2, 0.3]}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}
	worker.HTTPClient.Transport = mockHTTP

	// --- 1. First Ingestion (Empty Qdrant, should ingest) ---
	mockClient.scrollResp = nil
	worker.SyncFileState(context.Background(), tmpFile.Name())
	worker.BatchUpserter.Flush()

	mockClient.mu.Lock()
	initialUpsertCount := len(mockClient.upsertCalls)
	initialDeleteCount := len(mockClient.deleteCalls)
	mockClient.mu.Unlock()

	if initialUpsertCount != 1 {
		t.Fatalf("Expected exactly 1 upsert call on first ingestion, got %d", initialUpsertCount)
	}
	if initialDeleteCount != 1 {
		t.Fatalf("Expected exactly 1 delete call to purge old vectors, got %d", initialDeleteCount)
	}

	// --- 2. Second Ingestion with matching hash (should skip) ---
	mockClient.mu.Lock()
	mockClient.upsertCalls = nil
	mockClient.deleteCalls = nil
	mockClient.mu.Unlock()

	localHash := fmt.Sprintf("%x", sha256.Sum256(content))
	mockClient.scrollResp = []*qdrant.RetrievedPoint{
		{
			Payload: map[string]*qdrant.Value{
				"file_hash": qdrant.NewValueString(localHash),
			},
		},
	}

	worker.SyncFileState(context.Background(), tmpFile.Name())
	worker.BatchUpserter.Flush()

	mockClient.mu.Lock()
	skipUpsertCount := len(mockClient.upsertCalls)
	skipDeleteCount := len(mockClient.deleteCalls)
	mockClient.mu.Unlock()

	if skipUpsertCount != 0 {
		t.Fatalf("Expected 0 upsert calls when hash matches, got %d", skipUpsertCount)
	}
	if skipDeleteCount != 0 {
		t.Fatalf("Expected 0 delete calls when hash matches, got %d", skipDeleteCount)
	}

	// --- 3. Third Ingestion with differing hash (should re-ingest) ---
	mockClient.mu.Lock()
	mockClient.upsertCalls = nil
	mockClient.deleteCalls = nil
	mockClient.mu.Unlock()

	mockClient.scrollResp = []*qdrant.RetrievedPoint{
		{
			Payload: map[string]*qdrant.Value{
				"file_hash": qdrant.NewValueString("some-stale-hash"),
			},
		},
	}

	worker.SyncFileState(context.Background(), tmpFile.Name())
	worker.BatchUpserter.Flush()

	mockClient.mu.Lock()
	staleUpsertCount := len(mockClient.upsertCalls)
	staleDeleteCount := len(mockClient.deleteCalls)
	mockClient.mu.Unlock()

	if staleUpsertCount != 1 {
		t.Fatalf("Expected 1 upsert call when hash does not match, got %d", staleUpsertCount)
	}
	if staleDeleteCount != 1 {
		t.Fatalf("Expected 1 delete call when hash does not match, got %d", staleDeleteCount)
	}
}

func TestConcurrencyController_AIMD(t *testing.T) {
	c := server.NewConcurrencyController(4)

	// Verify initial limit is max
	if limit := c.GetLimit(); limit != 4 {
		t.Fatalf("Expected limit 4, got %d", limit)
	}

	// Record failures to check decrease
	c.RecordFailure("OOM error") // Halves to 2
	if limit := c.GetLimit(); limit != 2 {
		t.Fatalf("Expected limit 2 after 1 failure, got %d", limit)
	}

	c.RecordFailure("OOM error") // Halves to 1
	if limit := c.GetLimit(); limit != 1 {
		t.Fatalf("Expected limit 1 after 2 failures, got %d", limit)
	}

	c.RecordFailure("OOM error") // Minimum limit remains 1
	if limit := c.GetLimit(); limit != 1 {
		t.Fatalf("Expected limit 1 (minimum limit), got %d", limit)
	}

	// Record fast successes to check increase (requires 5 consecutive fast successes)
	for i := 0; i < 4; i++ {
		c.RecordSuccess(100 * time.Millisecond)
		if limit := c.GetLimit(); limit != 1 {
			t.Fatalf("Limit should stay 1 until 5 consecutive successes, got %d on success %d", limit, i+1)
		}
	}

	c.RecordSuccess(100 * time.Millisecond) // 5th success: increases to 2
	if limit := c.GetLimit(); limit != 2 {
		t.Fatalf("Expected limit 2 after 5 successes, got %d", limit)
	}

	// Success with slow response should decrease
	c.RecordSuccess(2 * time.Second) // > 1.5s -> halves to 1
	if limit := c.GetLimit(); limit != 1 {
		t.Fatalf("Expected limit to scale down to 1 on slow response, got %d", limit)
	}
}

func TestFetchRemoteEmbedding_RetryAndThrottle(t *testing.T) {
	mockClient := &MockQdrantClient{}
	Cfg := server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 4,
	}

	worker := server.NewIngestionWorker(Cfg, mockClient, nil)
	defer worker.Close()

	attempts := 0
	mockHTTP := &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts < 3 {
				// Return 429 for the first two attempts
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader("Too Many Requests")),
					Header:     make(http.Header),
				}, nil
			}
			// Succeed on the third attempt
			respBody := `{"embedding": [0.5, 0.6, 0.7]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}
	worker.HTTPClient.Transport = mockHTTP

	ctx := context.Background()
	emb, err := worker.FetchRemoteEmbedding(ctx, "hello test")
	if err != nil {
		t.Fatalf("Expected embedding fetch to eventually succeed after retries, got error: %v", err)
	}

	if len(emb) != 3 {
		t.Fatalf("Expected embedding length 3, got %d", len(emb))
	}
	if attempts != 3 {
		t.Fatalf("Expected exactly 3 attempts, got %d", attempts)
	}

	// Verify concurrency limit scaled down due to 429 failures
	if limit := worker.ConcurrencyController.GetLimit(); limit != 1 {
		t.Fatalf("Expected concurrency limit to be scaled down to 1, got %d", limit)
	}
}

func TestSyncFileState_IgnoreLogAndConfigFolder(t *testing.T) {
	mockClient := &MockQdrantClient{}
	Cfg := server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
	}

	worker := server.NewIngestionWorker(Cfg, mockClient, nil)
	defer worker.Close()

	// Test absolute legacy file path
	logPath := filepath.Join(os.TempDir(), ".qdrant-mcp-server.log")
	worker.SyncFileState(context.Background(), logPath)

	// Test path inside the dedicated folder
	dedicatedLogPath := filepath.Join(os.TempDir(), ".qdrant-mcp-server", "qdrant-mcp-server.log")
	worker.SyncFileState(context.Background(), dedicatedLogPath)

	dedicatedStopWordsPath := filepath.Join(os.TempDir(), ".qdrant-mcp-server", ".mcp-stopwords")
	worker.SyncFileState(context.Background(), dedicatedStopWordsPath)

	dedicatedRandomPath := filepath.Join(os.TempDir(), ".qdrant-mcp-server", "some-other-file.go")
	worker.SyncFileState(context.Background(), dedicatedRandomPath)

	mockClient.mu.Lock()
	upsertCount := len(mockClient.upsertCalls)
	mockClient.mu.Unlock()

	if upsertCount != 0 {
		t.Fatalf("Expected log file and .qdrant-mcp-server folder content to be completely ignored, got %d upserts", upsertCount)
	}
}

func TestComputeSparseVector(t *testing.T) {
	text := "Hello world! This is a test_vector symbol. Hello code."
	indices, values := server.ComputeSparseVector(text, nil)

	// "hello", "world", "test_vector", "symbol", "code" are valid tokens.
	// "this", "is", "a" are stop words.
	// Let's verify that the length of indices and values are the same
	if len(indices) != len(values) {
		t.Fatalf("Expected indices and values lengths to be equal, got %d and %d", len(indices), len(values))
	}

	// We expect 5 unique tokens
	if len(indices) != 5 {
		t.Fatalf("Expected 5 unique tokens, got %d", len(indices))
	}

	// Verify indices are sorted in ascending order
	for i := 1; i < len(indices); i++ {
		if indices[i] <= indices[i-1] {
			t.Errorf("Expected indices to be sorted, but index %d is not greater than %d", indices[i], indices[i-1])
		}
	}

	// Verify that "test_vector" has a higher weight than "code" because:
	// "test_vector" has tf=1, length=11 -> weight = 1 * log(1 + 11) = log(12)
	// "code" has tf=1, length=4 -> weight = 1 * log(1 + 4) = log(5)
	// Let's find their indexes and compare values
	var testVectorWeight, codeWeight float32
	h := fnv.New64a()
	h.Write([]byte("test_vector"))
	v := h.Sum64()
	testVectorIdx := uint32(v ^ (v >> 32))

	h2 := fnv.New64a()
	h2.Write([]byte("code"))
	v2 := h2.Sum64()
	codeIdx := uint32(v2 ^ (v2 >> 32))

	for i, idx := range indices {
		if idx == testVectorIdx {
			testVectorWeight = values[i]
		}
		if idx == codeIdx {
			codeWeight = values[i]
		}
	}

	if testVectorWeight == 0 {
		t.Errorf("Expected to find weight for 'test_vector'")
	}
	if codeWeight == 0 {
		t.Errorf("Expected to find weight for 'code'")
	}
	if testVectorWeight <= codeWeight {
		t.Errorf("Expected weight of 'test_vector' (%f) to be greater than 'code' (%f)", testVectorWeight, codeWeight)
	}
}

func TestExecuteVectorSearch_SearchModes(t *testing.T) {
	mockClient := &MockQdrantClient{}
	Cfg := server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		SearchMode:          "dense",
	}

	worker := server.NewIngestionWorker(Cfg, mockClient, nil)
	defer worker.Close()

	// Mock Ollama host HTTP response for fetching query embedding
	mockHTTP := &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			respBody := `{"embedding": [0.1, 0.2, 0.3]}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}
	worker.HTTPClient.Transport = mockHTTP

	ctx := context.Background()

	// Test 1: Dense Search Mode
	worker.Cfg.SearchMode = "dense"
	_, _ = worker.ExecuteVectorSearch(ctx, "test query", nil, "")

	mockClient.mu.Lock()
	if len(mockClient.queryCalls) == 0 {
		mockClient.mu.Unlock()
		t.Fatalf("Expected at least 1 query call, got %d", len(mockClient.queryCalls))
	}
	denseCall := mockClient.queryCalls[len(mockClient.queryCalls)-1]
	mockClient.queryCalls = nil // reset
	mockClient.mu.Unlock()

	if denseCall.Query == nil || denseCall.Query.GetNearest() == nil || denseCall.Query.GetNearest().GetDense() == nil {
		t.Errorf("Expected dense query to be set")
	}
	if len(denseCall.Prefetch) != 0 {
		t.Errorf("Expected no prefetch queries for dense mode")
	}

	// Test 2: Sparse Search Mode
	worker.Cfg.SearchMode = "sparse"
	_, _ = worker.ExecuteVectorSearch(ctx, "test query", nil, "")

	mockClient.mu.Lock()
	if len(mockClient.queryCalls) == 0 {
		mockClient.mu.Unlock()
		t.Fatalf("Expected at least 1 query call for sparse, got %d", len(mockClient.queryCalls))
	}
	sparseCall := mockClient.queryCalls[len(mockClient.queryCalls)-1]
	mockClient.queryCalls = nil // reset
	mockClient.mu.Unlock()

	if sparseCall.Query == nil || sparseCall.Query.GetNearest() == nil || sparseCall.Query.GetNearest().GetSparse() == nil {
		t.Errorf("Expected sparse query to be set")
	}
	if sparseCall.Using == nil || *sparseCall.Using != "sparse" {
		t.Errorf("Expected using to be 'sparse', got %v", sparseCall.Using)
	}

	// Test 3: Hybrid Search Mode
	worker.Cfg.SearchMode = "hybrid"
	_, _ = worker.ExecuteVectorSearch(ctx, "test query", nil, "")

	mockClient.mu.Lock()
	if len(mockClient.queryCalls) == 0 {
		mockClient.mu.Unlock()
		t.Fatalf("Expected at least 1 query call for hybrid, got %d", len(mockClient.queryCalls))
	}
	hybridCall := mockClient.queryCalls[len(mockClient.queryCalls)-1]
	mockClient.queryCalls = nil // reset
	mockClient.mu.Unlock()

	if hybridCall.Query == nil || hybridCall.Query.GetFusion() != qdrant.Fusion_RRF {
		t.Errorf("Expected fusion query (RRF) to be set, got %v", hybridCall.Query)
	}
	if len(hybridCall.Prefetch) != 2 {
		t.Fatalf("Expected 2 prefetch queries in hybrid mode, got %d", len(hybridCall.Prefetch))
	}

	densePrefetch := hybridCall.Prefetch[0]
	sparsePrefetch := hybridCall.Prefetch[1]

	if densePrefetch.Query == nil || densePrefetch.Query.GetNearest() == nil || densePrefetch.Query.GetNearest().GetDense() == nil {
		t.Errorf("Expected dense prefetch query to be set")
	}
	if densePrefetch.Using != nil && *densePrefetch.Using != "" {
		t.Errorf("Expected dense prefetch to have empty/nil Using, got %v", densePrefetch.Using)
	}

	if sparsePrefetch.Query == nil || sparsePrefetch.Query.GetNearest() == nil || sparsePrefetch.Query.GetNearest().GetSparse() == nil {
		t.Errorf("Expected sparse prefetch query to be set")
	}
	if sparsePrefetch.Using == nil || *sparsePrefetch.Using != "sparse" {
		t.Errorf("Expected sparse prefetch Using to be 'sparse', got %v", sparsePrefetch.Using)
	}
}

func TestComputeSparseVector_CustomStopWords(t *testing.T) {
	// Test multilingual stop words are filtered correctly
	// Spanish "de", Portuguese "com", German "und" are stop words in our new map!
	text := "hola de amigo com hello und code"
	// Without custom stop words
	indices, _ := server.ComputeSparseVector(text, nil)
	// Expected unique tokens: "hola" (not stop), "amigo" (not stop), "hello" (not stop), "code" (not stop).
	// "de", "com", "und" are multilingual stop-words!
	if len(indices) != 4 {
		t.Fatalf("Expected 4 tokens after multilingual stop-word filtering, got %d", len(indices))
	}

	// Test custom stop words file
	customStopWords := map[string]struct{}{
		"amigo": {},
		"hello": {},
	}
	indices2, _ := server.ComputeSparseVector(text, customStopWords)
	// Expected unique tokens: "hola", "code" (since "amigo" and "hello" are custom-filtered!)
	if len(indices2) != 2 {
		t.Fatalf("Expected 2 tokens after custom stop-word filtering, got %d", len(indices2))
	}
}

func TestShouldIgnoreFile(t *testing.T) {
	mockClient := &MockQdrantClient{}
	Cfg := server.Config{
		ExcludeDirs:       []string{"node_modules"},
		IncludeHiddenDirs: []string{".allowed-hidden"},
		ExcludeExtensions: []string{".sql", "json"},
	}
	worker := server.NewIngestionWorker(Cfg, mockClient, nil)

	tests := []struct {
		path   string
		isDir  bool
		ignore bool
	}{
		// DB Migration patterns
		{"/src/Migrations/AgroOpsDbContextModelSnapshot.cs", false, true},
		{"/src/migrations/20260523131344_InitialCreate.Designer.cs", false, true},
		{"/src/Migrations/20260523131344_InitialCreate.cs", false, true},
		{"/src/Migrations", true, true},

		// Designer and Snapshot patterns
		{"/src/SomeFolder/AgroOpsDbContextModelSnapshot.cs", false, true},
		{"/src/SomeFolder/InitialCreate.Designer.cs", false, true},
		{"/src/SomeFolder/InitialCreate.designer.cs", false, true},

		// Minified and map file patterns
		{"/src/SomeFolder/app.min.js", false, true},
		{"/src/SomeFolder/styles.min.css", false, true},
		{"/src/SomeFolder/app.js.map", false, true},
		{"/src/SomeFolder/styles.css.map", false, true},

		// AI agent temporary files
		{"/src/SomeFolder/ServiceCollectionExtensions.cs.tmp.162720.ea80bb6bbe20", false, true},
		{"/src/SomeFolder/OperationalSettingConfiguration.cs.tmp.162720.3a57ced2b041", false, true},
		{"/src/SomeFolder/LegalSourceConfiguration.cs.tmp.162720.d752904c92b1", false, true},
		{"/src/SomeFolder/LegalResearchItemConfiguration.cs.tmp.162720.ccb4c979dcf3", false, true},

		// Custom Exclude extensions
		{"/src/SomeFolder/schema.sql", false, true},
		{"/src/SomeFolder/config.json", false, true},
		{"/src/SomeFolder/config.yaml", false, false},

		// Normal files & folders
		{"/src/SomeFolder/NormalCode.cs", false, false},
		{"/src/SomeFolder/NormalCode.go", false, false},
		{"/src/SomeFolder", true, false},

		// Hidden directories & allowed hidden directories
		{"/src/.git", true, true},
		{"/src/.allowed-hidden", true, false},
		{"/src/.allowed-hidden/file.txt", false, false},
	}

	for _, tc := range tests {
		res := worker.ShouldIgnoreFile(tc.path, tc.isDir)
		if res != tc.ignore {
			t.Errorf("ShouldIgnoreFile(%q, %t) = %t; want %t", tc.path, tc.isDir, res, tc.ignore)
		}
	}
}

func TestSyncFileState_MaxFileSize(t *testing.T) {
	// Create a temp file
	tempFile, err := os.CreateTemp("", "large_file_*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := "some file content that has some bytes"
	if _, err := tempFile.WriteString(content); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	tempFile.Close()

	mockClient := &MockQdrantClient{}
	Cfg := server.Config{
		MaxFileSize: 10, // set very small limit
	}
	worker := server.NewIngestionWorker(Cfg, mockClient, nil)

	ctx := context.Background()
	// Call SyncFileState
	worker.SyncFileState(ctx, tempFile.Name())

	// Since the file is 37 bytes, and limit is 10, it should be skipped and no scroll or delete or upsert should be called.
	mockClient.mu.Lock()
	upsertCalls := len(mockClient.upsertCalls)
	mockClient.mu.Unlock()

	if upsertCalls != 0 {
		t.Errorf("Expected 0 upsert calls (file skipped due to MaxFileSize), got %d", upsertCalls)
	}
}

func TestWatchLoop_WatchesNewDirectories(t *testing.T) {
	rootDir := t.TempDir()

	mockClient := &MockQdrantClient{}
	worker := server.NewIngestionWorker(server.Config{
		WatchDirectory:      rootDir,
		DebounceDuration:    10 * time.Millisecond,
		ExcludeExtensions:   []string{".sql"},
		MaxEmbeddingWorkers: 1,
	}, mockClient, nil)
	defer worker.Close()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("Failed to create watcher: %v", err)
	}
	defer watcher.Close()

	if err := watcher.Add(rootDir); err != nil {
		t.Fatalf("Failed to watch root directory: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventChan := make(chan string, 10)
	go worker.WatchLoop(ctx, watcher, eventChan)

	newDir := filepath.Join(rootDir, "tests", "AgroOps.Application.Tests")
	if err := os.MkdirAll(newDir, 0755); err != nil {
		t.Fatalf("Failed to create nested directory: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	newFile := filepath.Join(newDir, "DtcFrameDecoderServiceTests.cs")
	if err := os.WriteFile(newFile, []byte("class DtcFrameDecoderServiceTests {}"), 0644); err != nil {
		t.Fatalf("Failed to create file in new directory: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case path := <-eventChan:
			if path == newFile {
				return
			}
		case <-deadline:
			t.Fatalf("Timed out waiting for watcher event for %s", newFile)
		}
	}
}

func TestSyncFileState_AddsTagsToPayload(t *testing.T) {
	rootDir := t.TempDir()
	testFile := filepath.Join(rootDir, "tests", "AgroOps.Application.Tests", "DtcFrameDecoderServiceTests.cs")
	if err := os.MkdirAll(filepath.Dir(testFile), 0755); err != nil {
		t.Fatal(err)
	}

	content := `using Xunit;
namespace AgroOps.Application.Tests;
public class DtcFrameDecoderServiceTests
{
    public void DecodeFrame_ShouldReturnExpectedResult() {}
}`
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	mockClient := &MockQdrantClient{}
	worker := server.NewIngestionWorker(server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      rootDir,
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		ParserMode:          "full",
		MaxEmbeddingWorkers: 1,
		BatchSize:           1,
		BatchTimeout:        time.Second,
	}, mockClient, nil)
	defer worker.Close()

	worker.HTTPClient.Transport = &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			respBody := `{"embedding": [0.1, 0.2, 0.3]}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}

	worker.SyncFileState(context.Background(), testFile)
	worker.BatchUpserter.Flush()

	mockClient.mu.Lock()
	defer mockClient.mu.Unlock()
	if len(mockClient.upsertCalls) == 0 || len(mockClient.upsertCalls[0]) == 0 {
		t.Fatalf("Expected at least one upserted point")
	}

	payload := mockClient.upsertCalls[0][0].Payload
	tagVals, ok := payload["tags"]
	if !ok {
		t.Fatalf("Expected tags payload to be present")
	}

	var tags []string
	for _, v := range tagVals.GetListValue().Values {
		tags = append(tags, v.GetStringValue())
	}

	expectedTags := []string{"test", "service", "decoder", "dtc", "csharp", "application", "xunit"}
	for _, expected := range expectedTags {
		if !containsString(tags, expected) {
			t.Fatalf("Expected tag %q in payload tags %v", expected, tags)
		}
	}
}

func TestSyncFileState_AddsFrameworkTagsForOtherLanguages(t *testing.T) {
	testCases := []struct {
		name         string
		relPath      string
		content      string
		expectedTags []string
	}{
		{
			name:         "Go",
			relPath:      "internal/api/user_handler_test.go",
			content:      "package api\n\nimport (\n\t\"github.com/gin-gonic/gin\"\n\t\"github.com/stretchr/testify/require\"\n)\n\nfunc TestHandler() {}\n",
			expectedTags: []string{"go", "gin", "testify", "test", "api", "handler"},
		},
		{
			name:         "TypeScript",
			relPath:      "web/src/components/UserList.spec.ts",
			content:      "import React from 'react'\nimport { describe, it } from 'vitest'\n\nexport function UserList() { return <div /> }\n",
			expectedTags: []string{"typescript", "react", "vitest", "test", "web"},
		},
		{
			name:         "Python",
			relPath:      "services/orders/test_api.py",
			content:      "from fastapi import FastAPI\nimport pytest\n\ndef test_orders_api():\n    assert True\n",
			expectedTags: []string{"python", "fastapi", "pytest", "test", "service", "api"},
		},
		{
			name:         "PHP",
			relPath:      "app/Infrastructure/UserRepositoryTest.php",
			content:      "<?php\nuse PHPUnit\\Framework\\TestCase;\nuse Doctrine\\ORM\\EntityManager;\nclass UserRepositoryTest extends TestCase {}\n",
			expectedTags: []string{"php", "phpunit", "doctrine", "test", "repository", "infrastructure"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rootDir := t.TempDir()
			filePath := filepath.Join(rootDir, filepath.FromSlash(tc.relPath))
			if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filePath, []byte(tc.content), 0644); err != nil {
				t.Fatal(err)
			}

			mockClient := &MockQdrantClient{}
			worker := newTestWorker(rootDir, mockClient)
			defer worker.Close()

			worker.SyncFileState(context.Background(), filePath)
			worker.BatchUpserter.Flush()

			tags := payloadTagsFromFirstPoint(t, mockClient)
			for _, expected := range tc.expectedTags {
				if !containsString(tags, expected) {
					t.Fatalf("Expected tag %q in payload tags %v", expected, tags)
				}
			}
		})
	}
}

func TestExecuteVectorSearch_ReranksUsingTags(t *testing.T) {
	mockClient := &MockQdrantClient{
		queryResp: []*qdrant.ScoredPoint{
			{
				Score: 0.92,
				Payload: qdrant.NewValueMap(map[string]interface{}{
					"file_path":     "/repo/src/DiagnosticsController.cs",
					"relative_path": "src/DiagnosticsController.cs",
					"content":       "public class DiagnosticsController {}",
					"type":          "function",
					"name":          "GetStatus",
					"updated":       int64(1716570000),
					"tags":          []interface{}{"controller", "diagnostics", "status"},
				}),
			},
			{
				Score: 0.61,
				Payload: qdrant.NewValueMap(map[string]interface{}{
					"file_path":     "/repo/tests/AgroOps.Application.Tests/DtcFrameDecoderServiceTests.cs",
					"relative_path": "tests/AgroOps.Application.Tests/DtcFrameDecoderServiceTests.cs",
					"content":       "public class DtcFrameDecoderServiceTests {}",
					"type":          "function",
					"name":          "DecodeFrame_ShouldReturnExpectedResult",
					"updated":       int64(1716570001),
					"tags":          []interface{}{"test", "service", "decoder", "dtc", "csharp"},
				}),
			},
		},
	}

	worker := server.NewIngestionWorker(server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      "/repo",
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		ParserMode:          "full",
		MaxEmbeddingWorkers: 1,
		SearchMode:          "hybrid",
	}, mockClient, nil)
	defer worker.Close()

	worker.HTTPClient.Transport = &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			respBody := `{"embedding": [0.1, 0.2, 0.3]}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}

	result, err := worker.ExecuteVectorSearch(context.Background(), "dtc frame decoder service tests", nil, "tests")
	if err != nil {
		t.Fatalf("ExecuteVectorSearch returned error: %v", err)
	}

	firstDiagnostics := strings.Index(result, "DiagnosticsController")
	firstDecoderTest := strings.Index(result, "DtcFrameDecoderServiceTests.cs")
	if firstDecoderTest == -1 {
		t.Fatalf("Expected test file in search results, got: %s", result)
	}
	if firstDiagnostics != -1 && firstDiagnostics < firstDecoderTest {
		t.Fatalf("Expected decoder test result to rank ahead of diagnostics controller. Result:\n%s", result)
	}
}

func containsString(slice []string, target string) bool {
	for _, item := range slice {
		if item == target {
			return true
		}
	}
	return false
}

func newTestWorker(rootDir string, mockClient *MockQdrantClient) *server.IngestionWorker {
	worker := server.NewIngestionWorker(server.Config{
		CollectionName:      "test-collection",
		WatchDirectory:      rootDir,
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		ParserMode:          "full",
		MaxEmbeddingWorkers: 1,
		BatchSize:           1,
		BatchTimeout:        time.Second,
	}, mockClient, nil)

	worker.HTTPClient.Transport = &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			respBody := `{"embedding": [0.1, 0.2, 0.3]}`
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(respBody)),
				Header:     make(http.Header),
			}, nil
		},
	}
	return worker
}

func payloadTagsFromFirstPoint(t *testing.T, mockClient *MockQdrantClient) []string {
	t.Helper()

	mockClient.mu.Lock()
	defer mockClient.mu.Unlock()
	if len(mockClient.upsertCalls) == 0 || len(mockClient.upsertCalls[0]) == 0 {
		t.Fatalf("Expected at least one upserted point")
	}

	payload := mockClient.upsertCalls[0][0].Payload
	tagVals, ok := payload["tags"]
	if !ok {
		t.Fatalf("Expected tags payload to be present")
	}

	var tags []string
	for _, v := range tagVals.GetListValue().Values {
		tags = append(tags, v.GetStringValue())
	}
	return tags
}

func TestMockQdrantClient_SetPayload(t *testing.T) {
	mock := &MockQdrantClient{}
	ctx := context.Background()
	_, err := mock.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: "test",
		Payload:        map[string]*qdrant.Value{"branch": qdrant.NewValueString("main")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mock.mu.Lock()
	count := len(mock.setPayloadCalls)
	mock.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 SetPayload call, got %d", count)
	}
}

func TestSyncFileState_BranchPayload(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "branch_payload_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Write([]byte("hello branch"))
	tmpFile.Close()

	mock := &MockQdrantClient{}
	cfg := server.Config{
		CollectionName:      "test",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		BatchSize:           1,
		BatchTimeout:        1 * time.Second,
		Branch:              "feature/test",
		DefaultBranch:       "main",
	}
	worker := server.NewIngestionWorker(cfg, mock, nil)
	defer worker.Close()

	mockHTTP := &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"embedding":[0.1,0.2,0.3]}`)),
				Header:     make(http.Header),
			}, nil
		},
	}
	worker.HTTPClient.Transport = mockHTTP

	worker.SyncFileState(context.Background(), tmpFile.Name())
	worker.BatchUpserter.Flush()

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.upsertCalls) == 0 {
		t.Fatal("expected upsert calls, got none")
	}
	for _, batch := range mock.upsertCalls {
		for _, point := range batch {
			branch := point.Payload["branch"].GetStringValue()
			if branch != "feature/test" {
				t.Fatalf("expected branch 'feature/test', got %q", branch)
			}
			defBranch := point.Payload["default_branch"].GetStringValue()
			if defBranch != "main" {
				t.Fatalf("expected default_branch 'main', got %q", defBranch)
			}
		}
	}
}

func TestPurgeFileVectors_CompoundFilter(t *testing.T) {
	mock := &MockQdrantClient{}
	cfg := server.Config{
		CollectionName: "test",
		WatchDirectory: os.TempDir(),
		Branch:         "feature/test",
		DefaultBranch:  "main",
	}
	worker := server.NewIngestionWorker(cfg, mock, nil)
	defer worker.Close()

	worker.SyncFileState(context.Background(), "/nonexistent/path/gone.go")

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.deletePointsCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mock.deletePointsCalls))
	}
	filter := mock.deletePointsCalls[0].Points.GetFilter()
	if len(filter.Must) != 2 {
		t.Fatalf("expected 2 Must conditions in purge filter, got %d", len(filter.Must))
	}
}

func TestSyncWorkspace_MigratesLegacyVectors(t *testing.T) {
	dir := t.TempDir()
	mock := &MockQdrantClient{}
	cfg := server.Config{
		WatchDirectory:      dir,
		CollectionName:      "test",
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		BatchSize:           10,
		BatchTimeout:        1 * time.Second,
		Branch:              "main",
		DefaultBranch:       "main",
	}
	worker := server.NewIngestionWorker(cfg, mock, nil)
	defer worker.Close()

	_, err := worker.SyncWorkspace(context.Background())
	if err != nil {
		t.Fatalf("SyncWorkspace failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.setPayloadCalls) == 0 {
		t.Fatal("expected SetPayload migration call, got none")
	}
	p := mock.setPayloadCalls[0].Payload
	if p["branch"].GetStringValue() != "main" {
		t.Fatalf("expected migrated branch 'main', got %q", p["branch"].GetStringValue())
	}
	if p["default_branch"].GetStringValue() != "main" {
		t.Fatalf("expected migrated default_branch 'main', got %q", p["default_branch"].GetStringValue())
	}
}
