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

func TestParseTextToChunks_PlainText(t *testing.T) {
	tempFile, err := os.CreateTemp("", "test_doc_*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tempFile.Name())

	content := `
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

	if doc.Type != "txt" {
		t.Errorf("expected doc type 'txt', got %q", doc.Type)
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

	if chunks[0].PageNumber != 0 {
		t.Errorf("expected page number 0 for non-paginated txt, got %d", chunks[0].PageNumber)
	}
}

func TestParseRustCode_FreeFunction(t *testing.T) {
	code := `
pub fn create_engine() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "free_fn.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(res.Functions))
	}
	fn := res.Functions[0]
	if fn.Name != "create_engine" {
		t.Errorf("expected name create_engine, got %q", fn.Name)
	}
	if fn.Language != "rust" {
		t.Errorf("expected language rust, got %q", fn.Language)
	}
}

func TestParseRustCode_Struct(t *testing.T) {
	code := `
pub struct Engine {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "struct_engine.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "Engine" {
		t.Fatalf("expected type Engine, got %+v", res.Types)
	}
}

func TestParseRustCode_ImplMethod(t *testing.T) {
	code := `
impl Engine {
    pub fn process(&mut self) {}
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "impl_method.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseRustCode_MultipleImplMethods(t *testing.T) {
	code := `
impl Engine {
    pub fn new() -> Self {
        Self {}
    }
    pub fn start(&mut self) {}
    pub fn stop(&mut self) {}
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "impl_multi.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	names := map[string]bool{}
	for _, fn := range res.Functions {
		names[fn.Name] = true
		if fn.Container != "Engine" {
			t.Errorf("expected container Engine for %s, got %q", fn.Name, fn.Container)
		}
	}
	for _, want := range []string{"new", "start", "stop"} {
		if !names[want] {
			t.Errorf("missing method %s in %+v", want, res.Functions)
		}
	}
}

func TestParseRustCode_TraitMethod(t *testing.T) {
	code := `
trait Processor {
    fn process(&mut self);
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "trait_method.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "Processor" {
		t.Fatalf("expected type Processor, got %+v", res.Types)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Container != "Processor" {
		t.Errorf("expected container Processor, got %q", fn.Container)
	}
}

func TestParseRustCode_TraitImpl(t *testing.T) {
	code := `
impl Processor for Engine {
    fn process(&mut self) {}
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "trait_impl.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseRustCode_Imports(t *testing.T) {
	code := `
use crate::runtime::Runtime;
use std::sync::{Arc, Mutex};

pub fn run() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "imports.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Imports) < 2 {
		t.Fatalf("expected at least 2 imports, got %+v", res.Imports)
	}
	joined := ""
	for _, imp := range res.Imports {
		joined += imp.RawPath + "\n"
	}
	if !strings.Contains(joined, "crate::runtime::Runtime") {
		t.Errorf("expected crate::runtime::Runtime import, got %+v", res.Imports)
	}
	if !strings.Contains(joined, "std::sync") {
		t.Errorf("expected std::sync import fragment, got %+v", res.Imports)
	}
}

func TestParseRustCode_AsyncUnsafeGenerics(t *testing.T) {
	code := `
pub async fn start() {}
pub unsafe fn process() {}
pub fn process_generic<T: Processor>(value: T) {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "async_unsafe_generic.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	names := map[string]bool{}
	for _, fn := range res.Functions {
		names[fn.Name] = true
	}
	for _, want := range []string{"start", "process", "process_generic"} {
		if !names[want] {
			t.Errorf("missing function %s in %+v", want, res.Functions)
		}
	}
}

func TestParseRustCode_StateMachine(t *testing.T) {
	code := `
pub struct StateMachine {
    current_state: StateId,
}

impl StateMachine {
    pub fn new(initial: StateId) -> Self {
        Self { current_state: initial }
    }

    pub fn transition(&mut self, event: &Event) -> Result<(), Error> {
        Ok(())
    }

    fn resolve_transition(&self, event: &Event) -> Option<StateId> {
        None
    }
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "state_machine.rs", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "StateMachine" {
		t.Fatalf("expected type StateMachine, got %+v", res.Types)
	}
	names := map[string]string{}
	for _, fn := range res.Functions {
		names[fn.Name] = fn.Container
	}
	for _, want := range []string{"new", "transition", "resolve_transition"} {
		container, ok := names[want]
		if !ok {
			t.Errorf("missing method %s in %+v", want, res.Functions)
			continue
		}
		if container != "StateMachine" {
			t.Errorf("expected container StateMachine for %s, got %q", want, container)
		}
	}
}

func TestParseCCode_Function(t *testing.T) {
	code := `
int add(int a, int b) {
    return a + b;
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "add.c", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "add" {
		t.Errorf("expected name add, got %q", fn.Name)
	}
	if fn.Language != "c" {
		t.Errorf("expected language c, got %q", fn.Language)
	}
}

func TestParseCCode_Struct(t *testing.T) {
	code := `
struct Engine {
    int state;
};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "engine.h", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "Engine" {
		t.Fatalf("expected type Engine, got %+v", res.Types)
	}
}

func TestParseCCode_TypedefStruct(t *testing.T) {
	code := `
typedef struct DinEngine DinEngine;
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "din_c_api.h", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	found := false
	for _, typ := range res.Types {
		if typ == "DinEngine" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected type DinEngine, got %+v", res.Types)
	}
}

func TestParseCCode_Enum(t *testing.T) {
	code := `
enum DinState {
    DIN_IDLE,
    DIN_RUNNING
};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "state.h", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "DinState" {
		t.Fatalf("expected type DinState, got %+v", res.Types)
	}
}

func TestParseCCode_Includes(t *testing.T) {
	code := `
#include <stdint.h>
#include "din_c_api.h"
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "includes.c", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	joined := ""
	for _, imp := range res.Imports {
		joined += imp.RawPath + "\n"
	}
	if !strings.Contains(joined, "stdint.h") {
		t.Errorf("expected stdint.h import, got %+v", res.Imports)
	}
	if !strings.Contains(joined, "din_c_api.h") {
		t.Errorf("expected din_c_api.h import, got %+v", res.Imports)
	}
}

func TestParseCCode_FunctionDeclaration(t *testing.T) {
	code := `
DinEngine *din_engine_create(
    const DinConfig *config
);
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "api.h", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	if res.Functions[0].Name != "din_engine_create" {
		t.Errorf("expected din_engine_create, got %q", res.Functions[0].Name)
	}
}

func TestParseCCode_FunctionDefinition(t *testing.T) {
	code := `
DinEngine *din_engine_create(
    const DinConfig *config
) {
    return 0;
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "api.c", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	if res.Functions[0].Name != "din_engine_create" {
		t.Errorf("expected din_engine_create, got %q", res.Functions[0].Name)
	}
}

func TestParseCCode_StaticFunction(t *testing.T) {
	code := `
static void reset(void) {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "static.c", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 || res.Functions[0].Name != "reset" {
		t.Fatalf("expected reset, got %+v", res.Functions)
	}
}

func TestParseCCode_PointerCallback(t *testing.T) {
	code := `
void din_engine_set_callback(
    DinEngine *engine,
    void (*callback)(const DinEvent *event, void *userdata),
    void *userdata
) {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "callback.c", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 || res.Functions[0].Name != "din_engine_set_callback" {
		t.Fatalf("expected din_engine_set_callback, got %+v", res.Functions)
	}
}

func TestParseCppCode_FreeFunction(t *testing.T) {
	code := `
void process() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "process.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(res.Functions))
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Language != "cpp" {
		t.Errorf("expected language cpp, got %q", fn.Language)
	}
}

func TestParseCppCode_Class(t *testing.T) {
	code := `
class Engine {};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "engine.hpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "Engine" {
		t.Fatalf("expected type Engine, got %+v", res.Types)
	}
}

func TestParseCppCode_InlineMethod(t *testing.T) {
	code := `
class Engine {
public:
    void process() {}
};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "engine.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseCppCode_OutOfClassMethod(t *testing.T) {
	code := `
void Engine::process() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "engine_method.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "process" {
		t.Errorf("expected name process, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseCppCode_Constructor(t *testing.T) {
	code := `
Engine::Engine() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "ctor.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "Engine" {
		t.Errorf("expected name Engine, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseCppCode_Destructor(t *testing.T) {
	code := `
Engine::~Engine() {}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "dtor.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d (%+v)", len(res.Functions), res.Functions)
	}
	fn := res.Functions[0]
	if fn.Name != "~Engine" {
		t.Errorf("expected name ~Engine, got %q", fn.Name)
	}
	if fn.Container != "Engine" {
		t.Errorf("expected container Engine, got %q", fn.Container)
	}
}

func TestParseCppCode_Namespace(t *testing.T) {
	code := `
namespace din {
    class Engine {};
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "ns.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if res.Namespace != "din" {
		t.Errorf("expected namespace din, got %q", res.Namespace)
	}
	if len(res.Types) != 1 || res.Types[0] != "Engine" {
		t.Fatalf("expected type Engine, got %+v", res.Types)
	}
}

func TestParseCppCode_EnumClass(t *testing.T) {
	code := `
enum class State {
    Idle,
    Running
};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "state.hpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "State" {
		t.Fatalf("expected type State, got %+v", res.Types)
	}
}

func TestParseCppCode_TemplateFunction(t *testing.T) {
	code := `
template<typename T>
T identity(T value) {
    return value;
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "identity.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 || res.Functions[0].Name != "identity" {
		t.Fatalf("expected identity, got %+v", res.Functions)
	}
}

func TestParseCppCode_TemplateClass(t *testing.T) {
	code := `
template<typename T>
class Processor {
public:
    void process(T value) {}
};
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "processor.hpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Types) != 1 || res.Types[0] != "Processor" {
		t.Fatalf("expected type Processor, got %+v", res.Types)
	}
	if len(res.Functions) != 1 || res.Functions[0].Name != "process" {
		t.Fatalf("expected process, got %+v", res.Functions)
	}
	if res.Functions[0].Container != "Processor" {
		t.Errorf("expected container Processor, got %q", res.Functions[0].Container)
	}
}

func TestParseCppCode_QualifiedCall(t *testing.T) {
	code := `
void run() {
    din::Engine::create();
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "calls.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(res.Functions))
	}
	found := false
	for _, call := range res.Functions[0].Calls {
		if call.Name == "din::Engine::create" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected call din::Engine::create, got %+v", res.Functions[0].Calls)
	}
}

func TestParseCppCode_PointerMethodCall(t *testing.T) {
	code := `
void run(Engine* engine) {
    engine->process();
}
`
	ctx := context.Background()
	res, err := ParseCodeToDocsWithMeta(ctx, "ptr_call.cpp", []byte(code))
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(res.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(res.Functions))
	}
	found := false
	for _, call := range res.Functions[0].Calls {
		if call.Name == "engine->process" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected call engine->process, got %+v", res.Functions[0].Calls)
	}
}

func TestParseNativeAPICAndCppFixture(t *testing.T) {
	header := `
typedef struct DinEngine DinEngine;

typedef struct DinEngineConfig {
    uint32_t sample_rate;
    uint32_t buffer_size;
} DinEngineConfig;

DinEngine *din_engine_create(
    const DinEngineConfig *config
);

void din_engine_destroy(DinEngine *engine);

int din_engine_process(
    DinEngine *engine,
    const float *input,
    float *output,
    uint32_t frames
);
`
	impl := `
DinEngine *din_engine_create(const DinEngineConfig *config) {
    return 0;
}

void din_engine_destroy(DinEngine *engine) {}

int din_engine_process(
    DinEngine *engine,
    const float *input,
    float *output,
    uint32_t frames
) {
    return 0;
}
`
	cppCode := `
namespace din {

class AudioRuntime {
public:
    AudioRuntime();
    ~AudioRuntime();

    void start();
    void stop();
    void process(float* buffer, size_t frames);
};

}
`
	ctx := context.Background()

	headerRes, err := ParseCodeToDocsWithMeta(ctx, "fixtures/native/din_c_api.h", []byte(header))
	if err != nil {
		t.Fatalf("header parse failed: %v", err)
	}
	implRes, err := ParseCodeToDocsWithMeta(ctx, "fixtures/native/din_c_api.c", []byte(impl))
	if err != nil {
		t.Fatalf("impl parse failed: %v", err)
	}
	cppRes, err := ParseCodeToDocsWithMeta(ctx, "fixtures/native/runtime.hpp", []byte(cppCode))
	if err != nil {
		t.Fatalf("cpp parse failed: %v", err)
	}

	typeSet := map[string]bool{}
	for _, typ := range headerRes.Types {
		typeSet[typ] = true
	}
	for _, want := range []string{"DinEngine", "DinEngineConfig"} {
		if !typeSet[want] {
			t.Errorf("header missing type %s in %+v", want, headerRes.Types)
		}
	}

	headerFns := map[string]bool{}
	for _, fn := range headerRes.Functions {
		headerFns[fn.Name] = true
	}
	for _, want := range []string{"din_engine_create", "din_engine_destroy", "din_engine_process"} {
		if !headerFns[want] {
			t.Errorf("header missing function %s in %+v", want, headerRes.Functions)
		}
	}

	implFns := map[string]bool{}
	for _, fn := range implRes.Functions {
		implFns[fn.Name] = true
	}
	for _, want := range []string{"din_engine_create", "din_engine_destroy", "din_engine_process"} {
		if !implFns[want] {
			t.Errorf("impl missing function %s in %+v", want, implRes.Functions)
		}
	}

	if cppRes.Namespace != "din" {
		t.Errorf("expected namespace din, got %q", cppRes.Namespace)
	}
	if len(cppRes.Types) != 1 || cppRes.Types[0] != "AudioRuntime" {
		t.Errorf("expected type AudioRuntime, got %+v", cppRes.Types)
	}
	cppFns := map[string]string{}
	for _, fn := range cppRes.Functions {
		cppFns[fn.Name] = fn.Container
	}
	for _, want := range []string{"AudioRuntime", "~AudioRuntime", "start", "stop", "process"} {
		container, ok := cppFns[want]
		if !ok {
			t.Errorf("cpp missing function %s in %+v", want, cppRes.Functions)
			continue
		}
		if container != "AudioRuntime" {
			t.Errorf("expected container AudioRuntime for %s, got %q", want, container)
		}
	}
}
