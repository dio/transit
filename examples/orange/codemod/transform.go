// Package main — transform.go
// Text-level transformation of openai_*.go files from ai-gateway into
// orange-compatible translator files.
//
// The approach: parse with go/ast only to extract structural metadata
// (struct name, constructor name, method boundaries). Then apply
// transformations at the text level, which avoids comment-position
// and printer-formatting pitfalls.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// runTransform is the entry point for Mode 1: transform-translators.
func runTransform(srcPath, outPath string) error {
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcPath, err)
	}

	// Parse to get structural info.
	_, fset, shape, err := analyzeFile(srcPath)
	if err != nil {
		return err
	}

	// Apply text-level transformation.
	out, err := transformText(src, fset, shape, srcPath)
	if err != nil {
		return err
	}

	// Write output.
	if err := writeToFile(outPath, out); err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}

	// Run goimports.
	if err := runGoimports(outPath); err != nil {
		fmt.Printf("warning: goimports: %v\n", err)
	}
	return nil
}

// transformText applies all rewrites to the source text.
func transformText(src []byte, fset *token.FileSet, shape *TranslatorShape, srcPath string) ([]byte, error) {
	text := string(src)

	// 0. Fix package declaration.
	text = fixPackage(text)

	// 1. Rewrite import block.
	text = rewriteImportBlock(text)

	// 2. Rewrite struct: drop fields of dropped types, replace internalapi types.
	text = rewriteStructText(text, shape, fset)

	// 3. Rewrite constructor.
	text = rewriteConstructorText(text, shape, fset, srcPath)

	// 4. Rewrite RequestBody signature.
	text = rewriteRequestBodyText(text, shape, fset)

	// 5. Rewrite ResponseHeaders return type.
	text = rewriteResponseHeadersText(text, shape)

	// 6. Rewrite ResponseBody signature.
	text = rewriteResponseBodyText(text, shape, fset)

	// 7. Rename ResponseError → responseError.
	text = rewriteResponseErrorText(text, shape)

	// 8. Rewrite or inject RequestHeaders.
	text = rewriteRequestHeadersText(text, shape, fset)

	// 9. Remove methods that are purely for dropped functionality.
	text = removeDroppedMethods(text)

	// 10. Content rewrites (type refs, dropped calls, etc.)
	text = rewriteContent(text)

	// 11. Add init() and generated header.
	text = addInitAndHeader(text, shape, srcPath)

	return []byte(text), nil
}

// --- Step 0: Package ---

func fixPackage(text string) string {
	re := regexp.MustCompile(`(?m)^package \S+`)
	return re.ReplaceAllString(text, "package translator")
}

// --- Step 1: Imports ---

// Import path rewrites.
var importPathRewrites = []struct{ from, to string }{
	{`github.com/envoyproxy/ai-gateway/internal/apischema/openai`, `github.com/dio/transit/examples/orange/internal/apischema/openai`},
	{`github.com/envoyproxy/ai-gateway/internal/apischema/anthropic`, `github.com/dio/transit/examples/orange/internal/apischema/anthropic`},
	{`github.com/envoyproxy/ai-gateway/internal/apischema/awsbedrock`, `github.com/dio/transit/examples/orange/internal/apischema/awsbedrock`},
	{`github.com/envoyproxy/ai-gateway/internal/json`, `encoding/json`},
}

var importPathsToDrop = []string{
	`github.com/envoyproxy/ai-gateway/internal/internalapi`,
	`github.com/envoyproxy/ai-gateway/internal/metrics`,
	`github.com/envoyproxy/ai-gateway/internal/tracing/tracingapi`,
	`github.com/envoyproxy/ai-gateway/internal/redaction`,
	`log/slog`,
}

func rewriteImportBlock(text string) string {
	// Find the import block: "import (\n...\n)"
	// We need to find the matching ) for the import (.
	importKw := "import ("
	startIdx := strings.Index(text, importKw)
	if startIdx == -1 {
		return text
	}

	// Find the matching closing paren.
	openParen := startIdx + len(importKw) - 1
	depth := 0
	closeIdx := -1
	for i := openParen; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeIdx = i
				goto found
			}
		}
	}
found:
	if closeIdx == -1 {
		return text
	}

	importBlock := text[openParen+1 : closeIdx]
	lines := strings.Split(importBlock, "\n")

	var kept []string
	alreadyHas := map[string]bool{}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// Preserve blank separator lines but only if we've added something.
			if len(kept) > 0 {
				kept = append(kept, "")
			}
			continue
		}
		// Extract the import path (possibly aliased).
		imp := extractImportPath(trimmed)
		if imp == "" {
			kept = append(kept, line)
			continue
		}

		// Check if it should be dropped.
		drop := false
		for _, d := range importPathsToDrop {
			if imp == d {
				drop = true
				break
			}
		}
		if drop {
			continue
		}

		// Check if it should be rewritten.
		for _, r := range importPathRewrites {
			if imp == r.from {
				line = strings.ReplaceAll(line, `"`+imp+`"`, `"`+r.to+`"`)
				imp = r.to
				break
			}
		}

		if alreadyHas[imp] {
			continue
		}
		alreadyHas[imp] = true
		kept = append(kept, line)
	}

	// Ensure bytes and encoding/json are present.
	for _, needed := range []string{"bytes", "encoding/json"} {
		if !alreadyHas[needed] {
			kept = append(kept, "\t\""+needed+"\"")
			alreadyHas[needed] = true
		}
	}

	// Remove trailing blank lines.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	newBlock := "import (\n" + strings.Join(kept, "\n") + "\n)"
	return text[:startIdx] + newBlock + text[closeIdx+1:]
}

func extractImportPath(s string) string {
	// s is like: `"encoding/json"`, or `alias "pkg/path"`, or `_ "pkg/path"`.
	i := strings.Index(s, `"`)
	if i == -1 {
		return ""
	}
	j := strings.LastIndex(s, `"`)
	if j <= i {
		return ""
	}
	return s[i+1 : j]
}

// --- Step 2: Struct rewriting ---

func rewriteStructText(text string, shape *TranslatorShape, fset *token.FileSet) string {
	if shape.StructName == "" {
		return text
	}

	// Find the struct declaration.
	structRe := regexp.MustCompile(`(?s)type ` + regexp.QuoteMeta(shape.StructName) + ` struct \{[^}]*?\}`)
	loc := structRe.FindStringIndex(text)
	if loc == nil {
		return text
	}

	structDecl := text[loc[0]:loc[1]]
	lines := strings.Split(structDecl, "\n")
	var newLines []string

	for _, line := range lines {
		// Drop fields of dropped types.
		if shouldDropStructField(line) {
			continue
		}
		// Replace internalapi type aliases.
		line = replaceInternalapiTypesInLine(line)
		newLines = append(newLines, line)
	}

	newStructDecl := strings.Join(newLines, "\n")
	return text[:loc[0]] + newStructDecl + text[loc[1]:]
}

var droppedFieldTypes = []string{
	"metrics.",
	"tracingapi.",
	"*slog.Logger",
}

func shouldDropStructField(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "//") {
		return false
	}
	for _, t := range droppedFieldTypes {
		if strings.Contains(line, t) {
			return true
		}
	}
	return false
}

func replaceInternalapiTypesInLine(line string) string {
	line = strings.ReplaceAll(line, "internalapi.ModelNameOverride", "string")
	line = strings.ReplaceAll(line, "internalapi.RequestModel", "string")
	line = strings.ReplaceAll(line, "internalapi.ResponseModel", "string")
	return line
}

// --- Step 3: Constructor rewriting ---

func rewriteConstructorText(text string, shape *TranslatorShape, fset *token.FileSet, srcPath string) string {
	if shape.ConstructorName == "" {
		return text
	}

	// Find the constructor function.
	fnStart, fnEnd := findFuncBounds(text, shape.ConstructorName, "")
	if fnStart == -1 {
		return text
	}

	fnText := text[fnStart:fnEnd]

	// Capture original signature for CODEMOD-TODO.
	origSig := extractFuncSignature(fnText)

	// Get old parameter mappings from AST.
	paramMap := buildParamMapFromAST(shape)

	// Rewrite the signature.
	// Replace from "func <Name>(" to ")" of params.
	sigRe := regexp.MustCompile(`func ` + regexp.QuoteMeta(shape.ConstructorName) + `\([^)]*\)[^{]*`)
	sigMatch := sigRe.FindString(fnText)
	if sigMatch == "" {
		return text
	}

	newSig := "func " + shape.ConstructorName + "(cfg ProviderConfig) Translator"
	fnText = strings.Replace(fnText, sigMatch, newSig, 1)

	// Rewrite parameter references in body.
	for oldName, cfgExpr := range paramMap {
		fnText = replaceIdentInFunc(fnText, oldName, cfgExpr)
	}

	// Add CODEMOD-TODO comment before the function.
	todo := "// CODEMOD-TODO: original params were: " + origSig + "\n"

	return text[:fnStart] + todo + fnText + text[fnEnd:]
}

func buildParamMapFromAST(shape *TranslatorShape) map[string]string {
	m := make(map[string]string)
	// We need to find the constructor in the file's AST and extract params.
	// We'll do this from the shape's fields and known param name conventions.
	// These mappings cover the common cases across openai_*.go files.
	m["modelNameOverride"] = "cfg.BackendModel"
	m["prefix"] = "cfg.PathPrefix"
	m["apiVersion"] = `cfg.Extra["azure_api_version"]`
	m["gcpProjectID"] = `cfg.Extra["gcp_project_id"]`
	m["gcpLocation"] = `cfg.Extra["gcp_location"]`
	m["anthropicVersion"] = `cfg.Extra["anthropic_version"]`
	return m
}

// replaceIdentInFunc replaces standalone occurrences of `ident` with `replacement`
// in function text (avoiding partial word matches, and NOT replacing composite
// literal keys which appear as `ident:` at the start of a kv pair).
func replaceIdentInFunc(fnText, ident, replacement string) string {
	// We replace `ident` but not when it's a composite literal key (followed by ':').
	// Use a regex that matches `ident` not followed by ':' (and not preceded by '.').
	re := regexp.MustCompile(`(?:^|([^.\w]))` + regexp.QuoteMeta(ident) + `(?:[^:\w]|$)`)
	return re.ReplaceAllStringFunc(fnText, func(match string) string {
		// Find where ident is within the match and replace only that part.
		idx := strings.Index(match, ident)
		if idx == -1 {
			return match
		}
		return match[:idx] + replacement + match[idx+len(ident):]
	})
}

func extractFuncSignature(fnText string) string {
	// Extract the part between "func Name(" and ")".
	start := strings.Index(fnText, "(")
	if start == -1 {
		return ""
	}
	// Find matching close paren.
	depth := 0
	for i := start; i < len(fnText); i++ {
		switch fnText[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return fnText[start+1 : i]
			}
		}
	}
	return ""
}

// --- Step 4: RequestBody rewriting ---

func rewriteRequestBodyText(text string, shape *TranslatorShape, fset *token.FileSet) string {
	if shape.RequestBodyFn == nil {
		return text
	}

	// Get the receiver name for this method.
	recvName := getReceiverName(shape.RequestBodyFn)

	// Find RequestBody func bounds.
	fnStart, fnEnd := findFuncBounds(text, "RequestBody", shape.StructName)
	if fnStart == -1 {
		return text
	}
	fnText := text[fnStart:fnEnd]

	// Find the body param name (second param of old signature).
	bodyParamName := extractBodyParamName(shape.RequestBodyFn)

	// Rewrite signature line.
	// Old: func (o *Struct) RequestBody(raw []byte, body *openai.ChatCompletionRequest, forceBodyMutation bool) (...)
	// New: func (o *Struct) RequestBody(raw []byte) ([]Header, []byte, error)
	sigRe := regexp.MustCompile(`func \(` + recvName + ` \*` + shape.StructName + `\) RequestBody\([^)]*\)[^{]*`)
	fnText = sigRe.ReplaceAllString(fnText, fmt.Sprintf(
		"func (%s *%s) RequestBody(raw []byte) (newHeaders []Header, newBody []byte, err error)",
		recvName, shape.StructName,
	))

	// Fix return type in any explicit return type annotation that survived.
	fnText = strings.ReplaceAll(fnText, "[]internalapi.Header", "[]Header")

	// Remove forceBodyMutation / flag / onRetry usages.
	fnText = removeForceBodyMutationLines(fnText)

	// Replace `original` with `raw` in sjson calls (some files use `original` as old param name).
	fnText = strings.ReplaceAll(fnText, "sjson.SetBytesOptions(original,", "sjson.SetBytesOptions(raw,")

	// Inject body-parsing statements at the start of the function body.
	bodyParamType := shape.RequestBodyParamType
	if bodyParamType == "" {
		bodyParamType = "openai.ChatCompletionRequest"
	}
	fnText = injectBodyParse(fnText, bodyParamName, bodyParamType)

	return text[:fnStart] + fnText + text[fnEnd:]
}

func getReceiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return "o"
	}
	if len(fd.Recv.List[0].Names) == 0 {
		return "_"
	}
	return fd.Recv.List[0].Names[0].Name
}

func extractBodyParamName(fd *ast.FuncDecl) string {
	if fd.Type.Params == nil {
		return "body"
	}
	// The OpenAI ChatCompletionRequest param is the second one.
	for _, p := range fd.Type.Params.List {
		ts := typeString(p.Type)
		if strings.Contains(ts, "ChatCompletionRequest") && len(p.Names) > 0 {
			return p.Names[0].Name
		}
	}
	// Fallback: second param.
	if len(fd.Type.Params.List) >= 2 && len(fd.Type.Params.List[1].Names) > 0 {
		return fd.Type.Params.List[1].Names[0].Name
	}
	return "body"
}

func removeForceBodyMutationLines(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		// Remove lines that reference forceBodyMutation/onRetry/flag.
		if strings.Contains(trimmed, "forceBodyMutation") || strings.Contains(trimmed, "onRetry") {
			// If it's an if statement, skip the whole block.
			if strings.HasPrefix(trimmed, "if ") && strings.HasSuffix(trimmed, "{") {
				indent := leadingWhitespace(line)
				result = append(result, indent+"// CODEMOD-TODO: forceBodyMutation/flag handling removed; review if needed")
				depth := 1
				i++
				for i < len(lines) && depth > 0 {
					for _, ch := range lines[i] {
						if ch == '{' {
							depth++
						} else if ch == '}' {
							depth--
						}
					}
					i++
				}
				continue
			}
			i++
			continue
		}
		result = append(result, line)
		i++
	}
	return strings.Join(result, "\n")
}

func injectBodyParse(fnText, bodyParamName, bodyParamType string) string {
	// Find the opening brace of the function body.
	braceIdx := strings.Index(fnText, "{\n")
	if braceIdx == -1 {
		return fnText
	}
	insertion := "\n\tvar " + bodyParamName + " " + bodyParamType + "\n\t_ = json.Unmarshal(raw, &" + bodyParamName + ")\n"
	return fnText[:braceIdx+2] + insertion + fnText[braceIdx+2:]
}

// --- Step 5: ResponseHeaders ---

func rewriteResponseHeadersText(text string, shape *TranslatorShape) string {
	if shape.ResponseHeadersFn == nil {
		return text
	}

	recvName := getReceiverName(shape.ResponseHeadersFn)

	// Replace the whole signature line for ResponseHeaders.
	// Matches: func (o *Struct) ResponseHeaders(...) <anything> {
	sigRe := regexp.MustCompile(
		`func \(` + regexp.QuoteMeta(recvName) + ` \*` + regexp.QuoteMeta(shape.StructName) + `\) ResponseHeaders\([^)]*\)[^{]*\{`,
	)
	text = sigRe.ReplaceAllStringFunc(text, func(sig string) string {
		// Find the params.
		start := strings.Index(sig, "ResponseHeaders(")
		if start == -1 {
			return sig
		}
		start += len("ResponseHeaders(")
		// Find end of params.
		depth := 1
		i := start
		for i < len(sig) && depth > 0 {
			switch sig[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			i++
		}
		params := sig[start : i-1]
		return fmt.Sprintf("func (%s *%s) ResponseHeaders(%s) (newHeaders []Header, err error) {",
			recvName, shape.StructName, params)
	})

	text = strings.ReplaceAll(text, "[]internalapi.Header", "[]Header")
	return text
}

// --- Step 6: ResponseBody rewriting ---

func rewriteResponseBodyText(text string, shape *TranslatorShape, fset *token.FileSet) string {
	if shape.ResponseBodyFn == nil {
		return text
	}

	recvName := getReceiverName(shape.ResponseBodyFn)

	// Find ResponseBody func bounds.
	fnStart, fnEnd := findFuncBounds(text, "ResponseBody", shape.StructName)
	if fnStart == -1 {
		return text
	}
	fnText := text[fnStart:fnEnd]

	// Find old body io.Reader param name.
	bodyParamName := extractIOReaderParamName(shape.ResponseBodyFn)

	// Rewrite signature.
	sigRe := regexp.MustCompile(
		`func \(` + recvName + ` \*` + shape.StructName + `\) ResponseBody\([^{]*\)[^{]*`,
	)
	fnText = sigRe.ReplaceAllString(fnText, fmt.Sprintf(
		"func (%s *%s) ResponseBody(chunk []byte, endOfStream bool) (newHeaders []Header, newBody []byte, err error)",
		recvName, shape.StructName,
	))

	// Replace body io.Reader parameter usages.
	if bodyParamName != "" && bodyParamName != "chunk" {
		fnText = replaceBodyIOReader(fnText, bodyParamName)
	}

	// Remove span.* references.
	fnText = removeSpanLines(fnText)

	// Remove tokenUsage.Set* calls.
	fnText = removeTokenUsageLines(fnText)

	// Remove responseModel = ... lines (not part of return).
	fnText = removeModelAssignLines(fnText)

	// Remove extractUsageFromBufferEvent call.
	fnText = removeExtractUsageCall(fnText)

	// Remove streaming response model tracking.
	fnText = removeStreamingModelTracking(fnText)

	// Fix return statements: 5-value → 3-value.
	fnText = fixReturnStatements(fnText)

	// Remove the if o.debugLogEnabled block.
	fnText = removeDebugLogBlock(fnText)

	return text[:fnStart] + fnText + text[fnEnd:]
}

func extractIOReaderParamName(fd *ast.FuncDecl) string {
	if fd.Type.Params == nil {
		return ""
	}
	for _, p := range fd.Type.Params.List {
		if typeString(p.Type) == "io.Reader" && len(p.Names) > 0 {
			return p.Names[0].Name
		}
	}
	return ""
}

func replaceBodyIOReader(fnText, oldName string) string {
	// Replace occurrences of the body variable name used as a function argument
	// (i.e., when it appears inside parentheses as a call argument).
	// Avoid replacing inside string literals.
	// Pattern: matches the identifier when preceded by '(' or ',' (with optional spaces).
	re := regexp.MustCompile(`([,(]\s*)` + regexp.QuoteMeta(oldName) + `(\s*[,)])`)
	result := re.ReplaceAllString(fnText, "${1}bytes.NewReader(chunk)${2}")
	// Also handle case where it's the sole argument: func(body)
	re2 := regexp.MustCompile(`\(` + regexp.QuoteMeta(oldName) + `\)`)
	result = re2.ReplaceAllString(result, "(bytes.NewReader(chunk))")
	return result
}

func removeSpanLines(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		// Remove span.* call lines.
		if strings.Contains(line, "span.") {
			i++
			continue
		}
		// Remove "if span != nil { ... }" blocks.
		if strings.HasPrefix(trimmed, "if span") && strings.Contains(trimmed, "nil") {
			depth := 0
			for _, ch := range line {
				if ch == '{' {
					depth++
				} else if ch == '}' {
					depth--
				}
			}
			i++
			if depth > 0 {
				// Multi-line block: skip until depth reaches 0.
				for i < len(lines) && depth > 0 {
					for _, ch := range lines[i] {
						if ch == '{' {
							depth++
						} else if ch == '}' {
							depth--
						}
					}
					i++
				}
			}
			continue
		}
		// Remove span from function call argument lists.
		// e.g., o.extractUsageFromBufferEvent(span) → already handled elsewhere
		result = append(result, line)
		i++
	}
	return strings.Join(result, "\n")
}

func removeTokenUsageLines(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	i := 0
	addedTODO := false // track if we already added a CODEMOD-TODO in this sequence
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Detect tokenUsage.Set* assignments.
		if strings.Contains(line, "tokenUsage.Set") || strings.Contains(line, "tokenUsage =") {
			if !strings.Contains(trimmed, "return") {
				if !addedTODO {
					indent := leadingWhitespace(line)
					result = append(result, indent+"// CODEMOD-TODO: wire token usage via Envoy stats")
					addedTODO = true
				}
				i++
				continue
			}
		}

		// Remove PromptTokensDetails / CompletionTokensDetails / ReasoningTokens blocks.
		if strings.Contains(line, "PromptTokensDetails") ||
			strings.Contains(line, "CompletionTokensDetails") ||
			strings.Contains(line, "ReasoningTokens") {
			if strings.HasPrefix(trimmed, "if ") {
				// Skip the whole block.
				depth := 0
				for _, ch := range line {
					if ch == '{' {
						depth++
					} else if ch == '}' {
						depth--
					}
				}
				i++
				if depth > 0 {
					for i < len(lines) && depth > 0 {
						for _, ch := range lines[i] {
							if ch == '{' {
								depth++
							} else if ch == '}' {
								depth--
							}
						}
						i++
					}
				}
				continue
			}
			i++
			continue
		}

		// Reset addedTODO when we see a non-tokenUsage statement.
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "//") {
			addedTODO = false
		}

		result = append(result, line)
		i++
	}
	return strings.Join(result, "\n")
}

func removeModelAssignLines(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Remove responseModel = ... and requestModel = ... (not in return statements).
		if (strings.HasPrefix(trimmed, "responseModel =") || strings.HasPrefix(trimmed, "requestModel =")) &&
			!strings.Contains(trimmed, "return") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func removeExtractUsageCall(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "extractUsageFromBufferEvent") {
			indent := leadingWhitespace(line)
			result = append(result, indent+"// CODEMOD-TODO: wire token usage via Envoy stats")
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func removeStreamingModelTracking(fnText string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	for _, line := range lines {
		// Remove streamingResponseModel assignments.
		if strings.Contains(line, "streamingResponseModel") && !strings.Contains(line, "//") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func fixReturnStatements(fnText string) string {
	// Match multi-value return statements with 5 values.
	// Pattern: return expr1, expr2, tokenUsage, responseModel/o.requestModel, err
	// We want: return expr1, expr2, err
	// This is tricky to do with regex reliably; we'll do it line-by-line.
	lines := strings.Split(fnText, "\n")
	var result []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "return ") {
			result = append(result, line)
			continue
		}
		// Count commas to see if it's a multi-value return.
		args := splitReturnArgs(trimmed[len("return "):])
		switch len(args) {
		case 5:
			// (newHeaders, newBody, tokenUsage, responseModel, err) → (newHeaders, newBody, err)
			indent := leadingWhitespace(line)
			result = append(result, indent+"return "+strings.Join([]string{args[0], args[1], args[4]}, ", "))
		case 4:
			// Could be (nil, nil, tokenUsage, requestModel) or similar
			indent := leadingWhitespace(line)
			result = append(result, indent+"return "+strings.Join([]string{args[0], args[1], args[3]}, ", "))
		default:
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}

// splitReturnArgs splits a return expression list by top-level commas.
func splitReturnArgs(s string) []string {
	var args []string
	depth := 0
	start := 0
	for i, ch := range s {
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		args = append(args, strings.TrimSpace(s[start:]))
	}
	return args
}

func removeDebugLogBlock(fnText string) string {
	return removeBlockContaining(fnText, "debugLogEnabled")
}

func removeBlockContaining(fnText, keyword string) string {
	lines := strings.Split(fnText, "\n")
	var result []string
	inBlock := false
	depth := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inBlock && strings.Contains(line, keyword) {
			if strings.HasPrefix(trimmed, "if ") {
				inBlock = true
				depth = 0
				for _, ch := range line {
					if ch == '{' {
						depth++
					} else if ch == '}' {
						depth--
					}
				}
				if depth <= 0 {
					inBlock = false
				}
				continue
			}
			// Single-line removal.
			continue
		}
		if inBlock {
			for _, ch := range line {
				if ch == '{' {
					depth++
				} else if ch == '}' {
					depth--
				}
			}
			if depth <= 0 {
				inBlock = false
			}
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// --- Step 7: ResponseError → responseError ---

func rewriteResponseErrorText(text string, shape *TranslatorShape) string {
	if shape.ResponseErrorFn == nil {
		return text
	}
	recvName := getReceiverName(shape.ResponseErrorFn)

	// Rename the method.
	text = strings.ReplaceAll(text,
		fmt.Sprintf("func (%s *%s) ResponseError(", recvName, shape.StructName),
		fmt.Sprintf("func (%s *%s) responseError(", recvName, shape.StructName),
	)

	// Fix its return type.
	fnStart, fnEnd := findFuncBounds(text, "responseError", shape.StructName)
	if fnStart == -1 {
		return text
	}
	fnText := text[fnStart:fnEnd]
	fnText = strings.ReplaceAll(fnText, "[]internalapi.Header", "[]Header")
	return text[:fnStart] + fnText + text[fnEnd:]
}

// --- Step 8: RequestHeaders ---

func rewriteRequestHeadersText(text string, shape *TranslatorShape, _ *token.FileSet) string {
	if shape.RequestHeadersSetterFn != nil {
		recvName := getReceiverName(shape.RequestHeadersSetterFn)
		// Rename SetRequestHeaders → RequestHeaders, add return type.
		text = strings.ReplaceAll(text,
			fmt.Sprintf("func (%s *%s) SetRequestHeaders(", recvName, shape.StructName),
			fmt.Sprintf("// CODEMOD-TODO: extracted from SetRequestHeaders in source\nfunc (%s *%s) RequestHeaders(", recvName, shape.StructName),
		)
		// Fix return type: () → ([]Header, error)
		fnStart, fnEnd := findFuncBounds(text, "RequestHeaders", shape.StructName)
		if fnStart != -1 {
			fnText := text[fnStart:fnEnd]
			// Add return type after the closing paren of params.
			fnText = fixRequestHeadersReturnType(fnText, recvName, shape.StructName)
			// Add return nil, nil at end of body.
			fnText = addReturnNilNil(fnText)
			text = text[:fnStart] + fnText + text[fnEnd:]
		}
	} else {
		// Inject no-op RequestHeaders.
		text = injectNoopRequestHeadersText(text, shape)
	}
	return text
}

func fixRequestHeadersReturnType(fnText, recvName, structName string) string {
	// Pattern: func (recv *Struct) RequestHeaders(headers map[string]string) {
	// Replace with: func (recv *Struct) RequestHeaders(headers map[string]string) ([]Header, error) {
	re := regexp.MustCompile(
		`func \(` + regexp.QuoteMeta(recvName) + ` \*` + regexp.QuoteMeta(structName) + `\) RequestHeaders\([^)]*\)\s*\{`,
	)
	return re.ReplaceAllStringFunc(fnText, func(match string) string {
		// Insert return type before the final {.
		braceIdx := strings.LastIndex(match, "{")
		return match[:braceIdx] + "([]Header, error) {" + match[braceIdx+1:]
	})
}

func addReturnNilNil(fnText string) string {
	// Find the closing brace of the function.
	lastBrace := strings.LastIndex(fnText, "\n}")
	if lastBrace == -1 {
		return fnText
	}
	return fnText[:lastBrace] + "\n\treturn nil, nil\n}" + fnText[lastBrace+2:]
}

func injectNoopRequestHeadersText(text string, shape *TranslatorShape) string {
	s := shape
	noopFn := fmt.Sprintf(`
// RequestHeaders implements Translator.RequestHeaders.
func (o *%s) RequestHeaders(_ map[string]string) ([]Header, error) {
	return nil, nil
}
`, s.StructName)

	// Insert before the last closing brace or after the last method.
	// We'll append it before the final init() if present, or at the end.
	insertPoint := strings.LastIndex(text, "\nfunc init()")
	if insertPoint == -1 {
		return text + noopFn
	}
	return text[:insertPoint] + noopFn + text[insertPoint:]
}

// --- Step 9: Remove dropped-functionality methods ---

var droppedMethodNames = []string{
	"SetRedactionConfig",
	"RedactBody",
	"RedactAnthropicBody",
	"redactResponseMessage",
	"redactReasoningContent",
	"extractUsageFromBufferEvent",
	"SetRequestHeaders", // already handled by renaming to RequestHeaders
}

func removeDroppedMethods(text string) string {
	// These methods should have already been dropped by step 8 (RequestHeaders rename),
	// but we need to also drop extractUsageFromBufferEvent and Redact* methods.
	for _, method := range droppedMethodNames {
		text = removeFuncByName(text, method)
	}
	return text
}

func removeFuncByName(text, funcName string) string {
	// Find the function declaration.
	// Pattern: // comment\nfunc ... funcName(...)
	lines := strings.Split(text, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		line := lines[i]
		// Check if this is the start of the target function.
		if isFuncDecl(line, funcName) {
			// Skip doc comments before it.
			j := len(result) - 1
			for j >= 0 && isCommentLine(result[j]) {
				j--
			}
			result = result[:j+1]
			// Skip the function body.
			depth := 0
			started := false
			for i < len(lines) {
				for _, ch := range lines[i] {
					if ch == '{' {
						depth++
						started = true
					} else if ch == '}' {
						depth--
					}
				}
				i++
				if started && depth <= 0 {
					break
				}
			}
			// Skip blank line after the function.
			if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			continue
		}
		result = append(result, line)
		i++
	}
	return strings.Join(result, "\n")
}

func isFuncDecl(line, funcName string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "func ") {
		return false
	}
	// Match: func (recv *Type) funcName( or func funcName(
	return strings.Contains(trimmed, ") "+funcName+"(") ||
		strings.Contains(trimmed, "func "+funcName+"(")
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "//")
}

// --- Step 10: Global content rewrites ---

func rewriteContent(text string) string {
	// Rename modelNameOverride → backendModel to match ProviderConfig.BackendModel.
	text = strings.ReplaceAll(text, "modelNameOverride", "backendModel")

	// Replace internalapi.Header{...} → Header{...}.
	text = strings.ReplaceAll(text, "internalapi.Header{", "Header{")
	// Replace []internalapi.Header → []Header.
	text = strings.ReplaceAll(text, "[]internalapi.Header", "[]Header")
	// Replace internalapi.ModelNameOverride → string.
	text = strings.ReplaceAll(text, "internalapi.ModelNameOverride", "string")
	// Replace internalapi.ResponseModel → string.
	text = strings.ReplaceAll(text, "internalapi.ResponseModel", "string")
	// Replace internalapi.RequestModel → string.
	text = strings.ReplaceAll(text, "internalapi.RequestModel", "string")
	// Remove remaining internalapi. references.
	text = replaceRemainingInternalapiRefs(text)

	// Remove remaining metrics.* references.
	text = removeMetricsLines(text)
	// Remove remaining tracingapi.* references.
	text = removeTracingLines(text)
	// Remove remaining redaction.* references.
	text = removeRedactionLines(text)
	// Remove remaining slog.* references.
	text = removeSlogLines(text)

	return text
}

func replaceRemainingInternalapiRefs(text string) string {
	// For any remaining internalapi.X usages that we couldn't handle structurally.
	re := regexp.MustCompile(`internalapi\.\w+`)
	return re.ReplaceAllStringFunc(text, func(match string) string {
		switch match {
		case "internalapi.Header":
			return "Header"
		case "internalapi.ModelNameOverride", "internalapi.RequestModel", "internalapi.ResponseModel":
			return "string"
		default:
			return "/* CODEMOD-TODO: unknown internalapi ref: " + match + " */"
		}
	})
}

func removeMetricsLines(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "metrics.") {
			indent := leadingWhitespace(line)
			result = append(result, indent+"// CODEMOD-TODO: wire token usage via Envoy stats")
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func removeTracingLines(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "tracingapi.") || strings.Contains(line, "span.Record") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func removeRedactionLines(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "redaction.") {
			indent := leadingWhitespace(line)
			result = append(result, indent+"// CODEMOD-TODO: redaction removed")
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

func removeSlogLines(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	for _, line := range lines {
		if strings.Contains(line, "slog.") {
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// --- Step 11: Add init() and generated header ---

func addInitAndHeader(text string, shape *TranslatorShape, srcPath string) string {
	providerName := providerNameFromFile(filepath.Base(srcPath))

	// Build init function text.
	initText := ""
	if shape.ConstructorName != "" {
		initText = fmt.Sprintf(`
func init() {
	Register(%s, func(cfg ProviderConfig) Translator {
		return %s(cfg)
	})
}
`, strconv.Quote(providerName), shape.ConstructorName)
	}

	// Add generated header at top (before copyright comment or package).
	header := "// Code generated by examples/orange/codemod; DO NOT EDIT.\n"

	// Append init before the last closing of text.
	text = text + initText

	// Prepend header.
	return header + text
}

// --- Utility: find function bounds ---

// findFuncBounds returns the [start, end) byte offsets of the function
// named `funcName` with receiver type `structName` in text.
// If structName is empty, it matches any function with that name.
func findFuncBounds(text, funcName, structName string) (int, int) {
	lines := strings.Split(text, "\n")
	// Build character offsets per line.
	offsets := make([]int, len(lines)+1)
	for i, line := range lines {
		offsets[i+1] = offsets[i] + len(line) + 1
	}

	for lineIdx, line := range lines {
		if !isFuncLine(line, funcName, structName) {
			continue
		}
		start := offsets[lineIdx]
		// Find the end: scan until matching braces close.
		depth := 0
		started := false
		for i := lineIdx; i < len(lines); i++ {
			for _, ch := range lines[i] {
				if ch == '{' {
					depth++
					started = true
				} else if ch == '}' {
					depth--
				}
			}
			if started && depth <= 0 {
				end := offsets[i+1]
				return start, end
			}
		}
	}
	return -1, -1
}

func isFuncLine(line, funcName, structName string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "func ") {
		return false
	}
	if structName != "" {
		return strings.Contains(trimmed, "*"+structName+")") &&
			(strings.Contains(trimmed, ") "+funcName+"(") ||
				strings.Contains(trimmed, ") "+funcName+" ("))
	}
	return strings.Contains(trimmed, "func "+funcName+"(") ||
		strings.Contains(trimmed, ") "+funcName+"(")
}

// --- Helpers ---

func writeToFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func runGoimports(path string) error {
	cmd := exec.Command("goimports", "-w", path)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, out.String())
	}
	return nil
}

func providerNameFromFile(base string) string {
	base = strings.TrimSuffix(base, ".go")
	base = strings.TrimSuffix(base, "_test")
	// Strip test data suffixes like ".input", ".golden".
	base = strings.TrimSuffix(base, ".input")
	base = strings.TrimSuffix(base, ".golden")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return base
}

// renderToBytes renders an AST file to formatted bytes (unused in text mode, kept for compatibility).
func renderToBytes(f *ast.File, fset *token.FileSet) ([]byte, error) {
	_ = f
	_ = fset
	return nil, fmt.Errorf("renderToBytes not used in text-mode transform")
}

// sortedKeys returns sorted keys from a string map.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
