package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qdrant/go-client/qdrant"
)

type MockQdrantClient struct {
	mu          sync.Mutex
	upsertCalls [][]*qdrant.PointStruct
	upsertErr   error
}

func (m *MockQdrantClient) Upsert(ctx context.Context, in *qdrant.UpsertPoints) (*qdrant.UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertCalls = append(m.upsertCalls, in.Points)
	return &qdrant.UpdateResult{}, m.upsertErr
}

func (m *MockQdrantClient) Delete(ctx context.Context, in *qdrant.DeletePoints) (*qdrant.UpdateResult, error) {
	return nil, nil
}

func (m *MockQdrantClient) CollectionExists(ctx context.Context, collectionName string) (bool, error) {
	return true, nil
}

func (m *MockQdrantClient) CreateCollection(ctx context.Context, in *qdrant.CreateCollection) error {
	return nil
}

func (m *MockQdrantClient) Query(ctx context.Context, in *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	return nil, nil
}

func TestBatchUpserter_BatchSizeTrigger(t *testing.T) {
	mockClient := &MockQdrantClient{}
	batchSize := 3
	timeout := 10 * time.Second // large timeout so it doesn't trigger by timeout
	b := NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
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
	b := NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
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
	b := NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
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
	b := NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)
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
	b := NewBatchUpserter(mockClient, "test-collection", batchSize, timeout)

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
