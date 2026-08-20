package ast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempMarkdown(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp markdown: %v", err)
	}
	return path
}

func TestMarkdown_MD1_HeadingsHierarchy(t *testing.T) {
	path := writeTempMarkdown(t, "arch.md", `# Architecture

Intro.

## Runtime

Runtime body.

### Audio Graph

Graph body.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var found []string
	for _, c := range chunks {
		if c.SourceKind != "markdown" {
			t.Fatalf("expected source_kind markdown, got %q", c.SourceKind)
		}
		if len(c.HeadingPath) > 0 {
			found = append(found, strings.Join(c.HeadingPath, " > "))
		}
	}
	want := []string{
		"Architecture",
		"Architecture > Runtime",
		"Architecture > Runtime > Audio Graph",
	}
	for _, w := range want {
		ok := false
		for _, f := range found {
			if f == w {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("missing heading path %q in %v", w, found)
		}
	}
}

func TestMarkdown_MD2_SiblingSections(t *testing.T) {
	path := writeTempMarkdown(t, "siblings.md", `## Runtime

Runtime details about scheduling.

## Studio

Studio details about the UI shell.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var headings []string
	for _, c := range chunks {
		if c.Heading != "" {
			headings = append(headings, c.Heading)
		}
	}
	if len(headings) < 2 {
		t.Fatalf("expected distinct sibling sections, got %v", headings)
	}
	if headings[0] == headings[1] {
		t.Fatalf("siblings should be distinct: %v", headings)
	}
	joined := strings.Join(headings, ",")
	if !strings.Contains(joined, "Runtime") || !strings.Contains(joined, "Studio") {
		t.Fatalf("expected Runtime and Studio sections, got %v", headings)
	}
}

func TestMarkdown_MD3_CodeFence(t *testing.T) {
	path := writeTempMarkdown(t, "fence.md", `## Transitions

Use this helper:

`+"```go"+`
func guard() bool { return true }
`+"```"+`

Then continue.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, c := range chunks {
		if c.Heading == "Transitions" &&
			strings.Contains(c.Content, "```go") &&
			strings.Contains(c.Content, "func guard()") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fenced code attached to Transitions section; chunks=%d", len(chunks))
	}
}

func TestMarkdown_MD4_Lists(t *testing.T) {
	path := writeTempMarkdown(t, "lists.md", `## Events

- Play
  - nested
- Pause
- Stop
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c.Content, "Play") &&
			strings.Contains(c.Content, "Pause") &&
			strings.Contains(c.Content, "Stop") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected list items preserved in section content")
	}
}

func TestMarkdown_MD5_LargeSectionFallback(t *testing.T) {
	t.Setenv("RAG_MAX_CHUNK_CHARS", "120")
	t.Setenv("RAG_MIN_CHUNK_CHARS", "40")
	t.Setenv("RAG_CHUNK_OVERLAP_CHARS", "10")

	var body strings.Builder
	for i := 0; i < 40; i++ {
		body.WriteString("Guards prevent invalid transitions when the machine is not ready. ")
	}
	path := writeTempMarkdown(t, "large.md", "# State Machine\n\n## Guards\n\n"+body.String()+"\n")
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	guardChunks := 0
	for _, c := range chunks {
		if c.Heading == "Guards" {
			guardChunks++
			if c.HeadingLevel != 2 {
				t.Fatalf("subchunk lost heading_level: %+v", c)
			}
			if len(c.HeadingPath) < 2 || c.HeadingPath[len(c.HeadingPath)-1] != "Guards" {
				t.Fatalf("subchunk lost heading_path: %+v", c.HeadingPath)
			}
			if !strings.Contains(c.Content, "Section: State Machine > Guards") {
				t.Fatalf("subchunk missing hierarchy in embedding text: %q", c.Content)
			}
		}
	}
	if guardChunks < 2 {
		t.Fatalf("expected oversized Guards section to split into multiple chunks, got %d", guardChunks)
	}
}

func TestMarkdown_MD6_Preamble(t *testing.T) {
	path := writeTempMarkdown(t, "preamble.md", `This document describes the runtime.

# Architecture

Body.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, c := range chunks {
		if c.HeadingLevel == 0 && strings.Contains(c.Content, "This document describes the runtime.") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("preamble before first heading was discarded")
	}
}

func TestMarkdown_MD7_Frontmatter(t *testing.T) {
	path := writeTempMarkdown(t, "fm.md", `---
title: State Machine Architecture
status: accepted
tags:
  - state-machine
  - runtime
---

# Overview

Body text about the machine.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	if chunks[0].Title != "State Machine Architecture" {
		t.Fatalf("expected frontmatter title, got %q", chunks[0].Title)
	}
	tagJoin := strings.Join(chunks[0].MetaTags, ",")
	if !strings.Contains(tagJoin, "state-machine") || !strings.Contains(tagJoin, "runtime") {
		t.Fatalf("expected frontmatter tags, got %v", chunks[0].MetaTags)
	}
	for _, c := range chunks {
		if strings.Contains(c.Content, "---") && strings.Contains(c.Content, "status: accepted") {
			t.Fatalf("raw frontmatter delimiters leaked into embedding content: %q", c.Content)
		}
	}
}

func TestMarkdown_MD8_RealisticArchitectureDoc(t *testing.T) {
	path := writeTempMarkdown(t, "state-machine.md", `# State Machine

The runtime owns a single authoritative state machine.

## States

Idle, Playing, and Paused are the primary states.

## Events

Play, Pause, and Stop drive transitions.

## Transitions

Transitions are validated before mutation.

### Guards

Guards prevent invalid transitions.

### Actions

Actions run side effects after a successful transition.

## Runtime

The runtime schedules events on the audio thread.
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantPaths := []string{
		"State Machine",
		"State Machine > States",
		"State Machine > Events",
		"State Machine > Transitions",
		"State Machine > Transitions > Guards",
		"State Machine > Transitions > Actions",
		"State Machine > Runtime",
	}
	got := map[string]bool{}
	for _, c := range chunks {
		if len(c.HeadingPath) > 0 {
			got[strings.Join(c.HeadingPath, " > ")] = true
		}
		if !strings.Contains(c.Content, "Document: State Machine") {
			t.Fatalf("missing document title in embedding: %q", c.Content)
		}
	}
	for _, w := range wantPaths {
		if !got[w] {
			t.Fatalf("missing section %q; have %v", w, got)
		}
	}
}

func TestParseTextToChunks_MarkdownStructural(t *testing.T) {
	// Replaces the old paragraph-merge expectation for headed Markdown documents.
	path := writeTempMarkdown(t, "test_doc.md", `
# Header 1

This is paragraph 1. It contains some text that we want to parse.

This is paragraph 2. It has another sentence.
`)
	doc, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("ParseTextToChunks failed: %v", err)
	}
	if doc.Type != "md" {
		t.Errorf("expected doc type 'md', got %q", doc.Type)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	found := false
	for _, c := range chunks {
		if c.Heading == "Header 1" &&
			strings.Contains(c.Content, "This is paragraph 1") &&
			strings.Contains(c.Content, "This is paragraph 2") {
			found = true
		}
		if c.PageNumber != 0 {
			t.Errorf("expected page number 0 for markdown, got %d", c.PageNumber)
		}
	}
	if !found {
		t.Fatalf("expected Header 1 section containing both paragraphs")
	}
}
