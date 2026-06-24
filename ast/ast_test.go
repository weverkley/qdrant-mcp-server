package ast

import (
	"context"
	"os"
	"strings"
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
	if f2.Container != "Greeter" {
		t.Errorf("expected container Greeter, got %q", f2.Container)
	}
	if res.Namespace != "main" {
		t.Errorf("expected namespace/package main, got %q", res.Namespace)
	}
	if len(res.Types) != 1 || res.Types[0] != "Greeter" {
		t.Errorf("expected type Greeter, got %+v", res.Types)
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
	if len(res.Functions) > 1 && res.Functions[1].Container != "Math" {
		t.Errorf("expected method container Math, got %q", res.Functions[1].Container)
	}
}

func TestParseCSharpCode_Metadata(t *testing.T) {
	code := `
using Xunit;
namespace AgroOps.Application.Tests;

public class DtcFrameDecoderServiceTests
{
    public void DecodeFrame_ShouldReturnExpectedResult()
    {
    }
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "DtcFrameDecoderServiceTests.cs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	if res.Namespace != "AgroOps.Application.Tests" {
		t.Fatalf("expected namespace AgroOps.Application.Tests, got %q", res.Namespace)
	}
	if len(res.Types) == 0 || res.Types[0] != "DtcFrameDecoderServiceTests" {
		t.Fatalf("expected type DtcFrameDecoderServiceTests, got %+v", res.Types)
	}
	if len(res.Functions) == 0 || res.Functions[0].Container != "DtcFrameDecoderServiceTests" {
		t.Fatalf("expected method container DtcFrameDecoderServiceTests, got %+v", res.Functions)
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

	// Small paragraphs below the merge threshold are combined into a single
	// chunk to avoid over-fragmentation.
	if len(chunks) != 1 {
		t.Fatalf("expected 1 merged chunk, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0].Content, "This is paragraph 1. It contains some text that we want to parse.") ||
		!strings.Contains(chunks[0].Content, "This is paragraph 2. It has another sentence.") {
		t.Errorf("merged chunk missing expected paragraphs: %q", chunks[0].Content)
	}

	// Markdown has no physical pagination, so page number is reported as 0.
	if chunks[0].PageNumber != 0 {
		t.Errorf("expected page number 0 for non-paginated md, got %d", chunks[0].PageNumber)
	}
}
