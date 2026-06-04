package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// importRewrites applies all import path substitutions needed for
// both sync-apischema and sync-helpers modes.
var commonImportRewrites = []struct{ from, to string }{
	{
		from: `"github.com/envoyproxy/ai-gateway/internal/apischema/openai"`,
		to:   `"github.com/dio/transit/examples/orange/internal/apischema/openai"`,
	},
	{
		from: `"github.com/envoyproxy/ai-gateway/internal/apischema/anthropic"`,
		to:   `"github.com/dio/transit/examples/orange/internal/apischema/anthropic"`,
	},
	{
		from: `"github.com/envoyproxy/ai-gateway/internal/apischema/awsbedrock"`,
		to:   `"github.com/dio/transit/examples/orange/internal/apischema/awsbedrock"`,
	},
	{
		from: `"github.com/envoyproxy/ai-gateway/internal/json"`,
		to:   `"encoding/json"`,
	},
	// Also handle non-quoted forms that may appear in replace directives.
	{
		from: "github.com/envoyproxy/ai-gateway/internal/apischema/openai",
		to:   "github.com/dio/transit/examples/orange/internal/apischema/openai",
	},
	{
		from: "github.com/envoyproxy/ai-gateway/internal/apischema/anthropic",
		to:   "github.com/dio/transit/examples/orange/internal/apischema/anthropic",
	},
	{
		from: "github.com/envoyproxy/ai-gateway/internal/apischema/awsbedrock",
		to:   "github.com/dio/transit/examples/orange/internal/apischema/awsbedrock",
	},
	{
		from: "github.com/envoyproxy/ai-gateway/internal/json",
		to:   "encoding/json",
	},
}

// helperDropImports are imports removed from helper files.
var helperDropImports = []string{
	`"github.com/envoyproxy/ai-gateway/internal/internalapi"`,
	`"github.com/envoyproxy/ai-gateway/internal/metrics"`,
	`"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi"`,
}

// helperTypeRewrites are string-level type replacements in helper files.
var helperTypeRewrites = []struct{ from, to string }{
	{"internalapi.Header", "Header"},
	{"internalapi.ModelNameOverride", "string"},
	{"internalapi.ResponseModel", "string"},
}

// syncApischema copies non-test .go files from
// ai-gateway/internal/apischema/{openai,anthropic,awsbedrock}/ to outDir/<pkg>/,
// applying import path rewrites.
func syncApischema(upstreamRoot, outDir string) error {
	srcBase := filepath.Join(upstreamRoot, "internal", "apischema")
	pkgs := []string{"openai", "anthropic", "awsbedrock"}

	for _, pkg := range pkgs {
		srcPkg := filepath.Join(srcBase, pkg)
		dstPkg := filepath.Join(outDir, pkg)

		entries, err := os.ReadDir(srcPkg)
		if err != nil {
			return fmt.Errorf("readdir %s: %w", srcPkg, err)
		}

		if err := os.MkdirAll(dstPkg, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dstPkg, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			// Skip test files.
			if strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}

			srcFile := filepath.Join(srcPkg, entry.Name())
			dstFile := filepath.Join(dstPkg, entry.Name())

			if err := syncGoFile(srcFile, dstFile, commonImportRewrites, nil, nil); err != nil {
				return fmt.Errorf("sync %s: %w", entry.Name(), err)
			}
			fmt.Printf("synced: %s\n", dstFile)
		}
	}
	return nil
}

// helperFiles lists the helper files to copy for sync-helpers.
// anthropic_usage.go is intentionally excluded: token usage extraction is
// owned by pipeline/meter, not the translator layer.
var helperFiles = []string{
	"util.go",
	"openai_helper.go",
	"anthropic_helper.go",
	"gemini_helper.go",
	"jsonschema_helper.go",
}

// syncHelpers copies the named helper files from ai-gateway/internal/translator/
// to outDir/, applying import rewrites and dropping metrics/tracing.
func syncHelpers(upstreamRoot, outDir string) error {
	srcDir := filepath.Join(upstreamRoot, "internal", "translator")

	for _, name := range helperFiles {
		srcFile := filepath.Join(srcDir, name)
		dstFile := filepath.Join(outDir, name)

		// Also copy _test.go companion if it exists.
		testName := strings.TrimSuffix(name, ".go") + "_test.go"
		testSrc := filepath.Join(srcDir, testName)
		testDst := filepath.Join(outDir, testName)

		if err := syncHelperFile(srcFile, dstFile); err != nil {
			return fmt.Errorf("sync %s: %w", name, err)
		}
		fmt.Printf("synced: %s\n", dstFile)

		if _, err := os.Stat(testSrc); err == nil {
			if err := syncHelperFile(testSrc, testDst); err != nil {
				return fmt.Errorf("sync %s: %w", testName, err)
			}
			fmt.Printf("synced: %s\n", testDst)
		}
	}
	return nil
}

// syncHelperFile copies a single helper file with all rewrites applied.
func syncHelperFile(src, dst string) error {
	// Build combined rewrites: common + helper-specific drops and type rewrites.
	rewrites := append([]struct{ from, to string }{}, commonImportRewrites...)

	// Add helper type rewrites.
	rewrites = append(rewrites, helperTypeRewrites...)

	return syncGoFile(src, dst, rewrites, helperDropImports, []struct{ from, to string }{
		// Replace internalapi.Header → Header, ModelNameOverride → string everywhere.
	})
}

// syncGoFile reads src, applies string replacements, and writes dst.
func syncGoFile(src, dst string, rewrites []struct{ from, to string }, dropImports []string, _ []struct{ from, to string }) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	content := string(data)

	// Apply import drops first (remove the whole quoted import line).
	for _, imp := range dropImports {
		// Remove the import line (tab-indented form).
		content = strings.ReplaceAll(content, "\t"+imp+"\n", "")
		// Also remove if it appears without tab.
		content = strings.ReplaceAll(content, imp+"\n", "")
	}

	// Apply string rewrites.
	for _, r := range rewrites {
		content = strings.ReplaceAll(content, r.from, r.to)
	}

	// Add metrics TODO comments: replace `metrics.` calls with CODEMOD-TODO.
	content = addMetricsTODOs(content)

	// Remove tracingapi references entirely.
	content = removeTracingapiRefs(content)

	// Remove slog/redaction references.
	content = removeSlogRedactionRefs(content)

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return os.WriteFile(dst, []byte(content), 0o644)
}

// addMetricsTODOs handles metrics.* references in helper files.
//   - Statement-level assignments like `x = metrics.Foo(...)` → CODEMOD-TODO comment
//     (handles multi-line calls by tracking paren depth).
//   - Type references like `metrics.TokenUsage` in struct fields / func signatures
//     are replaced with `interface{}` with a CODEMOD-TODO comment.
func addMetricsTODOs(content string) string {
	lines := strings.Split(content, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		if !strings.Contains(line, "metrics.") {
			result = append(result, line)
			i++
			continue
		}

		trimmed := strings.TrimSpace(line)
		indent := leadingWhitespace(line)

		// Detect type references (not assignments):
		// - struct field: `fieldName metrics.TokenUsage`
		// - func param/result: `) (... metrics.TokenUsage ...)`
		// - return statement: `return ..., metrics.TokenUsage{}, ...`
		isTypeRef := strings.Contains(line, "metrics.TokenUsage") &&
			!strings.Contains(trimmed, "= metrics.") &&
			!strings.HasPrefix(trimmed, "metrics.")

		if isTypeRef {
			// Replace metrics.TokenUsage{} literal (zero value in return) with nil.
			newLine := strings.ReplaceAll(line, "metrics.TokenUsage{}", "nil")
			// Replace metrics.TokenUsage as type reference with interface{}.
			newLine = strings.ReplaceAll(newLine, "metrics.TokenUsage", "interface{} /* CODEMOD-TODO: metrics.TokenUsage */")
			result = append(result, newLine)
			i++
			continue
		}

		// Statement-level call: replace and skip multi-line.
		result = append(result, indent+"// CODEMOD-TODO: wire token usage via Envoy stats")
		depth := 0
		for _, ch := range line {
			switch ch {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		i++
		for depth > 0 && i < len(lines) {
			for _, ch := range lines[i] {
				switch ch {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			i++
		}
	}
	return strings.Join(result, "\n")
}

// removeTracingapiRefs rewrites tracingapi.* references in helper files.
// Rather than dropping lines (which breaks multi-line signatures), we replace
// tracingapi.* type references with `any` and remove pure span-call lines.
func removeTracingapiRefs(content string) string {
	// Replace tracingapi span types with `any` in function signatures.
	// e.g. `span tracingapi.ChatCompletionSpan` → `_ any`
	re := regexp.MustCompile(`\w+ tracingapi\.\w+`)
	content = re.ReplaceAllString(content, "_ any")

	// Remove the import line.
	content = strings.ReplaceAll(content,
		"\t\"github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi\"\n", "")

	// Remove pure `span.` call lines (standalone statements).
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Drop `_ = span` and `span.RecordXxx(...)` lines.
		if trimmed == "_ = span" || (strings.HasPrefix(trimmed, "span.") && strings.HasSuffix(trimmed, ")")) {
			continue
		}
		// Drop `_ = span // ...` lines.
		if strings.HasPrefix(trimmed, "_ = span") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// removeSlogRedactionRefs removes lines containing slog or redaction package usage
// in helper files (these packages may not be available).
func removeSlogRedactionRefs(content string) string {
	// Don't remove slog import/usage from helper files that don't need dropping.
	// Only remove in translators. For sync-helpers we keep slog if it's in the original.
	// NOTE: This is intentionally conservative – we only drop redaction references.
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, `"github.com/envoyproxy/ai-gateway/internal/redaction"`) {
			continue
		}
		if strings.Contains(line, "redaction.") {
			indent := leadingWhitespace(line)
			result = append(result, indent+"// CODEMOD-TODO: redaction removed")
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// translatorHelperSet returns the set of helper file names that are synced
// by sync-helpers and should NOT be processed by sync-translators.
func translatorHelperSet() map[string]bool {
	s := make(map[string]bool, len(helperFiles))
	for _, f := range helperFiles {
		s[f] = true
	}
	return s
}

// syncTranslators runs the transform pipeline on every openai_*.go (non-helper,
// non-test) file found in ai-gateway/internal/translator/ and writes the result
// to outDir. Already-generated files are overwritten, so re-running is idempotent.
func syncTranslators(upstreamRoot, outDir string) error {
	srcDir := filepath.Join(upstreamRoot, "internal", "translator")
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", srcDir, err)
	}

	helpers := translatorHelperSet()
	var errs []string

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, "openai_") {
			continue
		}
		if helpers[name] {
			continue
		}

		srcFile := filepath.Join(srcDir, name)
		dstFile := filepath.Join(outDir, name)

		if err := runTransform(srcFile, dstFile); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			fmt.Fprintf(os.Stderr, "transform error: %s: %v\n", name, err)
			continue
		}
		fmt.Printf("transformed: %s\n", dstFile)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%d transform error(s): %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

func leadingWhitespace(s string) string {
	trimmed := strings.TrimLeft(s, " \t")
	return s[:len(s)-len(trimmed)]
}
