package gen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mergeGoFiles combines Go source files into one using go/parser (AST-based).
// dir is the directory containing the files, out and in are file names within dir.
//
// The input files are processed in sorted-by-name order so the output is
// deterministic across platforms; in particular, top-level declarations
// (including init functions) appear in the output in that same order, so
// init execution order is predictable.
//
// Imports from all files are merged into a single import block:
//   - exact duplicates (same name and path) are removed;
//   - the same path imported under different explicit names is unified to the
//     first name seen, and references to the dropped names are rewritten;
//   - the same explicit name used for different paths is disambiguated by
//     renaming the later import (name2, name3, ...) and rewriting its references;
//   - unnamed, blank (_) and dot (.) imports are kept as-is (only exact
//     duplicates are removed) since their package names cannot be known
//     without type information.
func mergeGoFiles(dir, out string, in ...string) error {

	if len(in) == 0 {
		return fmt.Errorf("mergeGoFiles: no input files")
	}

	sort.Strings(in) // deterministic output and init execution order

	fset := token.NewFileSet()

	// parse all the files
	files := make([]*ast.File, 0, len(in))
	for _, fname := range in {
		f, err := parser.ParseFile(fset, filepath.Join(dir, fname), nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("error trying to parse Go file %q: %w", fname, err)
		}
		files = append(files, f)
	}

	pkgName := files[0].Name.Name

	// merge state for imports
	type mergedImport struct {
		name string // local name: "", "_", "." or a regular identifier
		path string // quoted import path
	}
	pathToName := make(map[string]string) // import path -> canonical local name ("" if unnamed)
	nameToPath := make(map[string]string) // explicit local name -> import path
	var imports []mergedImport            // canonical imports, in first-seen order

	// bodyDecls[i] holds the non-import declarations of files[i]
	bodyDecls := make([][]ast.Decl, len(files))

	for i, f := range files {

		if f.Name.Name != pkgName {
			return fmt.Errorf("package name mismatch: %q is package %q but %q is package %q",
				in[0], pkgName, in[i], f.Name.Name)
		}

		// compute import renames needed for this file and collect canonical imports
		renames := make(map[string]string)
		for _, imp := range f.Imports {
			path := imp.Path.Value
			name := ""
			if imp.Name != nil {
				name = imp.Name.Name
			}

			canonicalName, seen := pathToName[path]
			if seen {
				switch {
				case canonicalName == name:
					continue // exact duplicate, drop it
				case isPlainImportName(canonicalName) && isPlainImportName(name):
					// same path under two different explicit names: unify to the
					// first one and rewrite references below
					renames[name] = canonicalName
					continue
				default:
					// one of them is unnamed, blank or dot: cannot safely unify
					// without package type information, keep both (legal Go)
					imports = append(imports, mergedImport{name: name, path: path})
					continue
				}
			}

			// first time seeing this path
			if isPlainImportName(name) {
				if otherPath, ok := nameToPath[name]; ok && otherPath != path {
					// same import name used for a different path: rename this one
					newName := uniqueImportName(name, nameToPath)
					renames[name] = newName
					name = newName
				}
				nameToPath[name] = path
			}
			pathToName[path] = name
			imports = append(imports, mergedImport{name: name, path: path})
		}

		// go through the declarations, dropping import decls and applying renames
		for _, decl := range f.Decls {
			if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
				continue
			}
			if len(renames) > 0 {
				applyImportRenames(decl, renames)
			}
			bodyDecls[i] = append(bodyDecls[i], decl)
		}
	}

	// print the merged program: package clause, one import block, then each
	// file's declarations (printed per file so comments keep their positions
	// relative to their own declarations)
	var newPgm bytes.Buffer
	newPgm.WriteString("package " + pkgName + "\n\n")

	if len(imports) > 0 {
		specs := make([]ast.Spec, len(imports))
		for i, imp := range imports {
			spec := &ast.ImportSpec{Path: &ast.BasicLit{Kind: token.STRING, Value: imp.path}}
			if imp.name != "" {
				spec.Name = ast.NewIdent(imp.name)
			}
			specs[i] = spec
		}
		importDecl := &ast.GenDecl{Tok: token.IMPORT, Specs: specs}
		err := printer.Fprint(&newPgm, token.NewFileSet(), importDecl)
		if err != nil {
			return fmt.Errorf("error trying to print merged imports: %w", err)
		}
		newPgm.WriteString("\n\n")
	}

	for i, f := range files {
		// print as a synthetic file (keeps comments positioned correctly
		// relative to their own declarations), then drop the package clause
		chunkFile := &ast.File{
			Package:  f.Package,
			Name:     f.Name,
			Decls:    bodyDecls[i],
			Comments: f.Comments,
		}
		var chunk bytes.Buffer
		err := printer.Fprint(&chunk, fset, chunkFile)
		if err != nil {
			return fmt.Errorf("error trying to print declarations from %q: %w", in[i], err)
		}
		newPgm.WriteString(dropPackageClause(chunk.String(), f.Name.Name))
		newPgm.WriteString("\n")
	}

	// now read it back in so positions are normalized, then clean up the imports
	fset2 := token.NewFileSet()
	f, err := parser.ParseFile(fset2, out, newPgm.String(), parser.ParseComments)
	if err != nil {
		log.Printf("DEBUG: full merged file contents:\n%s", newPgm.String())
		return fmt.Errorf("error trying to parse merged file: %w", err)
	}
	ast.SortImports(fset2, f)

	dedupAstFileImports(f)

	fileout, err := os.Create(filepath.Join(dir, out))
	if err != nil {
		return fmt.Errorf("error trying to open output file: %w", err)
	}
	defer fileout.Close()
	err = printer.Fprint(fileout, fset2, f)
	if err != nil {
		return err
	}
	return nil

}

// isPlainImportName reports whether name is a regular import identifier,
// i.e. not unnamed (""), blank ("_") or dot (".").
func isPlainImportName(name string) bool {
	return name != "" && name != "_" && name != "."
}

// uniqueImportName returns name, name2, name3, ... whichever is not already
// present in nameToPath.
func uniqueImportName(name string, nameToPath map[string]string) string {
	if _, ok := nameToPath[name]; !ok {
		return name
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s%d", name, i)
		if _, ok := nameToPath[candidate]; !ok {
			return candidate
		}
	}
}

// applyImportRenames rewrites package qualifier identifiers in decl according
// to renames (old import name -> new import name).
func applyImportRenames(decl ast.Decl, renames map[string]string) {
	ast.Inspect(decl, func(n ast.Node) bool {
		selExpr, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selExpr.X.(*ast.Ident)
		if !ok {
			return true
		}
		if newName, ok := renames[ident.Name]; ok {
			ident.Name = newName
		}
		return true
	})
}

// dropPackageClause removes the "package <name>" line from printed Go source.
func dropPackageClause(src, pkgName string) string {
	pkgLine := "package " + pkgName
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == pkgLine {
			return strings.Join(append(lines[:i], lines[i+1:]...), "\n")
		}
	}
	return src
}
