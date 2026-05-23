package ast

import (
	"context"
	"os"
	"testing"
)

func TestParseGoCode(t *testing.T) {
	code := `
package main

import "fmt"

// Hello is a greeting function
func Hello(name string) string {
	return fmt.Sprintf("Hello, %s", name)
}

type Greeter struct{}

func (g Greeter) Greet(name string) {
	fmt.Println("Hi " + name)
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "main.go", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if len(res.Functions) != 2 {
		t.Fatalf("expected 2 functions, got %d", len(res.Functions))
	}

	f1 := res.Functions[0]
	if f1.Name != "Hello" {
		t.Errorf("expected function name Hello, got %s", f1.Name)
	}
	if f1.StartLine != 7 {
		t.Errorf("expected function start line 7, got %d", f1.StartLine)
	}

	f2 := res.Functions[1]
	if f2.Name != "Greet" {
		t.Errorf("expected function name Greet, got %s", f2.Name)
	}
	if f2.Receiver != "(g Greeter)" && f2.Receiver != "g Greeter" {
		t.Errorf("expected receiver g Greeter or (g Greeter), got %q", f2.Receiver)
	}

	if len(res.Imports) != 1 || res.Imports[0].RawPath != "fmt" {
		t.Errorf("expected import 'fmt', got %+v", res.Imports)
	}
}

func TestParsePythonCode(t *testing.T) {
	code := `
import os

def calculate_sum(a, b):
    """Adds two numbers"""
    return a + b

class Math:
    def multiply(self, x, y):
        return x * y
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "script.py", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	// Python function definition matches calculate_sum
	if len(res.Functions) == 0 {
		t.Fatalf("expected at least 1 function, got 0")
	}

	f1 := res.Functions[0]
	if f1.Name != "calculate_sum" {
		t.Errorf("expected calculate_sum, got %s", f1.Name)
	}

	if len(res.Imports) != 1 || res.Imports[0].RawPath != "os" {
		t.Errorf("expected import os, got %+v", res.Imports)
	}
}

func TestParseTextToChunks(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_doc_*.md")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := `
# Header 1

This is paragraph 1. It contains some text that we want to parse.

This is paragraph 2. It has another sentence.
`
	if _, err := tempFile.Write([]byte(content)); err != nil {
		t.Fatalf("failed to write content: %v", err)
	}
	tempFile.Close()

	doc, chunks, err := ParseTextToChunks(tempFile.Name())
	if err != nil {
		t.Fatalf("ParseTextToChunks failed: %v", err)
	}

	if doc.Type != "md" {
		t.Errorf("expected doc type 'md', got %q", doc.Type)
	}

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	if chunks[1].Content != "This is paragraph 1. It contains some text that we want to parse." {
		t.Errorf("unexpected content in chunk 1: %q", chunks[1].Content)
	}
}
