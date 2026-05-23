# Go Qdrant-RAG MCP Server

[![Go Version](https://img.shields.io/github/go-mod/go-version/weverkley/qdrant-mcp-server?color=00ADD8)](https://golang.org)
[![Model Context Protocol](https://img.shields.io/badge/MCP-Protocol--2024--11--05-blueviolet)](https://modelcontextprotocol.io)
[![Qdrant](https://img.shields.io/badge/VectorDB-Qdrant-red)](https://qdrant.tech)

A high-performance **Model Context Protocol (MCP)** server written in Go that acts as a real-time Retrieval-Augmented Generation (RAG) agent for your codebases. 

This server recursively monitors your local files, auto-indexes changes in real-time using **Ollama** embeddings, stores them in a remote/local **Qdrant** vector database, and exposes a semantic vector search tool (`qdrant_search`) to your AI assistants (like Claude Desktop, Cursor, Windsurf, or Zed).

---

## 🏗️ Architecture

The server consists of two decoupled systems running concurrently:

```mermaid
graph TD
    %% Filesystem Ingestion Stream
    subgraph Ingestion ["Filesystem Real-time Ingestion"]
        A[Local Filesystem] -->|fsnotify Events| B[Debounce Queue - 800ms]
        B -->|Read Changed File| C{Is Code File?}
        C -->|Yes| C1[Parse AST Functions & Imports]
        C -->|No| C2[Chunk Text - 1000 char blocks]
        C1 -->|Function Signatures/Bodies| D[Ollama API]
        C2 -->|Raw Text Chunks| D
        D -->|Vector Embeddings| E[Qdrant gRPC client]
        E -->|gRPC Upsert / Delete| F[(Qdrant Vector Database)]
    end

    %% MCP Query Stream
    subgraph Query ["MCP Semantic Query Loop"]
        G[MCP Client e.g., Claude/Cursor] -->|JSON-RPC tools/call| H[MCP Server main]
        H -->|Generate Embed Query| D
        D -->|Vector Embedding| H
        H -->|gRPC Vector Query| F
        F -->|Top 5 Context Snippets| H
        H -->|Markdown Response| G
    end

    classDef default fill:#1e1e24,stroke:#3a3f58,color:#e2e8f0;
    classDef database fill:#2b223c,stroke:#634b8c,color:#e2e8f0;
    classDef client fill:#172e2d,stroke:#2b5e5a,color:#e2e8f0;
    class F database;
    class G,A client;
```

---

## ✨ Key Features

- **🧠 AST-Aware Code Intelligence:** Uses tree-sitter AST parsers for Go, JavaScript, TypeScript, PHP, C#, and Python to extract and embed precise function blocks, capturing receivers, signatures, and exact line maps (`start_line`/`end_line`) for deep semantic code searching.
- **⚡ Real-Time Indexing:** Uses OS-level file notifications (`fsnotify`) to watch your code workspace recursively. Any write, create, or delete operation immediately reflects in your vector database.
- **🛡️ Intelligent Ignoring & Filters:** Automatically avoids indexing large directories (like `node_modules` or `.git`) and temporary files. Includes configuration parameters to strictly exclude specific folders or whitelist particular hidden directories.
- **⏱️ Debounced Processing:** Features a configurable debounce duration (defaulting to 800ms) to ensure file saving sequences or git pulls do not thrash system/network resources.
- **🧠 Local Embeddings:** Harnesses **Ollama** embeddings (`/api/embeddings`) for localized, high-speed, and secure code representation.
- **⚡ Supercharged gRPC Storage:** Communicates with your **Qdrant** instance using native Go gRPC clients for ultra-low latency index operations.
- **🤖 Protocol Compliant:** Implements the latest **Model Context Protocol** spec. Keeps all internal execution logs redirected to `stderr` so that stdout is strictly reserved for clean JSON-RPC communication.

---

## ⚙️ Environment Variables

The server relies on the following environment variables for its configuration:

| Variable | Description | Default | Required |
|:---|:---|:---|:---:|
| `QDRANT_HOST` | IP address or hostname of your Qdrant instance. | `172.20.0.5` | No |
| `QDRANT_PORT` | The port of your Qdrant gRPC endpoint. | `6334` | No |
| `QDRANT_COLLECTION` | The Qdrant collection name to store the codebase vectors. | — | **Yes** |
| `WATCH_DIRECTORY` | The absolute path to the directory you want to watch and index. | — | **Yes** |
| `OLLAMA_HOST` | The base URL of your Ollama endpoint. | — | **Yes** |
| `EMBEDDING_MODEL` | The Ollama embedding model name (e.g., `nomic-embed-text`, `all-minilm`). | — | **Yes** |
| `EXCLUDE_DIRS` | Comma-separated directory names to ignore (e.g., `node_modules,vendor,dist`). | `""` | No |
| `INCLUDE_HIDDEN_DIRS` | Comma-separated hidden folder names to explicitly watch (e.g., `.github,.cursor`). | `""` | No |
| `PARSER_MODE` | Parsing mode: `code` (only AST), `doc` (only documents), or `full` (both). | `full` | No |

---

## 🚀 Installation & Compilation

### Direct One-Line Installation
If you simply want to install the pre-compiled binary on your client machine (supports Linux, macOS, and Windows/WSL), you can run the following command directly:

```bash
curl -fsSL https://raw.githubusercontent.com/weverkley/qdrant-mcp-server/main/install.sh | sh
```

To install a specific version, pass the `VERSION` environment variable:
```bash
curl -fsSL https://raw.githubusercontent.com/weverkley/qdrant-mcp-server/main/install.sh | VERSION=v1.0.0 sh
```

---

### Manual Compilation
Ensure you have [Go](https://go.dev/doc/install) 1.25.0 or later installed.

To compile the codebase into a single, high-performance static binary:

```bash
# Build with debug symbols stripped for maximum execution speed and minimal size
go build -ldflags="-s -w" -o ~/bin/qdrant-mcp-server main.go
```

Alternatively, you can build directly to your working directory:
```bash
go build -o qdrant-mcp-server main.go
```

---

## 🎓 Installing Agent Skills

To help your AI agent (like Cursor, Windsurf, Cline, or Copilot) understand when and how to use the semantic search capabilities, you can install specialized **skills** (rules files) directly into your workspace.

Run the compiled server binary with the `list-skills` and `install-skill` subcommands:

### 1. List Supported Skills
```bash
./qdrant-mcp-server list-skills
```

### 2. Install a Skill for an Agent
Install the rules directly in your active project's root folder:
```bash
# Install Cursor rules (.cursorrules)
./qdrant-mcp-server install-skill cursor

# Install Cline rules (.clinerules)
./qdrant-mcp-server install-skill cline

# Install Copilot instructions (.github/copilot-instructions.md)
./qdrant-mcp-server install-skill copilot

# Install Codex instructions (.codex/mcp-instructions.md)
./qdrant-mcp-server install-skill codex

# Install ALL supported agent skills at once
./qdrant-mcp-server install-skill all
```

You can also specify a custom target path as the last parameter:
```bash
./qdrant-mcp-server install-skill cursor /absolute/path/to/my-project
```

---

## 🔌 Integration with MCP Clients

To use this server with your favorite AI agent tool, add it to your client's MCP configuration settings.

### Claude Desktop Integration
Add the following block to your `claude_desktop_config.json` (typically located at `~/.config/Claude/claude_desktop_config.json` on Linux/macOS or `%APPDATA%\Claude\claude_desktop_config.json` on Windows):

```json
{
  "mcpServers": {
    "qdrant-rag": {
      "command": "/usr/local/bin/qdrant-mcp-server",
      "env": {
        "QDRANT_HOST": "172.20.0.5",
        "QDRANT_COLLECTION": "my-codebase-collection",
        "WATCH_DIRECTORY": "/home/user/Workspace/my-project",
        "OLLAMA_HOST": "http://127.0.0.1:11434",
        "EMBEDDING_MODEL": "nomic-embed-text",
        "EXCLUDE_DIRS": "node_modules,dist,bin,obj,.git",
        "INCLUDE_HIDDEN_DIRS": ".github"
      }
    }
  }
}
```

> [!NOTE]
> - **Direct Installer**: If you installed using the one-line `curl` command, the path is `/usr/local/bin/qdrant-mcp-server` (or `/home/<username>/.local/bin/qdrant-mcp-server` if installed as a non-root fallback).
> - **Manual Compilation**: If you compiled it manually, specify the path where you saved the binary (e.g., `/home/<username>/bin/qdrant-mcp-server` or the absolute path to your working directory build).

### Cursor & Windsurf Integration
1. Open your editor settings.
2. Navigate to **MCP** or **Model Context Protocol** settings.
3. Click **Add New MCP Server**.
4. Set the Type to `command` (or `stdio`).
5. Provide a name: `qdrant-rag`.
6. Provide the command: `/usr/local/bin/qdrant-mcp-server` (update this path to match your installation path: `/usr/local/bin/qdrant-mcp-server`, `/home/<username>/.local/bin/qdrant-mcp-server`, or `/home/<username>/bin/qdrant-mcp-server` depending on how you installed or built it).
7. Configure the environment variables list as shown in the JSON schema above.

---

## 📚 Codex / Knowledge Base Setup

Many developers maintain local documentation, architecture guidelines, team handbooks, or a personal knowledge base inside their repository or workspace using folders like `.codex` or `.obsidian`. 

By default, the server ignores all hidden directories (those starting with a `.`) to prevent performance bottlenecks. You can explicitly instruct the server to monitor, index, and query your Codex notes by adding `.codex` or `.obsidian` to the `INCLUDE_HIDDEN_DIRS` environment variable.

### Setup Example

Simply append your documentation directory to the `INCLUDE_HIDDEN_DIRS` variable in your MCP configuration:

```json
"env": {
  "WATCH_DIRECTORY": "/home/user/Workspace/my-project",
  "INCLUDE_HIDDEN_DIRS": ".codex,.obsidian",
  "QDRANT_COLLECTION": "my-project-vectors",
  "OLLAMA_HOST": "http://127.0.0.1:11434",
  "EMBEDDING_MODEL": "nomic-embed-text"
}
```

### 🧠 Benefits of indexing your Codex
Once configured, the MCP server automatically chunks and indexes your `.codex/*.md` documentation alongside your codebase. Your AI coding assistants can use the `qdrant_search` tool to:
* **Lookup Internal Design Guides:** *"Find the guidelines for writing telemetry logs."*
* **Retrieve Architecture Schemas:** *"What is the database connection strategy documented in the wiki?"*
* **Reference Feature Specifications:** *"How should the new user-onboarding flows behave according to our Codex specs?"*

---

## 🛠️ Provided Tools

### `qdrant_search`
Performs semantic vector-based searches across the entire watched workspace directory.

**Arguments:**
- `query` (string, Required): The natural language query or concept you are searching for.

**Example Client Call:**
```json
{
  "name": "qdrant_search",
  "arguments": {
    "query": "JWT token parsing middleware with custom claim validation"
  }
}
```

**Markdown Response Structure:**
The tool generates a rich, aggregated Markdown response containing up to 5 matching codebase snippets, recognizing and formatting AST function-level metadata (receivers, function names, line ranges) and structured document page numbers:

````markdown
### Core Codebase Reference Snippets for: "JWT token parsing middleware with custom claim validation"

#### [1] Function: `ValidateCustomClaims` in /home/user/Workspace/my-project/auth/middleware.go (Lines 12-32) (Match Score: 0.92)
```go
func ValidateCustomClaims(tokenString string) (*Claims, error) {
    // ...
}
```

#### [2] Doc Chunk (Page/Section 3) in /home/user/Workspace/my-project/docs/auth-specs.md (Match Score: 0.88)
```markdown
JWT token claims are validated against the current session lifecycle policy...
```
````

---

### `get_sync_status`
Retrieves the real-time status of the codebase vector ingestion pipeline, including the status state, pending queue size, active indexing threads, and the total count of successfully synced files during the session lifecycle.

**Arguments:**
*None*

**Example Client Call:**
```json
{
  "name": "get_sync_status"
}
```

**Markdown Response Structure:**
```markdown
### 🔄 Code Ingestion Sync Status

- **Status:** `syncing`
- **Queue Size (Debouncing):** `2`
- **Active Indexing Threads:** `1`
- **Lifetime Synced Files:** `24`

#### ⏳ Files Currently in Debounce Queue:
- `/home/user/Workspace/my-project/auth/middleware.go`
- `/home/user/Workspace/my-project/models/user.go`
```

---

## 📦 Automated Releases & CI/CD

This repository includes a fully automated release workflow powered by GitHub Actions. 

### Triggering a Release
The release process is manual and can be triggered at any time using GitHub's `workflow_dispatch`:

1. Navigate to the **Actions** tab in your GitHub repository.
2. Select the **Build and Release** workflow from the left sidebar.
3. Click the **Run workflow** dropdown on the right.
4. Input the target release version tag (e.g. `v1.0.0`) and click **Run workflow**.

### What the Release Workflow Does:
- **Verification**: Automatically checks Go module dependencies and runs the Go test suites before any builds are triggered.
- **Cross-Compilation**: Compiles native binaries in parallel using a matrix strategy for multiple architectures:
  - **Linux**: `amd64`, `arm64`
  - **macOS (Darwin)**: `amd64`, `arm64`
  - **Windows**: `amd64`
- **Dynamic Versioning**: Injects the exact version tag inputted by the user at build time into the application binary using `-ldflags="-X main.Version=<VERSION> -s -w"`.
- **Packaging**: Packs each compiled binary into `.tar.gz` archives (for Linux/macOS) and `.zip` archives (for Windows).
- **GitHub Release & Assets**: Automatically checks if the git tag exists (creates and pushes it if it does not), creates a new public GitHub release, generates release notes from recent commit history, and attaches all compressed archives as downloadable assets.

---

## 📜 License

[MIT License](LICENSE)