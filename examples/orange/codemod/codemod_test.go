package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTransformOpenAIOpenAI reads testdata/openai_openai.input.go, runs the
// transform, and compares output to testdata/openai_openai.golden.go.
func TestTransformOpenAIOpenAI(t *testing.T) {
	inputPath := filepath.Join("testdata", "openai_openai.input.go")
	goldenPath := filepath.Join("testdata", "openai_openai.golden.go")

	// Check golden file exists.
	goldenSrc, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	// Run transform into a temp file.
	outPath := filepath.Join(t.TempDir(), "openai_openai.go")
	if err := runTransform(inputPath, outPath); err != nil {
		t.Fatalf("runTransform: %v", err)
	}

	gotSrc, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	// Normalize: collapse multiple blank lines to one for comparison.
	got := normalizeBlankLines(string(gotSrc))
	want := normalizeBlankLines(string(goldenSrc))

	if got != want {
		// Show a simple diff.
		gotLines := strings.Split(got, "\n")
		wantLines := strings.Split(want, "\n")
		t.Errorf("output mismatch (got %d lines, want %d lines):", len(gotLines), len(wantLines))
		maxLines := len(gotLines)
		if len(wantLines) > maxLines {
			maxLines = len(wantLines)
		}
		diffCount := 0
		for i := 0; i < maxLines; i++ {
			var g, w string
			if i < len(gotLines) {
				g = gotLines[i]
			}
			if i < len(wantLines) {
				w = wantLines[i]
			}
			if g != w {
				t.Errorf("  line %d:\n    got:  %q\n    want: %q", i+1, g, w)
				diffCount++
				if diffCount >= 20 {
					t.Errorf("  ... (truncated after 20 differences)")
					break
				}
			}
		}
	}
}

// TestTransformOutputParses verifies the output of runTransform on
// openai_openai.go can be parsed as valid Go (syntax check only).
func TestTransformOutputParses(t *testing.T) {
	inputPath := filepath.Join("testdata", "openai_openai.input.go")
	outPath := filepath.Join(t.TempDir(), "openai_openai.go")

	if err := runTransform(inputPath, outPath); err != nil {
		t.Fatalf("runTransform: %v", err)
	}

	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, outPath, nil, 0); err != nil {
		src, _ := os.ReadFile(outPath)
		t.Fatalf("output does not parse as valid Go: %v\n\n--- output ---\n%s", err, src)
	}
}

// TestSyncApischema is an integration smoke test: syncs apischema from the
// real ai-gateway repo and verifies the output parses as valid Go.
// Skipped if ai-gateway is not present.
func TestSyncApischema(t *testing.T) {
	upstream := "/Users/dio/src/dio/ai-gateway"
	if _, err := os.Stat(upstream); err != nil {
		t.Skipf("ai-gateway not present at %s: %v", upstream, err)
	}

	outDir := filepath.Join(t.TempDir(), "apischema")
	if err := syncApischema(upstream, outDir); err != nil {
		t.Fatalf("syncApischema: %v", err)
	}

	// Verify each copied file parses.
	for _, pkg := range []string{"openai", "anthropic", "awsbedrock"} {
		pkgDir := filepath.Join(outDir, pkg)
		entries, err := os.ReadDir(pkgDir)
		if err != nil {
			t.Errorf("readdir %s: %v", pkgDir, err)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			filePath := filepath.Join(pkgDir, entry.Name())
			fset := token.NewFileSet()
			if _, err := parser.ParseFile(fset, filePath, nil, 0); err != nil {
				t.Errorf("parse %s: %v", filePath, err)
			}
		}
	}
}

// TestSyncHelpers is an integration smoke test: syncs helper files from the
// real ai-gateway repo and verifies the output parses as valid Go.
// Skipped if ai-gateway is not present.
func TestSyncHelpers(t *testing.T) {
	upstream := "/Users/dio/src/dio/ai-gateway"
	if _, err := os.Stat(upstream); err != nil {
		t.Skipf("ai-gateway not present at %s: %v", upstream, err)
	}

	outDir := t.TempDir()
	if err := syncHelpers(upstream, outDir); err != nil {
		t.Fatalf("syncHelpers: %v", err)
	}

	// Verify each synced file parses.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", outDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		filePath := filepath.Join(outDir, entry.Name())
		fset := token.NewFileSet()
		if _, err := parser.ParseFile(fset, filePath, nil, 0); err != nil {
			t.Errorf("parse %s: %v", filePath, err)
		}
	}
}

// normalizeBlankLines collapses sequences of blank lines to a single blank line.
func normalizeBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	result := make([]string, 0, len(lines))
	lastWasBlank := false
	for _, line := range lines {
		isBlank := strings.TrimSpace(line) == ""
		if isBlank && lastWasBlank {
			continue
		}
		result = append(result, line)
		lastWasBlank = isBlank
	}
	return strings.Join(result, "\n")
}
