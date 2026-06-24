package ast

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	lang *sitter.Language
	key  string
}

// languageForExt selects a tree-sitter language based on file extension.
func languageForExt(ext string) languageInfo {
	switch ext {
	case ".go":
		return languageInfo{lang: sittergo.GetLanguage(), key: "go"}
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

type queryMetadata struct {
	Namespace string
	Types     []string
	Imports   []ImportRef
}

type queryPatternSpec struct {
	name    string
	pattern string
}

var parserPools sync.Map
var queryCache sync.Map
var queryErrorCache sync.Map
var treeCache sync.Map

var metadataQuerySpecs = map[string][]queryPatternSpec{
	"go": {
		{name: "namespace", pattern: `(package_clause (package_identifier) @namespace)`},
		{name: "type", pattern: `(type_spec name: (type_identifier) @type.name)`},
		{name: "import", pattern: `(import_spec path: (interpreted_string_literal) @import.path)`},
		{name: "import_raw", pattern: `(import_spec path: (raw_string_literal) @import.path)`},
	},
	"javascript": {
		{name: "type", pattern: `(class_declaration name: (identifier) @type.name)`},
		{name: "import", pattern: `(import_statement source: (string (string_fragment) @import.path))`},
	},
	"typescript": {
		{name: "type", pattern: `(class_declaration name: (identifier) @type.name)`},
		{name: "type_interface", pattern: `(interface_declaration name: (type_identifier) @type.name)`},
		{name: "type_alias", pattern: `(type_alias_declaration name: (type_identifier) @type.name)`},
		{name: "type_enum", pattern: `(enum_declaration name: (identifier) @type.name)`},
		{name: "import", pattern: `(import_statement source: (string (string_fragment) @import.path))`},
	},
	"csharp": {
		{name: "namespace", pattern: `(namespace_declaration name: (_) @namespace)`},
		{name: "namespace_file", pattern: `(file_scoped_namespace_declaration name: (_) @namespace)`},
		{name: "type_class", pattern: `(class_declaration name: (identifier) @type.name)`},
		{name: "type_interface", pattern: `(interface_declaration name: (identifier) @type.name)`},
		{name: "type_struct", pattern: `(struct_declaration name: (identifier) @type.name)`},
		{name: "type_record", pattern: `(record_declaration name: (identifier) @type.name)`},
		{name: "type_enum", pattern: `(enum_declaration name: (identifier) @type.name)`},
		{name: "import", pattern: `(using_directive name: (_) @import.path)`},
	},
	"python": {
		{name: "type", pattern: `(class_definition name: (identifier) @type.name)`},
		{name: "import", pattern: `(import_statement name: (dotted_name) @import.path)`},
		{name: "import_from", pattern: `(import_from_statement module_name: (dotted_name) @import.path)`},
	},
	"php": {
		{name: "namespace", pattern: `(namespace_definition name: (_) @namespace)`},
		{name: "type", pattern: `(class_declaration name: (name) @type.name)`},
		{name: "type_interface", pattern: `(interface_declaration name: (name) @type.name)`},
		{name: "type_trait", pattern: `(trait_declaration name: (name) @type.name)`},
		{name: "import", pattern: `(namespace_use_declaration (namespace_use_clause name: (_) @import.path))`},
	},
}

var functionQuerySpecs = map[string][]queryPatternSpec{
	"go": {
		{name: "function", pattern: `(function_declaration name: (identifier) @func.name) @func.node`},
		{name: "method", pattern: `(method_declaration receiver: (parameter_list) @func.receiver name: (field_identifier) @func.name) @func.node`},
	},
	"javascript": {
		{name: "function", pattern: `(function_declaration name: (identifier) @func.name) @func.node`},
		{name: "method", pattern: `(method_definition name: (property_identifier) @func.name) @func.node`},
		{name: "arrow", pattern: `(lexical_declaration (variable_declarator name: (identifier) @func.name value: (arrow_function))) @func.node`},
	},
	"typescript": {
		{name: "function", pattern: `(function_declaration name: (identifier) @func.name) @func.node`},
		{name: "method", pattern: `(method_definition name: (property_identifier) @func.name) @func.node`},
		{name: "arrow", pattern: `(lexical_declaration (variable_declarator name: (identifier) @func.name value: (arrow_function))) @func.node`},
	},
	"php": {
		{name: "function", pattern: `(function_definition name: (name) @func.name) @func.node`},
		{name: "method", pattern: `(method_declaration name: (name) @func.name) @func.node`},
	},
	"csharp": {
		{name: "method", pattern: `(method_declaration name: (identifier) @func.name) @func.node`},
	},
	"python": {
		{name: "function", pattern: `(function_definition name: (identifier) @func.name) @func.node`},
	},
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
	Container string    `json:"container,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
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
	Namespace string
	Types     []string
}

// ParseCodeToDocsWithMeta extracts FunctionNodes and file metadata (imports, calls) from code content.
func ParseCodeToDocsWithMeta(ctx context.Context, filePath string, fileContent []byte) (CodeParseResult, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	langLabel := languageLabel(ext)
	info := languageForExt(ext)
	var functions []FunctionNode
	var imports []ImportRef
	var namespace string
	var types []string

	if info.lang != nil {
		parser := acquireParser(info)
		defer releaseParser(info, parser)

		var oldTree *sitter.Tree
		if cached, ok := treeCache.Load(filePath); ok {
			oldTree = cached.(*sitter.Tree)
		}
		tree, err := parser.ParseCtx(ctx, oldTree, fileContent)
		if tree != nil {
			treeCache.Store(filePath, tree)
			queryMeta := extractMetadataWithQueries(info, tree.RootNode(), fileContent)
			functions = extractFunctionsWithQueries(info, tree.RootNode(), fileContent, queryMeta.Namespace)
			switch info.key {
			case "go":
				imports = chooseImportFallback(queryMeta.Imports, parseGoImports(string(fileContent)))
				namespace = firstNonEmptyString(queryMeta.Namespace, parseGoPackage(string(fileContent)))
			case "javascript":
				imports = chooseImportFallback(queryMeta.Imports, parseJSImports(string(fileContent)))
				namespace = queryMeta.Namespace
			case "typescript":
				imports = chooseImportFallback(queryMeta.Imports, parseTSImports(string(fileContent)))
				namespace = queryMeta.Namespace
			case "php":
				imports = chooseImportFallback(queryMeta.Imports, parsePHPImports(string(fileContent)))
				namespace = firstNonEmptyString(queryMeta.Namespace, parsePHPNamespace(string(fileContent)))
			case "csharp":
				imports = chooseImportFallback(queryMeta.Imports, parseCSharpImports(string(fileContent)))
				namespace = firstNonEmptyString(queryMeta.Namespace, parseCSharpNamespace(string(fileContent)))
			case "python":
				imports = chooseImportFallback(queryMeta.Imports, parsePythonImports(string(fileContent)))
				namespace = queryMeta.Namespace
			}
			types = uniqueStrings(queryMeta.Types)
		}
		_ = err
	}

	if len(functions) == 0 {
		functions = genericFunctionNodes(filePath, fileContent)
	}

	for i := range functions {
		functions[i].Language = langLabel
		if functions[i].Namespace == "" {
			functions[i].Namespace = namespace
		}
	}

	return CodeParseResult{Functions: functions, Imports: imports, Namespace: namespace, Types: uniqueStrings(types)}, nil
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

func extractFunctionsWithQueries(info languageInfo, root *sitter.Node, content []byte, namespace string) []FunctionNode {
	specs := functionQuerySpecs[info.key]
	if len(specs) == 0 || root == nil || info.lang == nil {
		return nil
	}

	var functions []FunctionNode
	seen := make(map[string]struct{})
	for _, spec := range specs {
		query, err := loadQuery(info, spec)
		if err != nil || query == nil {
			continue
		}

		cursor := sitter.NewQueryCursor()
		cursor.Exec(query, root)
		for {
			match, ok := cursor.NextMatch()
			if !ok {
				break
			}

			fn := buildFunctionFromMatch(info, query, match, content, namespace)
			if fn == nil || fn.Name == "" {
				continue
			}

			key := fmt.Sprintf("%s|%d|%d", fn.Name, fn.StartLine, fn.EndLine)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			functions = append(functions, *fn)
		}
		cursor.Close()
	}

	sort.SliceStable(functions, func(i, j int) bool {
		if functions[i].StartLine == functions[j].StartLine {
			return functions[i].Name < functions[j].Name
		}
		return functions[i].StartLine < functions[j].StartLine
	})

	return functions
}

func buildFunctionFromMatch(info languageInfo, query *sitter.Query, match *sitter.QueryMatch, content []byte, namespace string) *FunctionNode {
	var fnNode, nameNode, receiverNode *sitter.Node
	for _, capture := range match.Captures {
		captureName := query.CaptureNameForId(capture.Index)
		switch captureName {
		case "func.node":
			fnNode = capture.Node
		case "func.name":
			nameNode = capture.Node
		case "func.receiver":
			receiverNode = capture.Node
		}
	}

	if fnNode == nil || nameNode == nil {
		return nil
	}

	receiver := ""
	container := ""
	if receiverNode != nil {
		receiver = strings.TrimSpace(receiverNode.Content(content))
		container = receiverTypeName(receiver)
	}
	if container == "" {
		container = findContainingTypeName(fnNode, content)
	}

	return &FunctionNode{
		Name:      strings.TrimSpace(nameNode.Content(content)),
		Signature: string(content[fnNode.StartByte():fnNode.EndByte()]),
		StartLine: int(fnNode.StartPoint().Row) + 1,
		EndLine:   int(fnNode.EndPoint().Row) + 1,
		Receiver:  receiver,
		Container: container,
		Namespace: namespace,
	}
}

func parseGoPackage(content string) string {
	re := regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z0-9_]+)`)
	match := re.FindStringSubmatch(content)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func parseCSharpNamespace(content string) string {
	re := regexp.MustCompile(`(?m)^\s*namespace\s+([A-Za-z0-9_.]+)`)
	match := re.FindStringSubmatch(content)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func parsePHPNamespace(content string) string {
	re := regexp.MustCompile(`(?m)^\s*namespace\s+([^;{]+)`)
	match := re.FindStringSubmatch(content)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func receiverTypeName(receiver string) string {
	replacer := strings.NewReplacer("(", " ", ")", " ", "*", " ", "[", " ", "]", " ", "{", " ", "}", " ")
	parts := strings.Fields(replacer.Replace(receiver))
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part != "" {
			return part
		}
	}
	return ""
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func acquireParser(info languageInfo) *sitter.Parser {
	poolAny, _ := parserPools.LoadOrStore(info.key, &sync.Pool{
		New: func() interface{} {
			parser := sitter.NewParser()
			parser.SetLanguage(info.lang)
			return parser
		},
	})
	parser := poolAny.(*sync.Pool).Get().(*sitter.Parser)
	parser.SetLanguage(info.lang)
	return parser
}

func releaseParser(info languageInfo, parser *sitter.Parser) {
	if parser == nil {
		return
	}
	if poolAny, ok := parserPools.Load(info.key); ok {
		poolAny.(*sync.Pool).Put(parser)
		return
	}
	parser.Close()
}

// EvictTree removes the cached parse tree for a file, freeing memory when a file is deleted.
func EvictTree(filePath string) {
	treeCache.Delete(filePath)
}

func extractMetadataWithQueries(info languageInfo, root *sitter.Node, content []byte) queryMetadata {
	specs := metadataQuerySpecs[info.key]
	if len(specs) == 0 || root == nil || info.lang == nil {
		return queryMetadata{}
	}

	meta := queryMetadata{}
	for _, spec := range specs {
		query, err := loadQuery(info, spec)
		if err != nil || query == nil {
			continue
		}

		cursor := sitter.NewQueryCursor()
		cursor.Exec(query, root)
		for {
			match, ok := cursor.NextMatch()
			if !ok {
				break
			}
			for _, capture := range match.Captures {
				captureName := query.CaptureNameForId(capture.Index)
				captureText := strings.TrimSpace(capture.Node.Content(content))
				if captureText == "" {
					continue
				}

				switch captureName {
				case "namespace":
					meta.Namespace = firstNonEmptyString(meta.Namespace, normalizeCapturedLiteral(captureText))
				case "type.name":
					meta.Types = append(meta.Types, normalizeCapturedLiteral(captureText))
				case "import.path":
					meta.Imports = append(meta.Imports, ImportRef{RawPath: normalizeCapturedLiteral(captureText)})
				}
			}
		}
		cursor.Close()
	}

	meta.Types = uniqueStrings(meta.Types)
	meta.Imports = uniqueImportRefs(meta.Imports)
	return meta
}

func loadQuery(info languageInfo, spec queryPatternSpec) (*sitter.Query, error) {
	cacheKey := info.key + ":" + spec.name
	if cached, ok := queryCache.Load(cacheKey); ok {
		return cached.(*sitter.Query), nil
	}
	if _, failed := queryErrorCache.Load(cacheKey); failed {
		return nil, fmt.Errorf("query previously failed to compile")
	}

	query, err := sitter.NewQuery([]byte(spec.pattern), info.lang)
	if err != nil {
		queryErrorCache.Store(cacheKey, struct{}{})
		return nil, err
	}
	queryCache.Store(cacheKey, query)
	return query, nil
}

func normalizeCapturedLiteral(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, "\"`'")
	return strings.TrimSpace(raw)
}

func mergeImportRefs(primary, fallback []ImportRef) []ImportRef {
	return uniqueImportRefs(append(primary, fallback...))
}

func chooseImportFallback(primary, fallback []ImportRef) []ImportRef {
	if len(primary) > 0 {
		return uniqueImportRefs(primary)
	}
	return uniqueImportRefs(fallback)
}

func uniqueImportRefs(values []ImportRef) []ImportRef {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]ImportRef, 0, len(values))
	for _, value := range values {
		key := strings.TrimSpace(value.RawPath) + "|" + strings.TrimSpace(value.Alias)
		if key == "|" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func findContainingTypeName(node *sitter.Node, content []byte) string {
	for current := node.Parent(); current != nil; current = current.Parent() {
		switch current.Type() {
		case "class_declaration", "interface_declaration", "struct_declaration", "class_definition", "class", "type_spec":
			nameNode := current.ChildByFieldName("name")
			if nameNode != nil {
				name := strings.TrimSpace(nameNode.Content(content))
				if name != "" {
					return name
				}
			}
		}
	}
	return ""
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

	// Size guardrails. Base default follows MAX_FILE_SIZE_BYTES (the single
	// user-facing knob); per-type RAG_MAX_* env vars override it individually.
	baseMax := getMaxBytesFromEnv("MAX_FILE_SIZE_BYTES", 2*1024*1024)
	maxPDFBytes := getMaxBytesFromEnv("RAG_MAX_PDF_BYTES", baseMax)
	maxTextBytes := getMaxBytesFromEnv("RAG_MAX_TEXT_BYTES", baseMax) // md/txt
	maxCSVBytes := getMaxBytesFromEnv("RAG_MAX_CSV_BYTES", baseMax)   // csv
	maxDocBytes := getMaxBytesFromEnv("RAG_MAX_DOC_BYTES", baseMax)   // doc/docx
	maxXLSBytes := getMaxBytesFromEnv("RAG_MAX_XLS_BYTES", baseMax)   // xls/xlsx

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
	case ".xlsx":
		docNode.Type = ext[1:]
		if err := checkSize(filePath, maxXLSBytes); err != nil {
			return DocumentNode{}, nil, err
		}
		content, err = readXlsx(filePath)
		if err != nil {
			return DocumentNode{}, nil, fmt.Errorf("failed to read XLSX file %s: %w", filePath, err)
		}
	case ".docx":
		docNode.Type = ext[1:]
		if err := checkSize(filePath, maxDocBytes); err != nil {
			return DocumentNode{}, nil, err
		}
		content, err = readDocx(filePath)
		if err != nil {
			return DocumentNode{}, nil, fmt.Errorf("failed to read DOCX file %s: %w", filePath, err)
		}
	case ".doc", ".xls":
		// Legacy binary OLE formats have no real parser here; raw-byte reads
		// produce garbage, so refuse rather than ingest noise.
		return DocumentNode{}, nil, fmt.Errorf("legacy binary format %s not supported (convert %s to %sx)", ext, ext, ext)
	default:
		return DocumentNode{}, nil, fmt.Errorf("unsupported file type for chunking: %s", ext)
	}

	// Chunking knobs (env-overridable).
	maxChunkChars := envInt("RAG_MAX_CHUNK_CHARS", 4000)        // hard upper bound per chunk
	minChunkChars := envInt("RAG_MIN_CHUNK_CHARS", 800)         // merge small paragraphs up to this
	overlapChars := envInt("RAG_CHUNK_OVERLAP_CHARS", 200)      // overlap when splitting big paragraphs

	// Readers insert \f between physical pages (PDF). Documents without real
	// pagination (docx/xlsx) have none, so page numbers are reported as 0.
	pages := strings.Split(content, "\f")
	paginated := len(pages) > 1

	for pageIdx, pageRaw := range pages {
		cleaned := cleanExtractedText(pageRaw)
		if strings.TrimSpace(cleaned) == "" {
			continue
		}
		pageNum := 0
		if paginated {
			pageNum = pageIdx + 1
		}
		// Merge small paragraphs and split oversized ones, then emit chunks.
		for _, part := range mergeParagraphs(cleaned, minChunkChars, maxChunkChars, overlapChars) {
			if !IsCleanText(part) {
				continue
			}
			hash := sha256.Sum256([]byte(part))
			chunks = append(chunks, ChunkNode{
				Content:    part,
				PageNumber: pageNum,
				Hash:       fmt.Sprintf("%x", hash),
			})
		}
	}

	if len(chunks) == 0 {
		return DocumentNode{}, nil, fmt.Errorf("file %s yielded no usable plain text after cleaning", filePath)
	}

	return docNode, chunks, nil
}

// envInt reads a positive int from env or returns def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// mergeParagraphs groups paragraphs (split on blank lines) into chunks of at
// least minChars, never exceeding maxChars. Paragraphs larger than maxChars are
// flushed then split with overlap. Prevents the over-fragmentation that turns a
// table-heavy PDF into hundreds of two-word chunks.
func mergeParagraphs(content string, minChars, maxChars, overlap int) []string {
	var out []string
	var buf strings.Builder
	flush := func() {
		if s := strings.TrimSpace(buf.String()); s != "" {
			out = append(out, s)
		}
		buf.Reset()
	}
	for _, p := range strings.Split(content, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > maxChars {
			flush()
			out = append(out, splitIntoChunks(p, maxChars, overlap)...)
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(p)
		if buf.Len() >= minChars {
			flush()
		}
	}
	flush()
	return out
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

	// Primary: per-page row extraction so physical page boundaries are known.
	// Pages are joined with \f (form feed); empty/null pages still emit a
	// separator so downstream page numbers stay aligned with the document.
	totalPage := r.NumPage()
	var doc strings.Builder
	nonEmpty := false
	for i := 1; i <= totalPage; i++ {
		if i > 1 {
			doc.WriteByte('\f')
		}
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		var page strings.Builder
		writeSpaceIfNeeded := func() {
			if page.Len() == 0 {
				return
			}
			last := page.String()[page.Len()-1]
			if last != ' ' && last != '\n' && last != '\t' {
				page.WriteByte(' ')
			}
		}
		rows, _ := p.GetTextByRow()
		for _, row := range rows {
			for _, text := range row.Content {
				s := strings.TrimSpace(text.S)
				if s == "" {
					continue
				}
				writeSpaceIfNeeded()
				page.WriteString(s)
			}
			page.WriteString("\n")
		}
		if strings.TrimSpace(page.String()) != "" {
			nonEmpty = true
		}
		doc.WriteString(page.String())
	}
	if nonEmpty {
		return doc.String(), nil
	}

	// Fallback: library plain text (loses page boundaries -> reported as page 0).
	if plainReader, err := r.GetPlainText(); err == nil {
		if data, err := io.ReadAll(plainReader); err == nil {
			return string(data), nil
		}
	}
	return "", nil
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

// readDocx extracts plain text from a .docx (OOXML zip). It reads
// word/document.xml and emits text content, turning paragraph and break
// elements into newlines so chunking by paragraph works.
func readDocx(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open docx zip: %w", err)
	}
	defer zr.Close()

	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("word/document.xml not found in %s", path)
	}

	rc, err := doc.Open()
	if err != nil {
		return "", fmt.Errorf("open document.xml: %w", err)
	}
	defer rc.Close()

	// Paragraph/break elements become newlines; tabs become spaces.
	breakOn := map[string]bool{"p": true, "br": true, "cr": true}
	spaceOn := map[string]bool{"tab": true}
	return xmlText(rc, breakOn, spaceOn)
}

// readXlsx extracts plain text from a .xlsx (OOXML zip): shared strings plus
// any inline string content across worksheets.
func readXlsx(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open xlsx zip: %w", err)
	}
	defer zr.Close()

	var b strings.Builder
	readPart := func(f *zip.File) {
		rc, err := f.Open()
		if err != nil {
			return
		}
		defer rc.Close()
		// Each <t> string element ends a cell value -> newline separated.
		if txt, err := xmlText(rc, map[string]bool{"t": true, "si": true, "row": true}, nil); err == nil && txt != "" {
			b.WriteString(txt)
			b.WriteString("\n")
		}
	}

	for _, f := range zr.File {
		name := f.Name
		if name == "xl/sharedStrings.xml" ||
			strings.HasPrefix(name, "xl/worksheets/") && strings.HasSuffix(name, ".xml") {
			readPart(f)
		}
	}
	return b.String(), nil
}

// xmlText streams an XML document and concatenates character data. Elements
// whose local name is in breakOn emit a newline on close; those in spaceOn emit
// a space. Unknown markup is discarded, so only real text survives.
func xmlText(r io.Reader, breakOn, spaceOn map[string]bool) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.StartElement:
			if spaceOn != nil && spaceOn[t.Name.Local] {
				b.WriteByte(' ')
			}
		case xml.EndElement:
			if breakOn != nil && breakOn[t.Name.Local] {
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

// IsCleanText reports whether s looks like genuine human-readable text rather
// than binary/markup noise. Rejects empty strings, runs lacking letters, and
// content with a high share of control or replacement characters.
func IsCleanText(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	var letters, total, bad int
	for _, r := range s {
		total++
		switch {
		case r == utf8.RuneError || r == '�':
			bad++
		case unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r':
			bad++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if total == 0 {
		return false
	}
	// Need a meaningful share of letters and almost no binary noise.
	if float64(letters)/float64(total) < 0.5 {
		return false
	}
	if float64(bad)/float64(total) > 0.02 {
		return false
	}
	return true
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
