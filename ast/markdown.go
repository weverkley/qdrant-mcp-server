package ast

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	mdast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type mdFrontMatter struct {
	Title string
	Tags  []string
	Raw   map[string]string
}

type mdSection struct {
	Heading      string
	HeadingPath  []string
	HeadingLevel int
	Body         string
	StartLine    int
	EndLine      int
}

// parseMarkdownToChunks builds one semantic chunk per Markdown section (heading
// hierarchy first). Oversized sections fall back to paragraph/size splitting
// while retaining heading metadata on every subchunk.
func parseMarkdownToChunks(filePath string, raw []byte) (DocumentNode, []ChunkNode, error) {
	ext := strings.ToLower(filepath.Ext(filePath))
	docType := "md"
	if ext == ".markdown" {
		docType = "markdown"
	}
	docNode := DocumentNode{Path: filePath, Type: docType}

	normalized := normalizeNewlines(string(raw))
	fm, body, bodyLineOffset := stripYAMLFrontMatter(normalized)
	source := []byte(body)

	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	reader := text.NewReader(source)
	root := md.Parser().Parse(reader)

	sections := collectMarkdownSections(root, source, bodyLineOffset)
	docTitle := fm.Title
	if docTitle == "" {
		docTitle = firstH1Title(sections)
	}
	if docTitle == "" {
		docTitle = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
	}

	maxChunkChars := envInt("RAG_MAX_CHUNK_CHARS", 4000)
	minChunkChars := envInt("RAG_MIN_CHUNK_CHARS", 800)
	overlapChars := envInt("RAG_CHUNK_OVERLAP_CHARS", 200)

	var chunks []ChunkNode
	for _, sec := range sections {
		bodyText := strings.TrimSpace(sec.Body)
		if bodyText == "" && sec.Heading == "" {
			continue
		}
		parts := splitSectionBody(bodyText, minChunkChars, maxChunkChars, overlapChars)
		if len(parts) == 0 {
			// Heading-only section: still emit a hierarchy-bearing chunk.
			parts = []string{""}
		}
		for _, part := range parts {
			content := formatMarkdownEmbedding(docTitle, sec.HeadingPath, part)
			if !IsCleanText(content) && strings.TrimSpace(part) != "" {
				continue
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			hash := sha256.Sum256([]byte(content))
			chunks = append(chunks, ChunkNode{
				Content:      content,
				Hash:         fmt.Sprintf("%x", hash),
				SourceKind:   "markdown",
				Heading:      sec.Heading,
				HeadingPath:  append([]string(nil), sec.HeadingPath...),
				HeadingLevel: sec.HeadingLevel,
				StartLine:    sec.StartLine,
				EndLine:      sec.EndLine,
				Title:        docTitle,
				MetaTags:     append([]string(nil), fm.Tags...),
			})
		}
	}

	if len(chunks) == 0 {
		return DocumentNode{}, nil, fmt.Errorf("markdown file %s yielded no usable sections", filePath)
	}
	return docNode, chunks, nil
}

func formatMarkdownEmbedding(docTitle string, headingPath []string, body string) string {
	var b strings.Builder
	b.WriteString("Document: ")
	b.WriteString(docTitle)
	b.WriteString("\n\n")
	if len(headingPath) > 0 {
		b.WriteString("Section: ")
		b.WriteString(strings.Join(headingPath, " > "))
		b.WriteString("\n\n")
	}
	b.WriteString(strings.TrimSpace(body))
	return strings.TrimSpace(b.String())
}

func splitSectionBody(body string, minChars, maxChars, overlap int) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	// Prefer structural integrity: if the whole section fits, keep it whole so
	// fenced code blocks are never split across chunks.
	if len(body) <= maxChars {
		return []string{body}
	}
	return mergeParagraphs(body, minChars, maxChars, overlap)
}

func firstH1Title(sections []mdSection) string {
	for _, sec := range sections {
		if sec.HeadingLevel == 1 && sec.Heading != "" {
			return sec.Heading
		}
	}
	return ""
}

func collectMarkdownSections(root mdast.Node, source []byte, lineOffset int) []mdSection {
	type headingMark struct {
		level int
		title string
		start int
		end   int
		line  int
	}

	var marks []headingMark
	for child := root.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != mdast.KindHeading {
			continue
		}
		h := child.(*mdast.Heading)
		start, end, line := nodeSourceSpan(child, source)
		marks = append(marks, headingMark{
			level: h.Level,
			title: strings.TrimSpace(nodeInlineText(child, source)),
			start: start,
			end:   end,
			line:  lineOffset + line,
		})
	}

	var sections []mdSection
	stack := make([]string, 0, 6)

	// Preamble: content before the first heading.
	preambleEnd := len(source)
	if len(marks) > 0 {
		preambleEnd = marks[0].start
	}
	if pre := strings.TrimSpace(string(source[:preambleEnd])); pre != "" {
		sections = append(sections, mdSection{
			Heading:      "",
			HeadingPath:  nil,
			HeadingLevel: 0,
			Body:         pre,
			StartLine:    lineOffset + 1,
			EndLine:      lineOffset + countLines(source[:preambleEnd]),
		})
	}

	for i, mark := range marks {
		for len(stack) >= mark.level {
			stack = stack[:len(stack)-1]
		}
		for len(stack) < mark.level-1 {
			stack = append(stack, "")
		}
		if len(stack) == mark.level-1 {
			stack = append(stack, mark.title)
		} else {
			stack[mark.level-1] = mark.title
			stack = stack[:mark.level]
		}
		path := compactHeadingPath(stack)

		bodyEnd := len(source)
		endLine := lineOffset + countLines(source)
		if i+1 < len(marks) {
			bodyEnd = marks[i+1].start
			endLine = marks[i+1].line - 1
			if endLine < mark.line {
				endLine = mark.line
			}
		}
		body := strings.TrimSpace(string(source[mark.end:bodyEnd]))
		sections = append(sections, mdSection{
			Heading:      mark.title,
			HeadingPath:  path,
			HeadingLevel: mark.level,
			Body:         body,
			StartLine:    mark.line,
			EndLine:      endLine,
		})
	}
	return sections
}

func compactHeadingPath(stack []string) []string {
	out := make([]string, 0, len(stack))
	for _, s := range stack {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func nodeSourceSpan(n mdast.Node, source []byte) (start, end, line int) {
	lines := n.Lines()
	if lines == nil || lines.Len() == 0 {
		// Fall back: scan children for text segments.
		start = len(source)
		end = 0
		mdast.Walk(n, func(node mdast.Node, entering bool) (mdast.WalkStatus, error) {
			if !entering {
				return mdast.WalkContinue, nil
			}
			if t, ok := node.(*mdast.Text); ok {
				seg := t.Segment
				if seg.Start < start {
					start = seg.Start
				}
				if seg.Stop > end {
					end = seg.Stop
				}
			}
			return mdast.WalkContinue, nil
		})
		if start > end {
			return 0, 0, 1
		}
		return start, end, 1 + bytes.Count(source[:start], []byte{'\n'})
	}
	first := lines.At(0)
	last := lines.At(lines.Len() - 1)
	start = first.Start
	end = last.Stop
	// Include trailing newline after the heading line when present.
	if end < len(source) && source[end] == '\n' {
		end++
	}
	line = 1 + bytes.Count(source[:start], []byte{'\n'})
	return start, end, line
}

func nodeInlineText(n mdast.Node, source []byte) string {
	var b strings.Builder
	mdast.Walk(n, func(node mdast.Node, entering bool) (mdast.WalkStatus, error) {
		if !entering {
			return mdast.WalkContinue, nil
		}
		switch t := node.(type) {
		case *mdast.Text:
			b.Write(t.Segment.Value(source))
		case *mdast.String:
			b.Write(t.Value)
		case *mdast.CodeSpan:
			for c := t.FirstChild(); c != nil; c = c.NextSibling() {
				if txt, ok := c.(*mdast.Text); ok {
					b.Write(txt.Segment.Value(source))
				}
			}
			return mdast.WalkSkipChildren, nil
		}
		return mdast.WalkContinue, nil
	})
	return b.String()
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := 1 + bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] == '\n' && n > 1 {
		n--
	}
	return n
}

// stripYAMLFrontMatter removes a leading --- ... --- block. Returns simple
// key/value metadata (scalars and string lists only).
func stripYAMLFrontMatter(content string) (mdFrontMatter, string, int) {
	fm := mdFrontMatter{Raw: map[string]string{}}
	s := content
	if !strings.HasPrefix(s, "---") {
		return fm, content, 0
	}
	// Allow optional BOM already stripped; require --- at start of file.
	rest := s[3:]
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else {
		return fm, content, 0
	}

	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, content, 0
	}
	yamlBlock := rest[:end]
	after := rest[end+len("\n---"):]
	after = strings.TrimPrefix(after, "\r")
	after = strings.TrimPrefix(after, "\n")

	lineOffset := 2 + strings.Count(yamlBlock, "\n") // opening --- + yaml lines + closing ---
	parseSimpleFrontMatter(yamlBlock, &fm)
	return fm, after, lineOffset
}

func parseSimpleFrontMatter(yamlBlock string, fm *mdFrontMatter) {
	lines := strings.Split(yamlBlock, "\n")
	var listKey string
	for _, line := range lines {
		trimmed := strings.TrimRightFunc(line, unicode.IsSpace)
		if trimmed == "" || strings.HasPrefix(strings.TrimSpace(trimmed), "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "  - ") || strings.HasPrefix(trimmed, "- ") {
			if listKey == "" {
				continue
			}
			item := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "-"))
			item = trimYAMLQuotes(item)
			if listKey == "tags" {
				fm.Tags = append(fm.Tags, item)
			}
			fm.Raw[listKey] = strings.TrimSpace(fm.Raw[listKey] + " " + item)
			continue
		}
		listKey = ""
		idx := strings.IndexByte(trimmed, ':')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.TrimSpace(trimmed[idx+1:])
		keyLower := strings.ToLower(key)
		if val == "" {
			listKey = keyLower
			continue
		}
		val = trimYAMLQuotes(val)
		fm.Raw[keyLower] = val
		switch keyLower {
		case "title":
			fm.Title = val
		case "tags":
			// Inline list: [a, b] or a, b
			fm.Tags = append(fm.Tags, splitFrontMatterList(val)...)
		}
	}
}

func trimYAMLQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func splitFrontMatterList(val string) []string {
	val = strings.TrimSpace(val)
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	parts := strings.Split(val, ",")
	var out []string
	for _, p := range parts {
		p = trimYAMLQuotes(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
