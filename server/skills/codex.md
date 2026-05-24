# Codex Knowledge Base - Qdrant RAG MCP Integration

This Codex/documentation directory is recursively monitored and indexed in real-time by the **Go Qdrant-RAG MCP Server**. 

As an AI assistant reading this workspace, you have direct access to semantic search capabilities across both this Codex and the broader codebase.

---

## 🛠️ Available MCP Tools

1. `qdrant_search`
   - **Description**: Performs a semantic, vector-based search across the codebase and Codex docs.
   - **Arguments**: `query` (string, required) - The natural language query or concept.
2. `get_sync_status`
   - **Description**: Checks the real-time status of the codebase and Codex indexing pipeline.

---

## 💡 How to Query the Codex

When you need to look up documentation, guidelines, specifications, or standards, use the `qdrant_search` tool with a semantic natural language query.

### Examples:
- To find architecture specifications: 
  `qdrant_search("What is the database connection strategy documented in the wiki?")`
- To find team styling guides: 
  `qdrant_search("Find the guidelines for writing telemetry logs")`
- To find feature requirements: 
  `qdrant_search("How should the new user-onboarding flows behave according to Codex specs?")`

### Best Practices:
- Keep queries descriptive and phrased as complete questions or conceptual statements.
- Before claiming a document or standard does not exist in the Codex, always run a search via `qdrant_search` to verify.
