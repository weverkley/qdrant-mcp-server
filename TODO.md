# 📋 Go Qdrant-RAG MCP Server Roadmap & TODO

This document outlines the planned improvements, architectural enhancements, and functional updates to make this real-time RAG server highly robust, scalable, and intelligent.

---

## ⚡ Performance & Ingestion Concurrency

- [ ] **Rate-Limited Embedding Worker Pool**
  - Implement a buffered worker pool (e.g., maximum concurrent workers controlled by a `MAX_EMBEDDING_WORKERS` environment variable).
  - Prevent local Ollama engine thrashing and timeout errors during bulk changes (like git branch switches or initial indexing).
- [ ] **Initial Startup Synchronization Crawler**
  - Read files on startup and verify their status.
  - Compare local file modification times (`mtime`) or content hashes with existing points in Qdrant.
  - Automatically index newly created or modified files, and purge deleted paths *before* opening the filesystem notification loop.

---

## 🧠 Semantic & AST-Aware Chunking

- [x] **Block & Structure-Aware Parser**
  - Replace the basic 1000-character line accumulator with a parser that respects semantic boundaries.
  - Support syntax parsing blocks (e.g., class/function boundaries for Go, C#, JS/TS, Python) so that logical segments are not cut in half.
  - Handle Markdown structure (headers, list bounds) when indexing directories like `.codex` or `.obsidian`.

---

## 🔍 Advanced Querying & Filtering

- [ ] **Metadata-Driven Filter Arguments**
  - Enhance the `qdrant_search` tool parameters to accept optional filtering parameters:
    * `file_extensions` (e.g., limit queries to `.go` or `.cs`).
    * `path_prefix` (e.g., search strictly inside a `src/auth/` directory).
  - Use Qdrant's high-speed payload keyword matching filters.
- [ ] **Hybrid Search (Dense + Sparse)**
  - Combine semantic dense vector search (Ollama embeddings) with sparse vector representations (like BM25).
  - Yield perfect search results for both high-level concepts and exact keyword matches (like specific variables, function names, or exact error codes).

---

## 🏷️ Rich Metadata & Precision Navigation

- [x] **Positional Payload Fields**
  - Extract and save precise positional indexes for code chunks in the payload:
    * `start_line` / `end_line`: Allow the LLM client to direct the user to the exact lines matching the chunk.
    * `language`: Store explicit languages for quick client UI rendering filters.
    * `content_hash`: Compare SHA1 checksums of file contents to skip re-indexing if no change has actually occurred.

---

## 🔒 Production, Security & Resiliency

- [ ] **Authenticated Qdrant Cloud Integration**
  - Support TLS/HTTPS connections.
  - Handle `QDRANT_API_KEY` configurations to connect safely to hosted Qdrant Cloud nodes.
- [ ] **Exponential Backoff Connection Retries**
  - Wrap connection handshakes in a retry engine with dynamic fallback to automatically reconnect if Qdrant or Ollama drops out or restarts.

---

## 📈 Developer Experience & MCP Capabilities

- [x] **Dynamic Progress Reporting**
  - Expose a simple status query tool (e.g. `get_sync_status`) so users can check if the workspace is fully indexed.
- [ ] **Server Command Line Interface**
  - Support execution flags (e.g., `--verbose`, `--dry-run`, or `--port`) when launching the binary directly from terminal setups.
