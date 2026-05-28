# Multi-Branch Support Design

**Date:** 2026-05-28
**Status:** Approved

## Problem

When working across multiple machines on different git branches sharing the same Qdrant collection, file ingestion overwrites vectors from other branches — corrupting data. A full re-ingest is triggered on every machine switch because there is no branch isolation.

## Goals

- One shared Qdrant collection supports multiple branches simultaneously
- `--ingest` and the file watcher tag vectors with the real branch name
- MCP search accepts an optional `branch` parameter; when provided, results prioritize branch-specific vectors and fall back to default-branch vectors for files not modified on that branch
- Old data (no branch field) is migrated automatically on `--ingest`

---

## Data Model

Every vector payload gains two new fields:

| Field | Type | Example | Description |
|---|---|---|---|
| `branch` | string | `"feature/improvements"` | Branch this vector was indexed on |
| `default_branch` | string | `"main"` | Repo default branch, used for fallback |

The compound dedup/purge key becomes `(relative_path, branch)`. Vectors for the same file on different branches coexist independently.

---

## Branch Detection

A utility function `detectBranches(dir string) (currentBranch, defaultBranch string)` runs at worker startup and on `--ingest`.

**Current branch:**
```
git -C <WatchDirectory> rev-parse --abbrev-ref HEAD
```

**Default branch** (in order of precedence):
1. `git -C <WatchDirectory> symbolic-ref refs/remotes/origin/HEAD` → parse last path segment
2. Check if local branch `main` exists → return `"main"`
3. Check if local branch `master` exists → return `"master"`
4. Fall back to current branch

Both values are stored in `Config.Branch` and `Config.DefaultBranch` at startup.

### Ingestion modes

| Mode | `Config.Branch` value |
|---|---|
| `--ingest` | current git branch (e.g. `"main"`) |
| Watcher, on default branch | current git branch (e.g. `"main"`) |
| Watcher, on feature branch | current git branch (e.g. `"feature/improvements"`) |

There are no special sentinels — every vector always carries a real branch name.

---

## Config Changes

`server/config.go` — add two fields to `Config`:

```go
Branch        string  // current git branch, auto-detected at startup
DefaultBranch string  // repo default branch, auto-detected at startup
```

No env var override needed; both are derived from git. If git is unavailable (not a git repo), both default to `"main"`.

---

## Ingestion Changes (`server/worker.go`)

### `SyncFileState`

1. Dedup scroll filter: `Must: [relative_path == relPath, branch == cfg.Branch]`
2. `purgeFileVectors`: filter `Must: [relative_path == relPath, branch == cfg.Branch]`
3. All payloads: add `"branch": cfg.Branch` and `"default_branch": cfg.DefaultBranch`

### `SyncWorkspace` — Migration Pass

Before the file crawl, run a migration pass to update old vectors:

1. Scroll all vectors where `branch` field is absent or empty string (paginated, batch size 100)
2. For each batch, call Qdrant `SetPayload` to add:
   - `"branch": cfg.Branch`
   - `"default_branch": cfg.DefaultBranch`
3. Log progress: `"Migrated N legacy vectors to branch=<branch>"`
4. Then proceed with normal crawl

Running `--ingest` once per machine after upgrading is sufficient to migrate all existing data.

---

## MCP Search Tool Changes (`server/worker.go` / `server/mcp.go`)

### New `branch` parameter

The MCP search tool gains an optional `branch` string parameter. When provided:

**Pass 1** — Branch-specific search:
- Execute vector search with additional filter `branch == <provided_branch>`
- Collect top N results, record the `relative_path` of each result
- Read `default_branch` from any result payload (all vectors carry it)

**Pass 2** — Fallback search:
- Execute the same vector search with filter `branch == <default_branch>`
- From these results, keep only those whose `relative_path` was NOT already covered by Pass 1

**Merge:**
- Combine Pass 1 results + uncovered Pass 2 results
- Re-rank the merged set normally
- Return final results

When `branch` is omitted, the search runs without any branch filter (existing behavior).

---

## Search Scoring

`boostedResultScore` gets a small boost for results whose `branch` matches the requested branch, ensuring branch-specific results rank above base results when both happen to appear in the same result window.

---

## Backward Compatibility

- Old vectors (no `branch` field) are migrated on first `--ingest` run
- Search without `branch` parameter continues to work as before
- `purgeFileVectors` now scoped to `(relative_path, branch)` — a one-time re-ingest on each machine ensures old orphan vectors are cleaned up and re-tagged

---

## Out of Scope

- Automatic branch cleanup when a branch is merged/deleted (manual collection management or future work)
- Branch auto-detection at MCP search time (user passes branch explicitly)
- Per-branch collection routing
