package ast

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	sitter "github.com/smacker/go-tree-sitter"
	sittercsharp "github.com/smacker/go-tree-sitter/csharp"
	sittergo "github.com/smacker/go-tree-sitter/golang"
	sitterjs "github.com/smacker/go-tree-sitter/javascript"
	sitterphp "github.com/smacker/go-tree-sitter/php"
	sitterpy "github.com/smacker/go-tree-sitter/python"
	sitterts "github.com/smacker/go-tree-sitter/typescript/typescript"
)

type languageInfo struct {
	lang        *sitter.Language
	goExtractor bool
	key         string
}

// languageForExt selects a tree-sitter language based on file extension.
func languageForExt(ext string) languageInfo {
	switch ext {
	case ".go":
		return languageInfo{lang: sittergo.GetLanguage(), goExtractor: true, key: "go"}
	case ".js", ".jsx", ".mjs", ".cjs":
		return languageInfo{lang: sitterjs.GetLanguage(), key: "javascript"}
	case ".ts", ".tsx":
		return languageInfo{lang: sitterts.GetLanguage(), key: "typescript"}
	case ".php":
		return languageInfo{lang: sitterphp.GetLanguage(), key: "php"}
	case ".cs":
		return languageInfo{lang: sittercsharp.GetLanguage(), key: "csharp"}
	case ".py":
		return languageInfo{lang: sitterpy.GetLanguage(), key: "python"}
	default:
		return languageInfo{}
	}
}

func languageLabel(ext string) string {
	switch strings.ToLower(ext) {
	case ".go":
		return "go"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".php":
		return "php"
	case ".cs":
		return "csharp"
	case ".py":
		return "python"
	default:
		return "unknown"
	}
}

// FunctionNode represents a function within a file.
type FunctionNode struct {
	Name      string    `json:"name"`
	Signature string    `json:"signature"` // e.g., func (r Receiver) MethodName(args) (returns)
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Language  string    `json:"language,omitempty"`
	Receiver  string    `json:"receiver,omitempty"`
	Calls     []CallRef `json:"calls,omitempty"`
}

// ImportRef captures an import statement in a file.
type ImportRef struct {
	RawPath  string `json:"raw_path"`
	Resolved string `json:"resolved,omitempty"`
	IsModule bool   `json:"is_module,omitempty"`
	Alias    string `json:"alias,omitempty"`
}

// CallRef captures a function call reference inside a function body.
type CallRef struct {
	Name string `json:"name"`
}

// CodeParseResult bundles parsed functions plus file-level metadata.
type CodeParseResult struct {
	Functions []FunctionNode
	Imports   []ImportRef
}

// ParseCodeToDocsWithMeta extracts FunctionNodes and file metadata (imports, calls) from code content.
func ParseCodeToDocsWithMeta(ctx context.Context, filePath string, fileContent []byte) (CodeParseResult, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	langLabel := languageLabel(ext)
	info := languageForExt(ext)
	var functions []FunctionNode
	var imports []ImportRef

	if info.lang != nil {
		parser := sitter.NewParser()
		parser.SetLanguage(info.lang)

		tree, err := parser.ParseCtx(ctx, nil, fileContent)
		if tree != nil {
			switch info.key {
			case "go":
				functions = findGoFunctions(tree.RootNode(), fileContent)
				imports = parseGoImports(string(fileContent))
			case "javascript":
				functions = findJSFunctions(tree.RootNode(), fileContent)
				imports = parseJSImports(string(fileContent))
			case "typescript":
				functions = findTSFunctions(tree.RootNode(), fileContent)
				imports = parseTSImports(string(fileContent))
			case "php":
				functions = findPHPFunctions(tree.RootNode(), fileContent)
				imports = parsePHPImports(string(fileContent))
			case "csharp":
				functions = findCSharpFunctions(tree.RootNode(), fileContent)
				imports = parseCSharpImports(string(fileContent))
			case "python":
				functions = findPythonFunctions(tree.RootNode(), fileContent)
				imports = parsePythonImports(string(fileContent))
			}
		}
		_ = err
	}

	if len(functions) == 0 {
		functions = genericFunctionNodes(filePath, fileContent)
	}

	for i := range functions {
		functions[i].Language = langLabel
	}

	return CodeParseResult{Functions: functions, Imports: imports}, nil
}

// ParseCodeToDocs extracts FunctionNodes from code content.
func ParseCodeToDocs(ctx context.Context, filePath string, fileContent []byte) ([]FunctionNode, error) {
	result, err := ParseCodeToDocsWithMeta(ctx, filePath, fileContent)
	return result.Functions, err
}

func genericFunctionNodes(filePath string, fileContent []byte) []FunctionNode {
	content := string(fileContent)
	lines := strings.Count(content, "\n") + 1
	name := filepath.Base(filePath)
	if len(content) > 4000 {
		content = content[:4000] + "..."
	}
	return []FunctionNode{{
		Name:      name,
		Signature: content,
		StartLine: 1,
		EndLine:   lines,
		Language:  languageLabel(filepath.Ext(filePath)),
	}}
}

// Go parser helpers
func findGoFunctions(root *sitter.Node, content []byte) []FunctionNode {
	var functions []FunctionNode
	var currentReceiver string

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "method_declaration" || n.Type() == "function_declaration" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name := nameNode.Content(content)
				start := int(n.StartPoint().Row) + 1
				end := int(n.EndPoint().Row) + 1
				sig := string(content[n.StartByte():n.EndByte()])
				recv := ""
				if n.Type() == "method_declaration" {
					recvNode := n.ChildByFieldName("receiver")
					if recvNode != nil {
						recv = recvNode.Content(content)
						currentReceiver = recv
					}
				}
				functions = append(functions, FunctionNode{
					Name:      name,
					Signature: sig,
					StartLine: start,
					EndLine:   end,
					Receiver:  recv,
				})
			}
		}
		if n.Type() == "type_spec" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				currentReceiver = nameNode.Content(content)
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}

	walk(root)
	_ = currentReceiver
	return functions
}

// JavaScript parser helpers
func findJSFunctions(root *sitter.Node, content []byte) []FunctionNode {
	var functions []FunctionNode

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "function_declaration" || n.Type() == "method_definition" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name := nameNode.Content(content)
				start := int(n.StartPoint().Row) + 1
				end := int(n.EndPoint().Row) + 1
				sig := string(content[n.StartByte():n.EndByte()])
				functions = append(functions, FunctionNode{Name: name, Signature: sig, StartLine: start, EndLine: end})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}

	walk(root)
	return functions
}

// TypeScript parser helpers
func findTSFunctions(root *sitter.Node, content []byte) []FunctionNode {
	return findJSFunctions(root, content)
}

// PHP parser helpers
func findPHPFunctions(root *sitter.Node, content []byte) []FunctionNode {
	var functions []FunctionNode

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "function_definition" || n.Type() == "method_declaration" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name := nameNode.Content(content)
				start := int(n.StartPoint().Row) + 1
				end := int(n.EndPoint().Row) + 1
				sig := string(content[n.StartByte():n.EndByte()])
				functions = append(functions, FunctionNode{Name: name, Signature: sig, StartLine: start, EndLine: end})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}

	walk(root)
	return functions
}

// C# parser helpers
func findCSharpFunctions(root *sitter.Node, content []byte) []FunctionNode {
	var functions []FunctionNode

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "method_declaration" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name := nameNode.Content(content)
				start := int(n.StartPoint().Row) + 1
				end := int(n.EndPoint().Row) + 1
				sig := string(content[n.StartByte():n.EndByte()])
				functions = append(functions, FunctionNode{Name: name, Signature: sig, StartLine: start, EndLine: end})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}

	walk(root)
	return functions
}

// Python parser helpers
func findPythonFunctions(root *sitter.Node, content []byte) []FunctionNode {
	var functions []FunctionNode

	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "function_definition" {
			nameNode := n.ChildByFieldName("name")
			if nameNode != nil {
				name := nameNode.Content(content)
				start := int(n.StartPoint().Row) + 1
				end := int(n.EndPoint().Row) + 1
				sig := string(content[n.StartByte():n.EndByte()])
				functions = append(functions, FunctionNode{Name: name, Signature: sig, StartLine: start, EndLine: end})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}

	walk(root)
	return functions
}

func parseGoImports(content string) []ImportRef {
	var imports []ImportRef
	block := false
	for _, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "import (") {
			block = true
			continue
		}
		if block && trim == ")" {
			block = false
			continue
		}
		if block {
			if trim == "" {
				continue
			}
			alias := ""
			parts := strings.Fields(trim)
			if len(parts) > 1 {
				alias = parts[0]
				trim = parts[len(parts)-1]
			}
			trim = strings.Trim(trim, "\"")
			if trim == "" {
				continue
			}
			imports = append(imports, ImportRef{RawPath: trim, Alias: alias})
		} else if strings.HasPrefix(trim, "import ") {
			trim = strings.TrimSpace(strings.TrimPrefix(trim, "import "))
			alias := ""
			parts := strings.Fields(trim)
			if len(parts) > 1 {
				alias = parts[0]
				trim = parts[len(parts)-1]
			}
			trim = strings.Trim(trim, "\"")
			if trim == "" {
				continue
			}
			imports = append(imports, ImportRef{RawPath: trim, Alias: alias})
		}
	}
	return imports
}

func parseJSImports(content string) []ImportRef {
	var imports []ImportRef
	re := regexp.MustCompile(`(?m)^\s*import\s+(?:[^\n]+\s+from\s+)?[\"']([^\"']+)[\"']`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			imports = append(imports, ImportRef{RawPath: match[1]})
		}
	}
	return imports
}

func parseTSImports(content string) []ImportRef {
	return parseJSImports(content)
}

func parsePHPImports(content string) []ImportRef {
	var imports []ImportRef
	re := regexp.MustCompile(`(?m)^\s*use\s+([^;]+);`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			imports = append(imports, ImportRef{RawPath: strings.TrimSpace(match[1])})
		}
	}
	return imports
}

func parseCSharpImports(content string) []ImportRef {
	var imports []ImportRef
	re := regexp.MustCompile(`(?m)^\s*using\s+([^;]+);`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			imports = append(imports, ImportRef{RawPath: strings.TrimSpace(match[1])})
		}
	}
	return imports
}

func parsePythonImports(content string) []ImportRef {
	var imports []ImportRef
	re := regexp.MustCompile(`(?m)^\s*(?:import|from)\s+([^\s]+)`)
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			imports = append(imports, ImportRef{RawPath: strings.TrimSpace(match[1])})
		}
	}
	return imports
}

// DocumentNode represents a text document.
type DocumentNode struct {
	Path    string `json:"path"`
	Type    string `json:"type"` // e.g., "md", "pdf", "txt"
	Session string `json:"session,omitempty"`
}

// ChunkNode represents a semantic chunk within a document.
type ChunkNode struct {
	Content    string    `json:"content"`
	Embedding  []float32 `json:"embedding,omitempty"`
	PageNumber int       `json:"page_number,omitempty"` // For PDF documents
	Hash       string    `json:"hash"`                  // Unique hash of content for deduplication
	Session    string    `json:"session,omitempty"`
}

// ParseTextToChunks reads text/PDF files and splits content into ChunkNodes.
func ParseTextToChunks(filePath string) (DocumentNode, []ChunkNode, error) {
	docNode := DocumentNode{Path: filePath}
	var chunks []ChunkNode
	var content string
	var err error

	// Size guardrails (overridable via env)
	maxPDFBytes := getMaxBytesFromEnv("RAG_MAX_PDF_BYTES", 2*1024*1024)
	maxTextBytes := getMaxBytesFromEnv("RAG_MAX_TEXT_BYTES", 2*1024*1024) // md/txt
	maxCSVBytes := getMaxBytesFromEnv("RAG_MAX_CSV_BYTES", 2*1024*1024)   // csv
	maxDocBytes := getMaxBytesFromEnv("RAG_MAX_DOC_BYTES", 2*1024*1024)   // doc/docx
	maxXLSBytes := getMaxBytesFromEnv("RAG_MAX_XLS_BYTES", 2*1024*1024)   // xls/xlsx
	maxBinaryText := maxDocBytes                                          // doc/docx share cap unless overridden separately

	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".pdf":
		docNode.Type = "pdf"
		if err := checkSize(filePath, maxPDFBytes); err != nil {
			return DocumentNode{}, nil, err
		}
		content, err = readPdf(filePath)
		if err != nil {
			return DocumentNode{}, nil, fmt.Errorf("failed to read PDF file %s: %w", filePath, err)
		}
		content = normalizeNewlines(content)
		if strings.TrimSpace(content) == "" {
			return DocumentNode{}, nil, fmt.Errorf("pdf file %s contained no extractable text", filePath)
		}
	case ".md", ".txt":
		docNode.Type = ext[1:]
		content, err = readTextWithGuards(filePath, maxTextBytes, false)
		if err != nil {
			return DocumentNode{}, nil, err
		}
	case ".csv":
		docNode.Type = ext[1:]
		content, err = readTextWithGuards(filePath, maxCSVBytes, false)
		if err != nil {
			return DocumentNode{}, nil, err
		}
	case ".xls", ".xlsx":
		docNode.Type = ext[1:]
		content, err = readTextWithGuards(filePath, maxXLSBytes, true)
		if err != nil {
			return DocumentNode{}, nil, err
		}
	case ".doc", ".docx":
		docNode.Type = ext[1:]
		content, err = readTextWithGuards(filePath, maxBinaryText, true)
		if err != nil {
			return DocumentNode{}, nil, err
		}
	default:
		return DocumentNode{}, nil, fmt.Errorf("unsupported file type for chunking: %s", ext)
	}

	// Simple chunking strategy: split by paragraphs (double newline)
	maxChunkChars := 4000
	if envVal := os.Getenv("RAG_MAX_CHUNK_CHARS"); envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil && n > 0 {
			maxChunkChars = n
		}
	}
	overlapChars := 200
	if envVal := os.Getenv("RAG_CHUNK_OVERLAP_CHARS"); envVal != "" {
		if n, err := strconv.Atoi(envVal); err == nil && n >= 0 {
			overlapChars = n
		}
	}

	paragraphs := strings.Split(content, "\n\n")
	for i, p := range paragraphs {
		trimmedP := strings.TrimSpace(p)
		if trimmedP == "" {
			continue
		}
		parts := splitIntoChunks(trimmedP, maxChunkChars, overlapChars)
		for _, part := range parts {
			hash := sha256.Sum256([]byte(part))
			chunks = append(chunks, ChunkNode{
				Content:    part,
				PageNumber: i + 1, // Placeholder for page number/paragraph number
				Hash:       fmt.Sprintf("%x", hash),
			})
		}
	}

	return docNode, chunks, nil
}

// readPdf extracts text from a PDF file. It guards against panics inside the PDF parser.
func readPdf(path string) (content string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf parse panic for %s: %v", path, r)
		}
	}()

	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	// Try library plain text first
	if plainReader, err := r.GetPlainText(); err == nil {
		if data, err := io.ReadAll(plainReader); err == nil {
			if cleaned := cleanExtractedText(string(data)); cleaned != "" {
				return cleaned, nil
			}
		}
	}

	// Fallback: row-based extraction.
	totalPage := r.NumPage()

	var textBuilder strings.Builder
	writeSpaceIfNeeded := func() {
		if textBuilder.Len() == 0 {
			return
		}
		last := textBuilder.String()[textBuilder.Len()-1]
		if last != ' ' && last != '\n' && last != '\t' {
			textBuilder.WriteByte(' ')
		}
	}

	for i := 1; i <= totalPage; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		rows, _ := p.GetTextByRow()
		for _, row := range rows {
			for _, text := range row.Content {
				s := strings.TrimSpace(text.S)
				if s == "" {
					continue
				}
				writeSpaceIfNeeded()
				textBuilder.WriteString(s)
			}
			textBuilder.WriteString("\n")
		}
		textBuilder.WriteString("\n")
	}

	return cleanExtractedText(textBuilder.String()), nil
}

// checkSize ensures the file does not exceed the provided limit. Zero/negative disables check.
func checkSize(path string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat failed for %s: %w", path, err)
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("file %s exceeds max supported size (%d bytes > %d)", path, info.Size(), maxBytes)
	}
	return nil
}

// getMaxBytesFromEnv reads an int64 from env or returns defaultVal when unset/invalid.
func getMaxBytesFromEnv(envKey string, defaultVal int64) int64 {
	val := strings.TrimSpace(os.Getenv(envKey))
	if val == "" {
		return defaultVal
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil || n < 0 {
		return defaultVal
	}
	return n
}

// splitIntoChunks splits a long string into smaller parts, trying to keep sentence boundaries.
func splitIntoChunks(text string, maxChars int, overlapChars int) []string {
	if maxChars <= 0 || len(text) <= maxChars {
		return []string{text}
	}

	var chunks []string
	remaining := strings.TrimSpace(text)
	if overlapChars < 0 {
		overlapChars = 0
	}
	for len(remaining) > maxChars {
		cut := maxChars
		// Try to split on sentence boundary
		if idx := strings.LastIndex(remaining[:maxChars], ". "); idx > maxChars/2 {
			cut = idx + 1
		} else if idx := strings.LastIndex(remaining[:maxChars], "\n"); idx > maxChars/2 {
			cut = idx
		} else if idx := strings.LastIndex(remaining[:maxChars], " "); idx > maxChars/2 {
			cut = idx
		}

		chunk := strings.TrimSpace(remaining[:cut])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		advance := cut - overlapChars
		if advance <= 0 {
			advance = cut
		}
		if advance >= len(remaining) {
			remaining = ""
		} else {
			remaining = strings.TrimSpace(remaining[advance:])
		}
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

func readTextWithGuards(path string, maxBytes int64, allowBinary bool) (string, error) {
	if err := checkSize(path, maxBytes); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !allowBinary && !utf8.Valid(data) {
		return "", fmt.Errorf("file %s is not valid UTF-8", path)
	}
	cleaned := sanitizeText(data)
	if cleaned == "" {
		return "", fmt.Errorf("file %s contains no usable text", path)
	}
	return cleaned, nil
}

func sanitizeText(data []byte) string {
	if utf8.Valid(data) {
		return cleanExtractedText(string(data))
	}
	// strip non-printable bytes
	var b strings.Builder
	for _, c := range data {
		if c == '\n' || c == '\r' || c == '\t' {
			b.WriteByte(c)
			continue
		}
		if c < 32 || c > 126 {
			continue
		}
		b.WriteByte(c)
	}
	return cleanExtractedText(b.String())
}

func cleanExtractedText(input string) string {
	input = normalizeNewlines(input)
	input = filterNoisyLines(input)
	input = mergeSoftLineBreaks(input)
	input = strings.TrimSpace(input)
	return input
}

func normalizeNewlines(input string) string {
	return strings.ReplaceAll(strings.ReplaceAll(input, "\r\n", "\n"), "\r", "\n")
}

// mergeSoftLineBreaks treats single newlines as soft breaks (space) and preserves paragraph gaps.
func mergeSoftLineBreaks(input string) string {
	lines := strings.Split(input, "\n")
	var out []string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		out = append(out, strings.Join(current, " "))
		current = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			out = append(out, "")
			continue
		}
		current = append(current, trimmed)
	}
	flush()
	return strings.Join(out, "\n")
}

// filterNoisyLines drops lines with very low alphanumeric ratio, keeping blanks as paragraph separators.
func filterNoisyLines(input string) string {
	lines := strings.Split(input, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			kept = append(kept, "")
			continue
		}
		total := 0
		alnum := 0
		for _, r := range trimmed {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
			if !unicode.IsSpace(r) {
				total++
			}
		}
		if total == 0 {
			continue
		}
		ratio := float64(alnum) / float64(total)
		if ratio < 0.2 {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "\n")
}
