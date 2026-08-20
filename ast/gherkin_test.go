package ast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFeature(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp feature: %v", err)
	}
	return path
}

func TestGherkin_FEATURE1_SimpleScenario(t *testing.T) {
	path := writeTempFeature(t, "simple.feature", `Feature: State Machine

  Scenario: Play from Idle
    Given state is Idle
    When Play is received
    Then state becomes Playing
`)
	doc, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc.Type != "feature" {
		t.Fatalf("expected type feature, got %q", doc.Type)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 scenario chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.SourceKind != "gherkin" || c.Feature != "State Machine" || c.Scenario != "Play from Idle" {
		t.Fatalf("unexpected metadata: %+v", c)
	}
	for _, want := range []string{
		"Feature: State Machine",
		"Scenario: Play from Idle",
		"Given state is Idle",
		"When Play is received",
		"Then state becomes Playing",
	} {
		if !strings.Contains(c.Content, want) {
			t.Fatalf("missing %q in %q", want, c.Content)
		}
	}
}

func TestGherkin_FEATURE2_MultipleScenarios(t *testing.T) {
	path := writeTempFeature(t, "multi.feature", `Feature: State Machine

  Scenario: Play from Idle
    Given state is Idle
    When Play is received
    Then state becomes Playing

  Scenario: Pause while Playing
    Given state is Playing
    When Pause is received
    Then state becomes Paused
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 scenario chunks, got %d", len(chunks))
	}
	names := []string{chunks[0].Scenario, chunks[1].Scenario}
	joined := strings.Join(names, "|")
	if !strings.Contains(joined, "Play from Idle") || !strings.Contains(joined, "Pause while Playing") {
		t.Fatalf("expected independent scenarios, got %v", names)
	}
}

func TestGherkin_FEATURE3_Background(t *testing.T) {
	path := writeTempFeature(t, "bg.feature", `Feature: State Machine

  Background:
    Given a state machine exists

  Scenario: Play from Idle
    Given state is Idle
    When Play is received
    Then state becomes Playing
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 scenario chunk (background inlined), got %d", len(chunks))
	}
	c := chunks[0]
	if c.Scenario != "Play from Idle" {
		t.Fatalf("chunk should identify scenario, got %q", c.Scenario)
	}
	if !strings.Contains(c.Content, "Background:") || !strings.Contains(c.Content, "a state machine exists") {
		t.Fatalf("background context missing from embedding: %q", c.Content)
	}
}

func TestGherkin_FEATURE4_Tags(t *testing.T) {
	path := writeTempFeature(t, "tags.feature", `@state-machine
Feature: State Machine

  @runtime
  Scenario: Play from Idle
    Given state is Idle
    When Play is received
    Then state becomes Playing
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := chunks[0]
	ft := strings.Join(c.FeatureTags, ",")
	st := strings.Join(c.ScenarioTags, ",")
	if !strings.Contains(ft, "state-machine") {
		t.Fatalf("feature tags missing: %v", c.FeatureTags)
	}
	if !strings.Contains(st, "runtime") {
		t.Fatalf("scenario tags missing: %v", c.ScenarioTags)
	}
}

func TestGherkin_FEATURE5_ScenarioOutline(t *testing.T) {
	path := writeTempFeature(t, "outline.feature", `Feature: Gain

  Scenario Outline: Set gain
    Given gain is <initial>
    When gain becomes <target>
    Then output gain should be <target>

    Examples:
      | initial | target |
      | 0.0     | 0.5    |
      | 0.5     | 1.0    |
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected single outline+examples chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.Scenario != "Set gain" {
		t.Fatalf("unexpected scenario name %q", c.Scenario)
	}
	if !strings.Contains(c.Content, "Examples:") ||
		!strings.Contains(c.Content, "0.0") ||
		!strings.Contains(c.Content, "1.0") {
		t.Fatalf("examples table not associated: %q", c.Content)
	}
}

func TestGherkin_FEATURE6_Rule(t *testing.T) {
	path := writeTempFeature(t, "rule.feature", `Feature: State Machine

  Rule: Transitions require valid events

    Scenario: Reject unknown event
      Given state is Idle
      When event "Unknown" is received
      Then the transition is rejected
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.Feature != "State Machine" || c.Rule != "Transitions require valid events" || c.Scenario != "Reject unknown event" {
		t.Fatalf("rule hierarchy not preserved: feature=%q rule=%q scenario=%q", c.Feature, c.Rule, c.Scenario)
	}
	if !strings.Contains(c.Content, "Rule: Transitions require valid events") {
		t.Fatalf("rule missing from embedding: %q", c.Content)
	}
}

func TestGherkin_FEATURE7_DataTable(t *testing.T) {
	path := writeTempFeature(t, "table.feature", `Feature: Transitions

  Scenario: Load transition table
    Given the following transitions:
      | from | event | to      |
      | Idle | Play  | Playing |
    Then the table is accepted
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := chunks[0]
	if !strings.Contains(c.Content, "| Idle | Play | Playing |") &&
		!strings.Contains(c.Content, "Idle") {
		t.Fatalf("data table not attached: %q", c.Content)
	}
	if !strings.Contains(c.Content, "from") || !strings.Contains(c.Content, "Playing") {
		t.Fatalf("data table cells missing: %q", c.Content)
	}
}

func TestGherkin_FEATURE8_DocString(t *testing.T) {
	path := writeTempFeature(t, "docstring.feature", `Feature: Config

  Scenario: Load JSON config
    Given the configuration:
      """
      {
        "state": "Idle"
      }
      """
    Then configuration is valid
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := chunks[0]
	if !strings.Contains(c.Content, `"state": "Idle"`) {
		t.Fatalf("doc string not attached: %q", c.Content)
	}
}

func TestGherkin_FEATURE9_RealisticStateMachine(t *testing.T) {
	path := writeTempFeature(t, "state-machine.feature", `@state-machine
Feature: State machine transitions

  Background:
    Given a state machine exists

  @runtime
  Scenario: Transition when Play is received
    Given the state machine is in "Idle"
    When the event "Play" is received
    Then the state machine enters "Playing"

  Scenario: Pause while Playing
    Given the state machine is in "Playing"
    When the event "Pause" is received
    Then the state machine enters "Paused"

  Scenario: Stop from Paused
    Given the state machine is in "Paused"
    When the event "Stop" is received
    Then the state machine enters "Idle"

  Rule: Invalid events are rejected

    Scenario: Pause from Idle is rejected
      Given the state machine is in "Idle"
      When the event "Pause" is received
      Then the transition is rejected
`)
	_, chunks, err := ParseTextToChunks(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(chunks) < 4 {
		t.Fatalf("expected at least 4 scenarios, got %d", len(chunks))
	}
	var names []string
	for _, c := range chunks {
		names = append(names, c.Scenario)
		if c.Feature != "State machine transitions" {
			t.Fatalf("feature name mismatch: %q", c.Feature)
		}
		if !strings.Contains(c.Content, "Background:") {
			t.Fatalf("background should be inlined for %q", c.Scenario)
		}
	}
	joined := strings.Join(names, "|")
	for _, want := range []string{
		"Transition when Play is received",
		"Pause while Playing",
		"Stop from Paused",
		"Pause from Idle is rejected",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing scenario %q in %v", want, names)
		}
	}
}
