package config

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
)

// commentReader reads the documentation comments of the configuration
// structure from its source code.
//
// The comments of a configuration key are the documentation a user reads, so
// they live next to the field they describe. The reflection that derives the
// schema cannot see them: a Go program has no access to the comments of its
// own source at run time. The source is therefore parsed, and the comments
// are matched to the fields by their path.
type commentReader struct {
	// comments holds the documentation of every field, by its dot path.
	comments map[string]string
	// structures holds the parsed structure of every named type in the file,
	// by its name.
	structures map[string]*ast.StructType
}

// readComments parses the configuration structure and collects the
// documentation of every field.
//
// The path of a field is the path of the section it belongs to followed by
// the key name of the field, which is the same path the schema uses.
//
// The configuration structure spans several files, so every file of the
// package is parsed: a section may live next to the setting it belongs to.
func readComments(sourcePaths []string, typeName string) (*commentReader, error) {
	reader := &commentReader{
		comments:   map[string]string{},
		structures: map[string]*ast.StructType{},
	}
	for _, sourcePath := range sourcePaths {
		file, err := parser.ParseFile(token.NewFileSet(), sourcePath, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("read the configuration source %q: %w", sourcePath, err)
		}
		for _, declaration := range file.Decls {
			generic, ok := declaration.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				reader.structures[typeSpec.Name.Name] = structure
			}
		}
	}
	root, ok := reader.structures[typeName]
	if !ok {
		return nil, fmt.Errorf("the configuration source has no type %q", typeName)
	}
	reader.collect(root, "")
	return reader, nil
}

// sourceFiles returns the Go files of the configuration package.
//
// The documentation lives next to the setting it describes, so the reader
// needs every file of the package, not one of them.
//
// The files are found next to the source of the reader itself, so the
// command finds them wherever it runs from.
func sourceFiles() ([]string, error) {
	directory, err := packageDirectory()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read the configuration package: %w", err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(directory, name))
	}
	sort.Strings(files)
	return files, nil
}

// packageDirectory returns the directory of the configuration package.
//
// The directory of the source file that names it is the package directory.
// The runtime reports the file of a compiled function, so the package can be
// found without a configuration file or a working directory.
func packageDirectory() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("find the configuration package: the runtime does not report the source file")
	}
	return filepath.Dir(file), nil
}

// collect records the documentation of the fields of one structure.
//
// The path of a nested structure is the path of its section: the field name
// in the parent structure, then the key name of the field inside it.
func (r *commentReader) collect(structure *ast.StructType, prefix string) {
	for _, field := range structure.Fields.List {
		if len(field.Names) == 0 {
			// An embedded field has no name of its own.
			continue
		}
		name := fieldName(field)
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if documentation := fieldComment(field); documentation != "" {
			r.comments[path] = documentation
		}
		r.collectNested(field.Type, path)
	}
}

// collectNested records the documentation of the fields of a nested
// structure.
//
// A nested structure is a named type, so the reader looks it up among the
// structures of the file. A field whose type is not a structure ends the
// walk.
func (r *commentReader) collectNested(expression ast.Expr, prefix string) {
	switch typed := expression.(type) {
	case *ast.StructType:
		r.collect(typed, prefix)
	case *ast.Ident:
		if structure, ok := r.structures[typed.Name]; ok {
			r.collect(structure, prefix)
		}
	case *ast.StarExpr:
		r.collectNested(typed.X, prefix)
	}
}

// fieldName returns the configuration key of a field.
//
// The key comes from the TOML tag, because that is the name the user writes.
// A field the TOML decoder ignores has no configuration key.
func fieldName(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}
	tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))
	name, ok := tag.Lookup("toml")
	if !ok {
		return ""
	}
	if name == "-" {
		return ""
	}
	return name
}

// fieldComment returns the documentation of a field.
//
// The comment of a field is the comment above it. A comment on the same line
// describes the field too, so the two are joined.
func fieldComment(field *ast.Field) string {
	var text []string
	for _, group := range []*ast.CommentGroup{field.Doc, field.Comment} {
		if group == nil {
			continue
		}
		for _, line := range group.List {
			content := strings.TrimPrefix(line.Text, "//")
			content = strings.TrimSpace(content)
			if content == "" {
				continue
			}
			text = append(text, content)
		}
	}
	return strings.Join(text, "\n")
}
