package ast

import (
	"crypto/sha256"
	"fmt"
	"strings"

	gherkin "github.com/cucumber/gherkin/go/v42"
	messages "github.com/cucumber/messages/go/v34"
)

// ParseFeatureToChunksFromContent parses a Gherkin .feature document into one
// ChunkNode per Scenario / Scenario Outline. Background steps are inlined into
// each scenario's embedding text (not stored as separate vectors). Scenario
// Outlines keep their Examples tables in the same chunk (no per-row expansion).
func ParseFeatureToChunksFromContent(filePath string, raw []byte) (DocumentNode, []ChunkNode, error) {
	docNode := DocumentNode{Path: filePath, Type: "feature"}
	content := normalizeNewlines(string(raw))
	if strings.TrimSpace(content) == "" {
		return DocumentNode{}, nil, fmt.Errorf("feature file %s is empty", filePath)
	}

	doc, err := gherkin.ParseGherkinDocument(strings.NewReader(content), (&messages.Incrementing{}).NewId)
	if err != nil {
		return DocumentNode{}, nil, fmt.Errorf("gherkin parse failed for %s: %w", filePath, err)
	}
	if doc.Feature == nil {
		return DocumentNode{}, nil, fmt.Errorf("feature file %s has no Feature", filePath)
	}

	feature := doc.Feature
	featureTags := tagNames(feature.Tags)
	var featureBackground *messages.Background
	var chunks []ChunkNode

	for _, child := range feature.Children {
		if child == nil {
			continue
		}
		if child.Background != nil {
			featureBackground = child.Background
			continue
		}
		if child.Scenario != nil {
			chunks = append(chunks, buildScenarioChunk(feature, "", featureTags, featureBackground, nil, child.Scenario)...)
			continue
		}
		if child.Rule != nil {
			rule := child.Rule
			var ruleBackground *messages.Background
			for _, rc := range rule.Children {
				if rc == nil {
					continue
				}
				if rc.Background != nil {
					ruleBackground = rc.Background
					continue
				}
				if rc.Scenario != nil {
					chunks = append(chunks, buildScenarioChunk(
						feature, rule.Name, featureTags, featureBackground, ruleBackground, rc.Scenario,
					)...)
				}
			}
		}
	}

	if len(chunks) == 0 {
		// Feature with only Background / description: emit a single feature-level chunk.
		text := formatFeatureOnlyEmbedding(feature, featureBackground)
		if strings.TrimSpace(text) == "" {
			return DocumentNode{}, nil, fmt.Errorf("feature file %s yielded no scenarios", filePath)
		}
		hash := sha256.Sum256([]byte(text))
		chunks = append(chunks, ChunkNode{
			Content:     text,
			Hash:        fmt.Sprintf("%x", hash),
			SourceKind:  "gherkin",
			Feature:     feature.Name,
			FeatureTags: featureTags,
			Title:       feature.Name,
			StartLine:   locationLine(feature.Location),
			EndLine:     locationLine(feature.Location),
		})
	}

	return docNode, chunks, nil
}

func buildScenarioChunk(
	feature *messages.Feature,
	ruleName string,
	featureTags []string,
	featureBG, ruleBG *messages.Background,
	scenario *messages.Scenario,
) []ChunkNode {
	if scenario == nil {
		return nil
	}
	scenarioTags := tagNames(scenario.Tags)
	body := formatScenarioEmbedding(feature, ruleName, featureBG, ruleBG, scenario)
	if strings.TrimSpace(body) == "" {
		return nil
	}

	maxChunkChars := envInt("RAG_MAX_CHUNK_CHARS", 4000)
	minChunkChars := envInt("RAG_MIN_CHUNK_CHARS", 800)
	overlapChars := envInt("RAG_CHUNK_OVERLAP_CHARS", 200)

	parts := []string{body}
	if len(body) > maxChunkChars {
		parts = mergeParagraphs(body, minChunkChars, maxChunkChars, overlapChars)
	}

	startLine := locationLine(scenario.Location)
	endLine := scenarioEndLine(scenario)
	var out []ChunkNode
	for _, part := range parts {
		if !IsCleanText(part) {
			continue
		}
		hash := sha256.Sum256([]byte(part))
		out = append(out, ChunkNode{
			Content:      part,
			Hash:         fmt.Sprintf("%x", hash),
			SourceKind:   "gherkin",
			Feature:      feature.Name,
			Rule:         ruleName,
			Scenario:     scenario.Name,
			FeatureTags:  append([]string(nil), featureTags...),
			ScenarioTags: append([]string(nil), scenarioTags...),
			Title:        feature.Name,
			StartLine:    startLine,
			EndLine:      endLine,
		})
	}
	return out
}

func formatFeatureOnlyEmbedding(feature *messages.Feature, bg *messages.Background) string {
	var b strings.Builder
	b.WriteString("Feature: ")
	b.WriteString(feature.Name)
	b.WriteString("\n")
	if desc := strings.TrimSpace(feature.Description); desc != "" {
		b.WriteString("\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}
	if bg != nil {
		b.WriteString("\n")
		writeBackground(&b, bg)
	}
	return strings.TrimSpace(b.String())
}

func formatScenarioEmbedding(
	feature *messages.Feature,
	ruleName string,
	featureBG, ruleBG *messages.Background,
	scenario *messages.Scenario,
) string {
	var b strings.Builder
	b.WriteString("Feature: ")
	b.WriteString(feature.Name)
	b.WriteString("\n")
	if ruleName != "" {
		b.WriteString("\nRule: ")
		b.WriteString(ruleName)
		b.WriteString("\n")
	}
	if featureBG != nil {
		b.WriteString("\n")
		writeBackground(&b, featureBG)
	}
	if ruleBG != nil {
		b.WriteString("\n")
		writeBackground(&b, ruleBG)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(scenario.Keyword))
	b.WriteString(": ")
	b.WriteString(scenario.Name)
	b.WriteString("\n")
	if desc := strings.TrimSpace(scenario.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n")
	}
	for _, step := range scenario.Steps {
		writeStep(&b, step)
	}
	for _, ex := range scenario.Examples {
		writeExamples(&b, ex)
	}
	return strings.TrimSpace(b.String())
}

func writeBackground(b *strings.Builder, bg *messages.Background) {
	b.WriteString(strings.TrimSpace(bg.Keyword))
	b.WriteString(":")
	if name := strings.TrimSpace(bg.Name); name != "" {
		b.WriteString(" ")
		b.WriteString(name)
	}
	b.WriteString("\n")
	for _, step := range bg.Steps {
		writeStep(b, step)
	}
}

func writeStep(b *strings.Builder, step *messages.Step) {
	if step == nil {
		return
	}
	b.WriteString(step.Keyword)
	b.WriteString(step.Text)
	b.WriteString("\n")
	if step.DocString != nil {
		delim := step.DocString.Delimiter
		if delim == "" {
			delim = "\"\"\""
		}
		b.WriteString("  ")
		b.WriteString(delim)
		if step.DocString.MediaType != "" {
			b.WriteString(step.DocString.MediaType)
		}
		b.WriteString("\n")
		b.WriteString(step.DocString.Content)
		if !strings.HasSuffix(step.DocString.Content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("  ")
		b.WriteString(delim)
		b.WriteString("\n")
	}
	if step.DataTable != nil {
		writeTable(b, step.DataTable.Rows)
	}
}

func writeExamples(b *strings.Builder, ex *messages.Examples) {
	if ex == nil {
		return
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(ex.Keyword))
	b.WriteString(":")
	if name := strings.TrimSpace(ex.Name); name != "" {
		b.WriteString(" ")
		b.WriteString(name)
	}
	b.WriteString("\n")
	var rows []*messages.TableRow
	if ex.TableHeader != nil {
		rows = append(rows, ex.TableHeader)
	}
	rows = append(rows, ex.TableBody...)
	writeTable(b, rows)
}

func writeTable(b *strings.Builder, rows []*messages.TableRow) {
	for _, row := range rows {
		if row == nil {
			continue
		}
		b.WriteString("  |")
		for _, cell := range row.Cells {
			val := ""
			if cell != nil {
				val = cell.Value
			}
			b.WriteString(" ")
			b.WriteString(val)
			b.WriteString(" |")
		}
		b.WriteString("\n")
	}
}

func tagNames(tags []*messages.Tag) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, t := range tags {
		if t == nil {
			continue
		}
		name := strings.TrimSpace(t.Name)
		name = strings.TrimPrefix(name, "@")
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func locationLine(loc *messages.Location) int {
	if loc == nil {
		return 0
	}
	return int(loc.Line)
}

func scenarioEndLine(scenario *messages.Scenario) int {
	end := locationLine(scenario.Location)
	for _, step := range scenario.Steps {
		if step == nil {
			continue
		}
		if l := locationLine(step.Location); l > end {
			end = l
		}
		if step.DocString != nil {
			if l := locationLine(step.DocString.Location); l > end {
				end = l
			}
			end += strings.Count(step.DocString.Content, "\n") + 1
		}
		if step.DataTable != nil {
			for _, row := range step.DataTable.Rows {
				if l := locationLine(row.Location); l > end {
					end = l
				}
			}
		}
	}
	for _, ex := range scenario.Examples {
		if ex == nil {
			continue
		}
		if l := locationLine(ex.Location); l > end {
			end = l
		}
		if ex.TableHeader != nil {
			if l := locationLine(ex.TableHeader.Location); l > end {
				end = l
			}
		}
		for _, row := range ex.TableBody {
			if l := locationLine(row.Location); l > end {
				end = l
			}
		}
	}
	return end
}
