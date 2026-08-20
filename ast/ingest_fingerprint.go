package ast

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

// Strategy identifiers for ingest fingerprints. Bump the relevant constant when
// chunk semantics for that parser change; do not bump on unrelated app releases.
const (
	MarkdownStrategy = "markdown-struct-v1"
	GherkinStrategy  = "gherkin-scenario-v1"
	DocumentStrategy = "doc-paragraph-v1"
	CodeStrategy     = "ast-code-v1"
	GenericStrategy  = "generic-chunk-v1"
)

// ParserStrategyForExt returns the ingest strategy id for a file extension.
func ParserStrategyForExt(ext string) string {
	ext = strings.ToLower(ext)
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	switch ext {
	case ".md", ".markdown":
		return MarkdownStrategy
	case ".feature":
		return GherkinStrategy
	case ".pdf", ".txt", ".csv", ".xls", ".xlsx", ".doc", ".docx":
		return DocumentStrategy
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".php", ".cs", ".py",
		".rs", ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return CodeStrategy
	default:
		return GenericStrategy
	}
}

// ChunkConfigFingerprint captures env knobs that affect document chunk sizes.
func ChunkConfigFingerprint() string {
	maxChars := envInt("RAG_MAX_CHUNK_CHARS", 4000)
	minChars := envInt("RAG_MIN_CHUNK_CHARS", 800)
	overlap := envInt("RAG_CHUNK_OVERLAP_CHARS", 200)
	return fmt.Sprintf("max=%d;min=%d;overlap=%d", maxChars, minChars, overlap)
}

// IngestFingerprint builds a stable digest used for incremental skip decisions.
// It combines file bytes with parser strategy, mode, chunk config, and embedding model.
func IngestFingerprint(content []byte, filePath, parserMode, embedModel string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	strategy := ParserStrategyForExt(ext)
	mode := strings.ToLower(strings.TrimSpace(parserMode))
	if mode == "" {
		mode = "full"
	}
	model := strings.TrimSpace(embedModel)
	chunkCfg := ChunkConfigFingerprint()

	h := sha256.New()
	_, _ = h.Write(content)
	_, _ = h.Write([]byte("\n|strategy="))
	_, _ = h.Write([]byte(strategy))
	_, _ = h.Write([]byte("|mode="))
	_, _ = h.Write([]byte(mode))
	_, _ = h.Write([]byte("|chunk="))
	_, _ = h.Write([]byte(chunkCfg))
	_, _ = h.Write([]byte("|model="))
	_, _ = h.Write([]byte(model))
	return fmt.Sprintf("%x", h.Sum(nil))
}
