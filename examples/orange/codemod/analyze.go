package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// FieldInfo describes a struct field.
type FieldInfo struct {
	Name string
	Type string
}

// TranslatorShape captures all the pieces of an openai_*.go translator file
// that the transform step needs to rewrite.
type TranslatorShape struct {
	StructName             string
	ConstructorName        string
	Fields                 []FieldInfo
	RequestBodyFn          *ast.FuncDecl
	RequestBodyParamType   string // e.g. "openai.ChatCompletionRequest", detected from AST
	ResponseHeadersFn      *ast.FuncDecl
	ResponseBodyFn         *ast.FuncDecl
	ResponseErrorFn        *ast.FuncDecl
	RequestHeadersSetterFn *ast.FuncDecl // SetRequestHeaders, if present
	Imports                []*ast.ImportSpec
}

// analyzeFile parses a single openai_*.go source file and returns a
// TranslatorShape describing the primary translator struct.
func analyzeFile(srcPath string) (*ast.File, *token.FileSet, *TranslatorShape, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, nil, parser.ParseComments)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", srcPath, err)
	}

	shape := &TranslatorShape{}

	// Collect imports.
	for _, imp := range f.Imports {
		shape.Imports = append(shape.Imports, imp)
	}

	// Find the translator struct. Look for a struct that has RequestBody, ResponseBody
	// etc. methods, i.e., the type that implements OpenAIChatCompletionTranslator.
	// Strategy: find all method receivers, then find the struct type associated.
	structMethods := map[string][]*ast.FuncDecl{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recv := fd.Recv.List[0]
		typeName := receiverTypeName(recv.Type)
		structMethods[typeName] = append(structMethods[typeName], fd)
	}

	// The primary struct is the one with RequestBody (and optionally ResponseBody).
	// Some translators (e.g. openai_azureopenai.go) embed the base struct and only
	// override RequestBody, so we accept structs with just RequestBody.
	var primaryStruct string
	// Prefer the struct with both RequestBody and ResponseBody.
	for typeName, methods := range structMethods {
		hasReqBody := false
		hasRespBody := false
		for _, m := range methods {
			switch m.Name.Name {
			case "RequestBody":
				hasReqBody = true
			case "ResponseBody":
				hasRespBody = true
			}
		}
		if hasReqBody && hasRespBody {
			primaryStruct = typeName
			break
		}
	}
	// Fall back to struct with just RequestBody.
	if primaryStruct == "" {
		for typeName, methods := range structMethods {
			for _, m := range methods {
				if m.Name.Name == "RequestBody" {
					primaryStruct = typeName
					break
				}
			}
			if primaryStruct != "" {
				break
			}
		}
	}
	if primaryStruct == "" {
		return nil, nil, nil, fmt.Errorf("could not find primary translator struct in %s", srcPath)
	}
	shape.StructName = primaryStruct

	// Collect methods for that struct.
	for _, fd := range structMethods[primaryStruct] {
		switch fd.Name.Name {
		case "RequestBody":
			shape.RequestBodyFn = fd
			shape.RequestBodyParamType = extractRequestBodyParamType(fd)
		case "ResponseHeaders":
			shape.ResponseHeadersFn = fd
		case "ResponseBody":
			shape.ResponseBodyFn = fd
		case "ResponseError":
			shape.ResponseErrorFn = fd
		case "SetRequestHeaders":
			shape.RequestHeadersSetterFn = fd
		}
	}

	// Find the constructor: func New*(...) OpenAIChatCompletionTranslator
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv != nil {
			continue
		}
		if !strings.HasPrefix(fd.Name.Name, "New") {
			continue
		}
		// Check return type includes the struct or OpenAIChatCompletionTranslator
		if fd.Type.Results == nil {
			continue
		}
		for _, result := range fd.Type.Results.List {
			resultType := typeString(result.Type)
			if strings.Contains(resultType, "Translator") || resultType == "*"+primaryStruct {
				shape.ConstructorName = fd.Name.Name
				break
			}
		}
		if shape.ConstructorName != "" {
			break
		}
	}

	// Collect struct fields.
	for _, decl := range f.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}
		for _, spec := range genDecl.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != primaryStruct {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				typStr := typeString(field.Type)
				for _, name := range field.Names {
					shape.Fields = append(shape.Fields, FieldInfo{Name: name.Name, Type: typStr})
				}
				// Embedded field (no Names).
				if len(field.Names) == 0 {
					shape.Fields = append(shape.Fields, FieldInfo{Name: "", Type: typStr})
				}
			}
		}
	}

	return f, fset, shape, nil
}

// receiverTypeName extracts the type name from a receiver expression,
// handling both *T and T forms.
func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// extractRequestBodyParamType returns the unqualified type name of the body
// parameter in a RequestBody method (the second parameter, e.g. "openai.ChatCompletionRequest").
// Falls back to "openai.ChatCompletionRequest" if the type cannot be determined.
func extractRequestBodyParamType(fd *ast.FuncDecl) string {
	if fd == nil || fd.Type.Params == nil {
		return "openai.ChatCompletionRequest"
	}
	for _, p := range fd.Type.Params.List {
		ts := typeString(p.Type)
		ts = strings.TrimPrefix(ts, "*")
		// Pick any openai.* type that looks like a request struct.
		if strings.HasPrefix(ts, "openai.") && strings.Contains(ts, "Request") {
			return ts
		}
	}
	// Fallback: second param type.
	if len(fd.Type.Params.List) >= 2 {
		ts := typeString(fd.Type.Params.List[1].Type)
		return strings.TrimPrefix(ts, "*")
	}
	return "openai.ChatCompletionRequest"
}

// typeString renders an ast.Expr as a string (for display/matching only).
func typeString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return "*" + typeString(e.X)
	case *ast.SelectorExpr:
		return typeString(e.X) + "." + e.Sel.Name
	case *ast.ArrayType:
		return "[]" + typeString(e.Elt)
	case *ast.MapType:
		return "map[" + typeString(e.Key) + "]" + typeString(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	}
	return fmt.Sprintf("%T", expr)
}
