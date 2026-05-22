# GitHub Copilot instructions - Qdrant RAG MCP Server Integration

This repository is integrated with the **Go Qdrant-RAG MCP Server** which indexes the workspace in real-time into a Qdrant vector database using Ollama embeddings. As the GitHub Copilot AI, you have access to semantic search tools when requested.

---

## 🛠️ Available MCP Tools

1. `qdrant_search`
   - **Description**: Performs a semantic, vector-based search across the codebase.
   - **Arguments**: `query` (string, required) - The natural language query or concept.
2. `get_sync_status`
   - **Description**: Checks the real-time status of the codebase indexing pipeline.

---

## 💡 Guidelines & Best Practices for Tool Usage

### 1. Search Before You Build or Declare Missing
- **Avoid Duplication**: Before writing a new utility, middleware, database logic, or component, always execute a semantic search via `qdrant_search` to check if a similar or helper implementation already exists.
- **Verification**: If you believe a feature, package, or function is missing, run a semantic query first to confirm. Do not assume something does not exist based only on visible tree structures.

### 2. Formulate Semantic Queries (Not Keywords)
- Since the vector search uses dense embeddings (`Ollama`), keyword matching is secondary.
- **Bad Queries**: `jwt`, `qdrant config`, `fsnotify`
- **Good Queries**: 
  - `"How does the JWT token parsing middleware validate custom claims?"`
  - `"Find the configuration loading structure for Qdrant connections"`
  - `"Explain the debounce logic inside the fsnotify file ingestion consumer loop"`

### 3. Verify Codebase Sync Status
- If you or the user have recently made extensive updates or added large folders of files, run `get_sync_status` to ensure the indexing queue is empty and all files have been successfully ingested before performing search queries.

### 4. Search in Developer Codex/Notes
- If the project includes a documentation directory like `.codex` or `.obsidian`, these developer guides are indexed! Search them for architectural standards, logging guidelines, or feature specifications.
