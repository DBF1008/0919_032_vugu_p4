package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// mergeGoFiles combines the Go source files listed in in (names relative to
// dir) into a single file named out.
//
// The merge is performed on ASTs parsed with go/parser rather than by
// line-oriented text scanning, so constructs that the old text splitter could
// not understand - most notably block comments that happen to contain the
// words "package" or "import" - are handled correctly.
//
// Imports are de-duplicated by path. When the same path is imported under more
// than one local name across the input files, a single canonical name is
// chosen for the merged file and the package-qualified selectors in each input
// file are rewritten to that name; local bindings that shadow the qualifier
// are left untouched. Names that would collide with a package-level
// declaration get a numeric suffix.
//
// Input files are always processed in sorted filename order, which also makes
// the order of the resulting init functions deterministic and independent of
// the (possibly platform dependent) order the callers pass them in.
func mergeGoFiles(dir, out string, in ...string) error {
	names := append([]string(nil), in...)
	sort.Strings(names) // deterministic output and deterministic init order

	files := make([]*parsedGoFile, 0, len(names))
	var pkgHeader []byte
	pkgName := ""
	for _, name := range names {
		f, err := parseGoFile(dir, name)
		if err != nil {
			return err
		}
		if pkgName == "" {
			pkgName = f.file.Name.Name
			// capture "package <name>" verbatim from the original source so
			// the leading package clause (and any whitespace/copyright header
			// preceding it) is preserved exactly.
			pkgHeader = f.src[:f.fset.Position(f.file.Name.End()).Offset]
		} else if f.file.Name.Name != pkgName {
			return fmt.Errorf("cannot merge %q: package %q does not match %q", name, f.file.Name.Name, pkgName)
		}
		files = append(files, f)
	}

	// Collect package-level names up front so a chosen import alias can never
	// collide with a top-level declaration.
	usedNames := make(map[string]bool)
	for _, f := range files {
		for _, decl := range f.file.Decls {
			collectTopLevelDeclNames(decl, usedNames)
		}
	}

	// Walk every named import and pick one canonical, file-wide bind per path.
	type importRef struct {
		fileIdx int
		path    string
		bind    string
	}
	var importRefs []importRef
	blankImports := make(map[string]bool) // "_" imports, keyed by path
	dotImports := make(map[string]bool)   // "." imports, keyed by path
	canonicalName := make(map[string]string)

	uniqueName := func(base string) string {
		if !usedNames[base] {
			return base
		}
		for i := 2; ; i++ {
			candidate := fmt.Sprintf("%s%d", base, i)
			if !usedNames[candidate] {
				return candidate
			}
		}
	}

	for fileIdx, f := range files {
		for _, spec := range importSpecs(f.file) {
			importPath := unquoteImportPath(spec)
			switch {
			case spec.Name != nil && spec.Name.Name == "_":
				blankImports[importPath] = true
			case spec.Name != nil && spec.Name.Name == ".":
				dotImports[importPath] = true
			default:
				bind := path.Base(importPath)
				if spec.Name != nil {
					bind = spec.Name.Name
				}
				importRefs = append(importRefs, importRef{fileIdx: fileIdx, path: importPath, bind: bind})
				if _, exists := canonicalName[importPath]; !exists {
					canonical := uniqueName(bind)
					canonicalName[importPath] = canonical
					usedNames[canonical] = true
				}
			}
		}
	}

	// Build a per-file rename map (local bind -> canonical bind) and rewrite
	// package-qualified selectors in that file before emitting it.
	renameMaps := make([]map[string]string, len(files))
	for i := range renameMaps {
		renameMaps[i] = make(map[string]string)
	}
	for _, ref := range importRefs {
		if canonical := canonicalName[ref.path]; ref.bind != canonical {
			renameMaps[ref.fileIdx][ref.bind] = canonical
		}
	}
	for i, f := range files {
		if len(renameMaps[i]) > 0 {
			rewriteImportQualifiers(f.file, renameMaps[i])
		}
	}

	// Collect all imports (named, dot and blank) in deterministic order.
	type mergedImport struct {
		path string
		bind string // "" for a plain import; "." and "_" keep their meaning
	}
	var mergedImports []mergedImport
	for _, importPath := range sortedStringKeys(canonicalName) {
		mergedImports = append(mergedImports, mergedImport{path: importPath, bind: canonicalName[importPath]})
	}
	for _, importPath := range sortedBoolKeys(dotImports) {
		mergedImports = append(mergedImports, mergedImport{path: importPath, bind: "."})
	}
	for _, importPath := range sortedBoolKeys(blankImports) {
		mergedImports = append(mergedImports, mergedImport{path: importPath, bind: "_"})
	}

	// Assemble the merged source: package clause, one import declaration, then
	// the non-import declarations of each file in sorted order.
	var merged bytes.Buffer
	merged.Write(pkgHeader)
	merged.WriteString("\n\n")
	writeImport := func(imp mergedImport) {
		switch {
		case imp.bind == "":
			fmt.Fprintf(&merged, "import %q\n", imp.path)
		case imp.bind == path.Base(imp.path):
			// canonical name matches the package base: no explicit alias
			fmt.Fprintf(&merged, "import %q\n", imp.path)
		default:
			fmt.Fprintf(&merged, "import %s %q\n", imp.bind, imp.path)
		}
	}
	if len(mergedImports) == 1 {
		// Match gofmt/ast.SortImports style: a lone import is not parenthesized.
		writeImport(mergedImports[0])
		merged.WriteString("\n")
	} else if len(mergedImports) > 1 {
		merged.WriteString("import (\n")
		for _, imp := range mergedImports {
			switch {
			case imp.bind == "" || imp.bind == path.Base(imp.path):
				fmt.Fprintf(&merged, "\t%q\n", imp.path)
			default:
				fmt.Fprintf(&merged, "\t%s %q\n", imp.bind, imp.path)
			}
		}
		merged.WriteString(")\n")
	}

	for _, f := range files {
		fmt.Fprintf(&merged, "// ===== %s =====\n", f.name)
		merged.WriteString(renderFileBody(f))
		merged.WriteString("\n\n")
	}

	// Reparse as a final validation and canonicalize the formatting.
	mergedFset := token.NewFileSet()
	if _, err := parser.ParseFile(mergedFset, out, merged.Bytes(), parser.ParseComments); err != nil {
		return fmt.Errorf("merged file %q failed to parse: %w\n--- merged source ---\n%s", out, err, merged.String())
	}
	formatted, err := format.Source(merged.Bytes())
	if err != nil {
		return fmt.Errorf("failed to format merged file %q: %w", out, err)
	}
	if err := os.WriteFile(filepath.Join(dir, out), formatted, 0644); err != nil {
		return fmt.Errorf("failed to write merged file %q: %w", out, err)
	}
	return nil
}

type parsedGoFile struct {
	name string
	fset *token.FileSet
	file *ast.File
	src  []byte
}

func parseGoFile(dir, name string) (*parsedGoFile, error) {
	fpath := filepath.Join(dir, name)
	src, err := os.ReadFile(fpath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %q: %w", name, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, fpath, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %q: %w", name, err)
	}
	return &parsedGoFile{name: name, fset: fset, file: file, src: src}, nil
}

func unquoteImportPath(spec *ast.ImportSpec) string {
	return strings.Trim(spec.Path.Value, `"`)
}

func importSpecs(file *ast.File) []*ast.ImportSpec {
	var specs []*ast.ImportSpec
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.IMPORT {
			continue
		}
		for _, spec := range genDecl.Specs {
			specs = append(specs, spec.(*ast.ImportSpec))
		}
	}
	return specs
}

// collectTopLevelDeclNames records the names a declaration introduces at
// package scope. Method receivers and blank identifiers are ignored.
func collectTopLevelDeclNames(decl ast.Decl, out map[string]bool) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv == nil {
			out[d.Name.Name] = true
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				out[s.Name.Name] = true
			case *ast.ValueSpec:
				for _, name := range s.Names {
					if name.Name != "_" {
						out[name.Name] = true
					}
				}
			}
		}
	}
}

// renderFileBody renders everything in a parsed file following its package and
// import clauses. The file is printed against its own FileSet so comment
// positions stay self-consistent, and the synthetic leading
// "package <name>" line emitted by the printer is stripped.
func renderFileBody(f *parsedGoFile) string {
	cut := f.file.Name.End()
	var bodyDecls []ast.Decl
	for _, decl := range f.file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			if genDecl.End() > cut {
				cut = genDecl.End()
			}
			continue
		}
		bodyDecls = append(bodyDecls, decl)
	}
	var bodyComments []*ast.CommentGroup
	for _, comment := range f.file.Comments {
		if comment.Pos() >= cut {
			bodyComments = append(bodyComments, comment)
		}
	}

	synthetic := &ast.File{
		Name:     f.file.Name,
		Decls:    bodyDecls,
		Comments: bodyComments,
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, f.fset, synthetic); err != nil {
		// The AST came from a successfully parsed file, so a printer failure
		// here indicates an internal bug rather than bad user input.
		panic(fmt.Errorf("failed to render body of %q: %w", f.name, err))
	}
	rendered := buf.String()
	if idx := bytes.IndexByte([]byte(rendered), '\n'); idx >= 0 {
		rendered = rendered[idx+1:]
	}
	return rendered
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// importQualRewriter renames package-qualified selector identifiers
// (e.g. vugu.X) when an import's local name is changed to resolve an alias
// conflict. It tracks package-level names and per-block local bindings so that
// a qualifier shadowed by a local variable, parameter or receiver is left
// alone.
type importQualRewriter struct {
	rename   map[string]string
	pkgScope map[string]bool
	scopes   []map[string]bool
}

// rewriteImportQualifiers rewrites selectors in file according to rename
// (old local bind -> canonical bind).
func rewriteImportQualifiers(file *ast.File, rename map[string]string) {
	rw := &importQualRewriter{
		rename:   rename,
		pkgScope: make(map[string]bool),
	}
	for _, decl := range file.Decls {
		collectTopLevelDeclNames(decl, rw.pkgScope)
	}
	for _, decl := range file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			continue
		}
		rw.walk(decl)
	}
}

func (r *importQualRewriter) pushScope() { r.scopes = append(r.scopes, make(map[string]bool)) }
func (r *importQualRewriter) popScope()  { r.scopes = r.scopes[:len(r.scopes)-1] }

func (r *importQualRewriter) bind(names ...string) {
	if len(r.scopes) == 0 {
		return
	}
	current := r.scopes[len(r.scopes)-1]
	for _, name := range names {
		if name != "" && name != "_" {
			current[name] = true
		}
	}
}

func (r *importQualRewriter) isShadowed(name string) bool {
	if r.pkgScope[name] {
		return true
	}
	for i := len(r.scopes) - 1; i >= 0; i-- {
		if r.scopes[i][name] {
			return true
		}
	}
	return false
}

// walk visits a subtree. Nodes that introduce a scope are handled explicitly
// so bindings are recorded in source order; every other node is traversed with
// ast.Inspect, which does not descend into the manually handled subtrees.
func (r *importQualRewriter) walk(node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if ident, ok := x.X.(*ast.Ident); ok {
				if replacement, ok := r.rename[ident.Name]; ok && !r.isShadowed(ident.Name) {
					ident.Name = replacement
				}
			}
			return true
		case *ast.FuncDecl:
			r.pushScope()
			r.bindFieldList(x.Recv)
			r.bindFieldList(x.Type.Params)
			r.bindFieldList(x.Type.Results)
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.FuncLit:
			r.pushScope()
			r.bindFieldList(x.Type.Params)
			r.bindFieldList(x.Type.Results)
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.BlockStmt:
			r.pushScope()
			r.walkStatements(x.List)
			r.popScope()
			return false
		case *ast.IfStmt:
			r.pushScope()
			r.walkInitStatement(x.Init)
			r.walk(x.Cond)
			r.walk(x.Body)
			r.walk(x.Else)
			r.popScope()
			return false
		case *ast.ForStmt:
			r.pushScope()
			r.walkInitStatement(x.Init)
			r.walk(x.Cond)
			r.walkInitStatement(x.Post)
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.RangeStmt:
			r.pushScope()
			r.walk(x.X)
			if x.Tok == token.DEFINE {
				if ident, ok := x.Key.(*ast.Ident); ok {
					r.bind(ident.Name)
				}
				if ident, ok := x.Value.(*ast.Ident); ok {
					r.bind(ident.Name)
				}
			}
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.SwitchStmt:
			r.pushScope()
			r.walkInitStatement(x.Init)
			r.walk(x.Tag)
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.TypeSwitchStmt:
			r.pushScope()
			r.walkInitStatement(x.Init)
			if assign, ok := x.Assign.(*ast.AssignStmt); ok {
				r.walkAssignment(assign)
			}
			r.walk(x.Body)
			r.popScope()
			return false
		case *ast.CaseClause:
			r.pushScope()
			for _, expr := range x.List {
				r.walk(expr)
			}
			r.walkStatements(x.Body)
			r.popScope()
			return false
		case *ast.CommClause:
			r.pushScope()
			if assign, ok := x.Comm.(*ast.AssignStmt); ok {
				r.walkAssignment(assign)
			} else {
				r.walk(x.Comm)
			}
			r.walkStatements(x.Body)
			r.popScope()
			return false
		}
		return true
	})
}

func (r *importQualRewriter) bindFieldList(fieldList *ast.FieldList) {
	if fieldList == nil {
		return
	}
	for _, field := range fieldList.List {
		for _, ident := range field.Names {
			r.bind(ident.Name)
		}
	}
}

// walkStatements visits statements in source order, recording short variable
// declarations and local type/value declarations as they are introduced so
// that subsequent statements see them.
func (r *importQualRewriter) walkStatements(list []ast.Stmt) {
	for _, statement := range list {
		switch stmt := statement.(type) {
		case *ast.AssignStmt:
			r.walkAssignment(stmt)
		case *ast.DeclStmt:
			genDecl, ok := stmt.Decl.(*ast.GenDecl)
			if !ok {
				r.walk(stmt)
				break
			}
			for _, spec := range genDecl.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					r.walk(s.Type)
					for _, value := range s.Values {
						r.walk(value)
					}
					for _, name := range s.Names {
						r.bind(name.Name)
					}
				case *ast.TypeSpec:
					r.walk(s.Type)
					r.bind(s.Name.Name)
				default:
					r.walk(spec)
				}
			}
		default:
			r.walk(statement)
		}
	}
}

// walkAssignment visits the right-hand side before introducing the defined
// left-hand side names, matching Go scoping for short variable declarations.
func (r *importQualRewriter) walkAssignment(assign *ast.AssignStmt) {
	for _, rhs := range assign.Rhs {
		r.walk(rhs)
	}
	for _, lhs := range assign.Lhs {
		r.walk(lhs)
	}
	if assign.Tok == token.DEFINE {
		for _, lhs := range assign.Lhs {
			if ident, ok := lhs.(*ast.Ident); ok {
				r.bind(ident.Name)
			}
		}
	}
}

func (r *importQualRewriter) walkInitStatement(statement ast.Stmt) {
	if statement == nil {
		return
	}
	if assign, ok := statement.(*ast.AssignStmt); ok {
		r.walkAssignment(assign)
		return
	}
	r.walk(statement)
}
