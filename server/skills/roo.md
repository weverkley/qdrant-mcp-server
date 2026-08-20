# Roo / Zoo Code Agent Skill - Qdrant RAG MCP Server Integration

This repository is integrated with the **Go Qdrant-RAG MCP Server**, which indexes the workspace in real-time into a Qdrant vector database using Ollama embeddings. As a Roo Code or Zoo Code agent, you have direct access to semantic search across this repository via the shared project RAG MCP tools.

Roo Code and Zoo Code are MCP clients. The knowledge base (collection, embeddings, AST index, branch metadata) is client-independent — do not assume a separate per-client collection.

---

## Available MCP Tools

1. `qdrant_search`
   - **Description**: Performs a semantic, vector-based search across the codebase.
   - **Arguments**: `query` (string, required) - The natural language query or concept.
2. `get_sync_status`
   - **Description**: Checks the real-time status of the codebase indexing pipeline.

---

## Guidelines & Best Practices

### 1. Query project RAG before broad repository exploration
- Prefer `qdrant_search` to locate relevant code, docs, and specs before walking the full tree or opening many unrelated files.
- Use RAG as navigation and context, not as an authoritative source of truth.

### 2. Locate relevant code, docs, and specs with RAG
- Search for implementations, helpers, and design docs with semantic queries.
- When changing architecture or behavior, search specs and documentation (including Markdown and Gherkin when indexed) before editing.

### 3. Open actual filesystem files before editing
- After RAG points you to a path, read the real file from disk before proposing or applying edits.
- Treat Git and the filesystem as the source of truth; indexed snippets may be stale or incomplete.

### 4. Prefer current branch context
- Prefer results and files that match the current working tree / branch context when available.
- Do not invent or switch collections for a specific editor client.

### 5. Avoid unnecessary full-repository scans
- When `qdrant_search` returns precise, high-confidence hits, open those files and proceed.
- Fall back to broader exploration only when RAG results are empty, weak, or clearly off-target.

### 6. Formulate semantic queries (not bare keywords)
- Bad: `jwt`, `qdrant config`, `fsnotify`
- Good:
  - `"How does the JWT token parsing middleware validate custom claims?"`
  - `"Find the configuration loading structure for Qdrant connections"`
  - `"Explain the debounce logic inside the fsnotify file ingestion consumer loop"`

### 7. Verify codebase sync status when needed
- After large updates or new folders, run `get_sync_status` before relying on search results.
