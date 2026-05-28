# Multi-Branch Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store vectors tagged with a real branch name, deduplicate per `(relative_path, branch)`, migrate old data on `--ingest`, and add a `branch` parameter to the MCP search tool that prioritises branch-specific results with automatic fallback to the default branch.

**Architecture:** A new `server/branch.go` file detects current and default git branches via shell commands. `Config` gains `Branch` and `DefaultBranch` fields set at startup. Every vector payload carries `branch` and `default_branch` fields. `SyncWorkspace` runs a migration pass (`SetPayload` via filter) before crawling. `ExecuteVectorSearch` accepts an optional branch string and performs a two-pass search when provided.

**Tech Stack:** Go 1.21+, `qdrant/go-client v1.18.1`, `os/exec` for git commands, existing test patterns in `tests/worker_test.go` (mock HTTP via `MockRoundTripper`, `worker.HTTPClient.Transport`).

---

### Task 1: Branch detection utility

**Files:**
- Create: `server/branch.go`
- Create: `tests/branch_test.go`

- [ ] **Step 1: Write the failing tests**

Create `tests/branch_test.go`:

```go
package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"qdrant-mcp-server/server"
)

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"checkout", "-b", "main"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestDetectBranches_CurrentBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	current, _ := server.DetectBranches(dir)
	if current != "main" {
		t.Fatalf("expected current branch 'main', got %q", current)
	}
}

func TestDetectBranches_FeatureBranch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	f := filepath.Join(dir, "a.txt")
	os.WriteFile(f, []byte("x"), 0644)
	exec.Command("git", "-C", dir, "add", ".").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "init").Run()
	exec.Command("git", "-C", dir, "checkout", "-b", "feature/test").Run()

	current, _ := server.DetectBranches(dir)
	if current != "feature/test" {
		t.Fatalf("expected 'feature/test', got %q", current)
	}
}

func TestDetectBranches_FallbackNonGit(t *testing.T) {
	dir := t.TempDir()
	current, def := server.DetectBranches(dir)
	if current != "main" {
		t.Fatalf("expected fallback 'main', got %q", current)
	}
	if def != "main" {
		t.Fatalf("expected fallback default 'main', got %q", def)
	}
}

func TestDetectBranches_DefaultFromLocal(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	_, def := server.DetectBranches(dir)
	if def != "main" {
		t.Fatalf("expected default branch 'main', got %q", def)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestDetectBranches" -v
```

Expected: `FAIL` — `server.DetectBranches` undefined.

- [ ] **Step 3: Implement `server/branch.go`**

```go
package server

import (
	"os/exec"
	"strings"
)

// DetectBranches returns (currentBranch, defaultBranch) for the git repo at dir.
// Falls back to "main" for both if git is unavailable or dir is not a repo.
func DetectBranches(dir string) (string, string) {
	current := gitOutput(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if current == "" || current == "HEAD" {
		current = "main"
	}
	return current, detectDefaultBranch(dir)
}

func detectDefaultBranch(dir string) string {
	ref := gitOutput(dir, "symbolic-ref", "refs/remotes/origin/HEAD")
	if ref != "" {
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	if gitOutput(dir, "rev-parse", "--verify", "main") != "" {
		return "main"
	}
	if gitOutput(dir, "rev-parse", "--verify", "master") != "" {
		return "master"
	}
	return "main"
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestDetectBranches" -v
```

Expected: all 4 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add server/branch.go tests/branch_test.go
git commit -m "feat: add git branch detection utility"
```

---

### Task 2: Add Branch and DefaultBranch to Config

**Files:**
- Modify: `server/config.go`
- Modify: `tests/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tests/config_test.go`:

```go
func TestConfig_BranchFieldsPresent(t *testing.T) {
	cfg := server.Config{}
	_ = cfg.Branch
	_ = cfg.DefaultBranch
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestConfig_BranchFieldsPresent" -v
```

Expected: compile error — `cfg.Branch` undefined.

- [ ] **Step 3: Add fields to Config struct**

In `server/config.go`, add to the `Config` struct after the `MaxFileSize` field:

```go
Branch        string // current git branch, auto-detected at startup
DefaultBranch string // repo default branch, auto-detected at startup
```

- [ ] **Step 4: Populate fields in LoadConfig**

In `server/config.go`, at the end of `LoadConfig()` just before `return cfg`, add:

```go
cfg.Branch, cfg.DefaultBranch = DetectBranches(cfg.WatchDirectory)
log.Printf("Branch context: current=%q default=%q", cfg.Branch, cfg.DefaultBranch)
```

- [ ] **Step 5: Run test to verify it passes**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestConfig_BranchFieldsPresent" -v
```

Expected: PASS.

- [ ] **Step 6: Build check**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./...
```

Expected: Success.

- [ ] **Step 7: Commit**

```bash
git add server/config.go tests/config_test.go
git commit -m "feat: add Branch and DefaultBranch to Config, auto-detected from git"
```

---

### Task 3: Add SetPayload to QdrantClient interface and mock

**Files:**
- Modify: `server/worker.go` (interface only)
- Modify: `tests/worker_test.go` (mock struct + method)

- [ ] **Step 1: Write the failing test**

Append to `tests/worker_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestMockQdrantClient_SetPayload" -v
```

Expected: compile error — `mock.SetPayload` undefined.

- [ ] **Step 3: Add SetPayload to QdrantClient interface**

In `server/worker.go`, add to the `QdrantClient` interface after the `Scroll` line:

```go
SetPayload(ctx context.Context, in *qdrant.SetPayloadPoints) (*qdrant.UpdateResult, error)
```

- [ ] **Step 4: Add setPayloadCalls to MockQdrantClient and implement method**

In `tests/worker_test.go`, add `setPayloadCalls []*qdrant.SetPayloadPoints` to the `MockQdrantClient` struct fields.

Then add the method:

```go
func (m *MockQdrantClient) SetPayload(ctx context.Context, in *qdrant.SetPayloadPoints) (*qdrant.UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setPayloadCalls = append(m.setPayloadCalls, in)
	return &qdrant.UpdateResult{}, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestMockQdrantClient_SetPayload" -v
```

Expected: PASS.

- [ ] **Step 6: Build check**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./...
```

Expected: Success.

- [ ] **Step 7: Commit**

```bash
git add server/worker.go tests/worker_test.go
git commit -m "feat: add SetPayload to QdrantClient interface and mock"
```

---

### Task 4: Add branch fields to payloads and update dedup/purge filters

**Files:**
- Modify: `server/worker.go` (`SyncFileState`, `purgeFileVectors`)
- Modify: `tests/worker_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `tests/worker_test.go`:

```go
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

	// Non-existent file triggers purge
	worker.SyncFileState(context.Background(), "/nonexistent/path/gone.go")

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mock.deleteCalls))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSyncFileState_BranchPayload|TestPurgeFileVectors_CompoundFilter" -v
```

Expected: FAIL — branch/default_branch not in payload.

- [ ] **Step 3: Add branch fields to all three payload maps in SyncFileState**

In `server/worker.go`, find the three `payload := map[string]interface{}{` blocks inside `SyncFileState` (for function chunks, doc_chunks, and plain chunks). Add these two lines to each:

```go
"branch":         iw.Cfg.Branch,
"default_branch": iw.Cfg.DefaultBranch,
```

The function-chunk payload (near the `"file_path": relPath` line) becomes:
```go
payload := map[string]interface{}{
    "file_path":      relPath,
    "content":        chunk,
    "type":           "function",
    "name":           fn.Name,
    "start_line":     int64(fn.StartLine),
    "end_line":       int64(fn.EndLine),
    "language":       fn.Language,
    "extension":      extClean,
    "relative_path":  relPath,
    "relative_dirs":  relDirs,
    "namespace":      firstNonEmpty(fn.Namespace, namespace),
    "container":      fn.Container,
    "symbol_names":   convertStringSlice(pointSymbols),
    "framework_tags": convertStringSlice(frameworkTags),
    "layer_tags":     convertStringSlice(layerTags),
    "tags":           convertStringSlice(pointTags),
    "is_test":        isTestFile,
    "test_framework": testFramework,
    "file_hash":      localHash,
    "modified":       modifiedUnix,
    "updated":        time.Now().Unix(),
    "branch":         iw.Cfg.Branch,
    "default_branch": iw.Cfg.DefaultBranch,
}
```

Apply the same two additions to the `doc_chunk` payload and the plain `chunk` payload.

- [ ] **Step 4: Update dedup scroll to compound key**

In `SyncFileState`, replace the scroll filter from:
```go
Must: []*qdrant.Condition{
    qdrant.NewMatchKeyword("relative_path", relPath),
},
```
to:
```go
Must: []*qdrant.Condition{
    qdrant.NewMatchKeyword("relative_path", relPath),
    qdrant.NewMatchKeyword("branch", iw.Cfg.Branch),
},
```

- [ ] **Step 5: Update purgeFileVectors to compound filter**

Replace the full `purgeFileVectors` function:

```go
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
```

- [ ] **Step 6: Run tests to verify they pass**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSyncFileState_BranchPayload|TestPurgeFileVectors_CompoundFilter|TestSyncFileState_ContentHashing" -v
```

Expected: all PASS.

- [ ] **Step 7: Build check**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./...
```

Expected: Success.

- [ ] **Step 8: Commit**

```bash
git add server/worker.go tests/worker_test.go
git commit -m "feat: add branch/default_branch to vector payloads, compound dedup/purge key"
```

---

### Task 5: Migration pass in SyncWorkspace

**Files:**
- Modify: `server/worker.go` (new `migrateUnbrandedVectors`, updated `SyncWorkspace`)
- Modify: `tests/worker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tests/worker_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSyncWorkspace_MigratesLegacyVectors" -v
```

Expected: FAIL — no `SetPayload` calls.

- [ ] **Step 3: Add migrateUnbrandedVectors method**

In `server/worker.go`, add this method just before `SyncWorkspace`:

```go
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
```

- [ ] **Step 4: Call migration in SyncWorkspace**

In `server/worker.go` in `SyncWorkspace`, after the collection exists/create block and before the `// 2. Discover files` comment, add:

```go
if err := iw.migrateUnbrandedVectors(ctx); err != nil {
	log.Printf("Warning: branch migration failed (non-fatal): %v", err)
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSyncWorkspace_MigratesLegacyVectors" -v
```

Expected: PASS.

- [ ] **Step 6: Build check**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./...
```

Expected: Success.

- [ ] **Step 7: Commit**

```bash
git add server/worker.go tests/worker_test.go
git commit -m "feat: migrate legacy unbranded vectors at SyncWorkspace startup"
```

---

### Task 6: Branch-priority two-pass search

**Files:**
- Modify: `server/worker.go` (`ExecuteVectorSearch`, new `addBranchFilter`)
- Modify: `server/server.go` (update callers)
- Modify: `tests/worker_test.go` (update existing callers + new tests)

- [ ] **Step 1: Add queryRespFn to MockQdrantClient**

In `tests/worker_test.go`, add `queryRespFn func(*qdrant.QueryPoints) []*qdrant.ScoredPoint` to the `MockQdrantClient` struct fields.

Update the `Query` method to use it when set:

```go
func (m *MockQdrantClient) Query(ctx context.Context, in *qdrant.QueryPoints) ([]*qdrant.ScoredPoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queryCalls = append(m.queryCalls, in)
	if m.queryRespFn != nil {
		return m.queryRespFn(in), m.queryErr
	}
	return m.queryResp, m.queryErr
}
```

- [ ] **Step 2: Write the failing tests**

Append to `tests/worker_test.go`:

```go
func TestExecuteVectorSearch_BranchPriority(t *testing.T) {
	branchPoint := &qdrant.ScoredPoint{
		Id:    qdrant.NewIDNum(1),
		Score: 0.9,
		Payload: map[string]*qdrant.Value{
			"file_path":      qdrant.NewValueString("src/ast.go"),
			"relative_path":  qdrant.NewValueString("src/ast.go"),
			"content":        qdrant.NewValueString("branch version of ast"),
			"type":           qdrant.NewValueString("chunk"),
			"branch":         qdrant.NewValueString("feature/test"),
			"default_branch": qdrant.NewValueString("main"),
		},
	}
	basePoint := &qdrant.ScoredPoint{
		Id:    qdrant.NewIDNum(2),
		Score: 0.85,
		Payload: map[string]*qdrant.Value{
			"file_path":      qdrant.NewValueString("src/server.go"),
			"relative_path":  qdrant.NewValueString("src/server.go"),
			"content":        qdrant.NewValueString("base version of server"),
			"type":           qdrant.NewValueString("chunk"),
			"branch":         qdrant.NewValueString("main"),
			"default_branch": qdrant.NewValueString("main"),
		},
	}

	mock := &MockQdrantClient{}
	mock.queryRespFn = func(in *qdrant.QueryPoints) []*qdrant.ScoredPoint {
		// Return branch-specific results when branch filter is feature/test
		for _, cond := range in.GetFilter().GetMust() {
			if kw := cond.GetFieldCondition().GetMatch().GetKeyword(); kw == "feature/test" {
				return []*qdrant.ScoredPoint{branchPoint}
			}
		}
		return []*qdrant.ScoredPoint{basePoint}
	}

	cfg := server.Config{
		CollectionName:      "test",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		SearchMode:          "dense",
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

	result, err := worker.ExecuteVectorSearch(context.Background(), "ast parsing", nil, "", "feature/test")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if !strings.Contains(result, "branch version of ast") {
		t.Error("expected branch-specific result in output")
	}
	if !strings.Contains(result, "base version of server") {
		t.Error("expected base fallback result for uncovered file")
	}
	// src/ast.go should appear exactly once (not duplicated from both passes)
	if strings.Count(result, "src/ast.go") > 1 {
		t.Error("src/ast.go appeared more than once — branch dedup failed")
	}
}

func TestExecuteVectorSearch_NoBranchNoFilter(t *testing.T) {
	mock := &MockQdrantClient{queryResp: []*qdrant.ScoredPoint{}}
	cfg := server.Config{
		CollectionName:      "test",
		WatchDirectory:      os.TempDir(),
		OllamaHost:          "http://localhost:11434",
		EmbeddingModel:      "nomic-embed-text",
		MaxEmbeddingWorkers: 1,
		SearchMode:          "dense",
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

	_, err := worker.ExecuteVectorSearch(context.Background(), "test query", nil, "", "")
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	for _, call := range mock.queryCalls {
		if call.Filter == nil {
			continue
		}
		for _, cond := range call.Filter.Must {
			kw := cond.GetFieldCondition().GetMatch().GetKeyword()
			if kw == "main" || kw == "feature/test" {
				t.Errorf("unexpected branch filter in no-branch search: %q", kw)
			}
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestExecuteVectorSearch_BranchPriority|TestExecuteVectorSearch_NoBranchNoFilter" -v
```

Expected: compile error — `ExecuteVectorSearch` has wrong number of arguments.

- [ ] **Step 4: Update ExecuteVectorSearch signature**

In `server/worker.go`, change the function signature from:
```go
func (iw *IngestionWorker) ExecuteVectorSearch(ctx context.Context, query string, fileExtensions []string, pathPrefix string) (string, error) {
```
to:
```go
func (iw *IngestionWorker) ExecuteVectorSearch(ctx context.Context, query string, fileExtensions []string, pathPrefix string, branch string) (string, error) {
```

- [ ] **Step 5: Add addBranchFilter helper**

In `server/worker.go`, add this function near `buildSearchFilter`:

```go
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
```

- [ ] **Step 6: Replace the body of ExecuteVectorSearch**

Replace the full body with:

```go
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
			pageVal := payloadInt(payloadMap, "page_number")
			if pageVal > 0 {
				sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk (Page/Section %d) in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, pageVal, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
			} else {
				sb.WriteString(fmt.Sprintf("#### [%d] Doc Chunk in %s [branch: %s] (Match Score: %.2f | Last Synced: %s)\n", i+1, filePath, resultBranch, rankedPoint.Score, lastSyncedStr))
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
```

- [ ] **Step 7: Update all callers of ExecuteVectorSearch to pass empty branch**

In `server/server.go`, update the two call sites:

```go
// search command (~line 91):
results, err := worker.ExecuteVectorSearch(context.Background(), query, nil, "", "")

// evaluate-search command (~line 110):
result, err := worker.ExecuteVectorSearch(context.Background(), suite.Query, suite.FileExtensions, suite.PathPrefix, "")
```

In `server/mcp.go`, the qdrant_search handler (~line 147) — leave as `""` for now; Task 7 wires the branch arg:
```go
resultsText, err := iw.ExecuteVectorSearch(context.Background(), args.Query, args.FileExtensions, args.PathPrefix, "")
```

In `tests/worker_test.go`, update the three existing `ExecuteVectorSearch` calls in `TestExecuteVectorSearch_SearchModes` (lines ~591, ~611, ~631) to add the fifth empty-string argument:
```go
_, _ = worker.ExecuteVectorSearch(ctx, "test query", nil, "", "")
```

- [ ] **Step 8: Run tests to verify they pass**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestExecuteVectorSearch" -v
```

Expected: all PASS.

- [ ] **Step 9: Build check**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./...
```

Expected: Success.

- [ ] **Step 10: Commit**

```bash
git add server/worker.go server/server.go tests/worker_test.go
git commit -m "feat: branch-priority two-pass search in ExecuteVectorSearch"
```

---

### Task 7: Wire branch parameter in MCP search tool

**Files:**
- Modify: `server/mcp.go`
- Modify: `tests/worker_test.go`

- [ ] **Step 1: Write the failing test**

Append to `tests/worker_test.go`:

```go
func TestSearchArguments_BranchField(t *testing.T) {
	raw := `{"query":"test","branch":"feature/test"}`
	var args server.SearchArguments
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if args.Branch != "feature/test" {
		t.Fatalf("expected branch 'feature/test', got %q", args.Branch)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSearchArguments_BranchField" -v
```

Expected: compile error — `args.Branch` undefined.

- [ ] **Step 3: Add Branch to SearchArguments**

In `server/mcp.go`, update `SearchArguments`:

```go
type SearchArguments struct {
	Query          string   `json:"query"`
	FileExtensions []string `json:"file_extensions,omitempty"`
	PathPrefix     string   `json:"path_prefix,omitempty"`
	Branch         string   `json:"branch,omitempty"`
}
```

- [ ] **Step 4: Add branch to tools/list schema**

In `server/mcp.go`, inside the `qdrant_search` tool's `inputSchema.properties` map, add:

```go
"branch": map[string]interface{}{
    "type":        "string",
    "description": "Optional git branch name for branch-priority search (e.g. 'feature/improvements'). Results from this branch take priority; files not modified on this branch fall back to the default branch.",
},
```

- [ ] **Step 5: Pass Branch to ExecuteVectorSearch**

In `server/mcp.go`, update the search handler call from:
```go
resultsText, err := iw.ExecuteVectorSearch(context.Background(), args.Query, args.FileExtensions, args.PathPrefix, "")
```
to:
```go
resultsText, err := iw.ExecuteVectorSearch(context.Background(), args.Query, args.FileExtensions, args.PathPrefix, args.Branch)
```

- [ ] **Step 6: Run tests to verify they pass**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./tests/ -run "TestSearchArguments_BranchField" -v
```

Expected: PASS.

- [ ] **Step 7: Full test suite and build**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build ./... && go test ./...
```

Expected: all tests PASS, build success.

- [ ] **Step 8: Commit**

```bash
git add server/mcp.go tests/worker_test.go
git commit -m "feat: add branch parameter to MCP qdrant_search tool"
```

---

### Task 8: Final verification

- [ ] **Step 1: Run full test suite with verbose output**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go test ./... -v 2>&1 | grep -E "^(=== RUN|--- PASS|--- FAIL|FAIL|ok)"
```

Expected: all `--- PASS`, no `--- FAIL`.

- [ ] **Step 2: Verify binary builds cleanly**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && go build -o /tmp/qdrant-mcp-server-test . && echo "Build OK"
```

Expected: `Build OK`.

- [ ] **Step 3: Confirm branch detection works on this repo**

```bash
cd /home/weverkley/Documents/qdrant-mcp-server && /tmp/qdrant-mcp-server-test --help 2>&1 | head -3
```

Expected: help text printed, no crash.
