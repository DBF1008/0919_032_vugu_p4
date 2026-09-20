package gen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

func TestMerge(t *testing.T) {

	debug := true

	type tcase struct {
		name       string
		infiles    map[string]string   // file structure to start with
		out        map[string][]string // regexps to match in output files
		outNot     map[string][]string // regexps to NOT match in output files
		outImports map[string][]string // exact set of imports expected in output files (rendered as [name] "path")
	}

	tcList := []tcase{
		{
			name: "simple",
			infiles: map[string]string{
				"file1.go": "package main\nfunc main(){}",
				"file2.go": "package main\nvar a string",
			},
			out: map[string][]string{
				"out.go": {`func main`, `var a string`},
			},
		},
		{
			name: "comments",
			infiles: map[string]string{
				"file1.go": "package main\n// main comment here\nfunc main(){}",
				"file2.go": "package main\nvar a string // a comment here\n",
			},
			out: map[string][]string{
				"out.go": {`func main`, `// main comment here`, `var a string`, `// a comment here`},
			},
		},
		{
			name: "multiline-comment",
			infiles: map[string]string{
				// a multi-line comment containing keywords that the old line-based
				// splitter would mistake for block boundaries; the import of "log"
				// in file2 must not be swallowed by the comment and both real
				// imports must survive in the merged output
				"file1.go": "package main\n\n/*\npackage notreal\nimport \"notreal\"\nfunc notreal() {}\ntype notreal2 struct{}\n*/\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hi\") }\n",
				"file2.go": "package main\n\nimport \"log\"\n\nvar _ = log.Println\n",
			},
			out: map[string][]string{
				"out.go": {`func main`, `var _ = log.Println`},
			},
			outImports: map[string][]string{
				"out.go": {`"fmt"`, `"log"`},
			},
		},
		{
			name: "import-dedup",
			infiles: map[string]string{
				"file1.go": "package main\nimport \"fmt\"\n// main comment here\nfunc main(){}",
				"file2.go": "package main\nimport \"fmt\"\nvar a string // a comment here\n",
			},
			out: map[string][]string{
				"out.go": {`import "fmt"`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)import "fmt".*import "fmt"`},
			},
			outImports: map[string][]string{
				"out.go": {`"fmt"`},
			},
		},
		{
			name: "import-dedup-2",
			infiles: map[string]string{
				"file1.go": "package main\nimport \"fmt\"\n// main comment here\nfunc main(){}",
				"file2.go": "package main\nimport \"fmt\"\nimport \"log\"\nvar a string // a comment here\n",
			},
			out: map[string][]string{
				"out.go": {`"fmt"`, `"log"`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)\}.*"log"`},
			},
			outImports: map[string][]string{
				"out.go": {`"fmt"`, `"log"`},
			},
		},
		{
			name: "import-alias-unify",
			infiles: map[string]string{
				// same package imported under two different names; should be
				// unified to the first name and references rewritten
				"file1.go": "package main\nimport vugu \"example.com/vugu\"\nvar A = vugu.New\n",
				"file2.go": "package main\nimport vugu2 \"example.com/vugu\"\nvar B = vugu2.New\n",
			},
			out: map[string][]string{
				"out.go": {`A = vugu.New`, `B = vugu.New`},
			},
			outNot: map[string][]string{
				"out.go": {`vugu2`},
			},
			outImports: map[string][]string{
				"out.go": {`vugu "example.com/vugu"`},
			},
		},
		{
			name: "import-name-conflict",
			infiles: map[string]string{
				// same import name used for two different packages; the later
				// one must be renamed and its references rewritten
				"file1.go": "package main\nimport js \"example.com/one/js\"\nvar A = js.X\n",
				"file2.go": "package main\nimport js \"example.com/two/js\"\nvar B = js.Y\n",
			},
			out: map[string][]string{
				"out.go": {`A = js.X`, `B = js2.Y`},
			},
			outImports: map[string][]string{
				"out.go": {`js "example.com/one/js"`, `js2 "example.com/two/js"`},
			},
		},
		{
			name: "init-order",
			infiles: map[string]string{
				// init functions must appear in sorted file name order so
				// initialization behavior is deterministic across platforms
				"b_init.go": "package main\nfunc init() { order = append(order, \"b\") }\n",
				"a_init.go": "package main\nvar order []string\nfunc init() { order = append(order, \"a\") }\n",
			},
			out: map[string][]string{
				"out.go": {`(?s)"a".*"b"`},
			},
		},
	}

	for _, tc := range tcList {
		t.Run(tc.name, func(t *testing.T) {

			tmpDir, err := os.MkdirTemp("", "TestMerge")
			if err != nil {
				t.Fatal(err)
			}

			if debug {
				t.Logf("Test %q using tmpDir: %s", tc.name, tmpDir)
			} else {
				defer os.RemoveAll(tmpDir)
				t.Parallel()
			}

			tstWriteFiles(tmpDir, tc.infiles)
			var in []string
			for k := range tc.infiles {
				// in = append(in, filepath.Join(tmpDir, k))
				in = append(in, k)
			}

			err = mergeGoFiles(tmpDir, "out.go", in...)
			if err != nil {
				t.Fatal(err)
			}

			for fname, patterns := range tc.out {
				b, err := os.ReadFile(filepath.Join(tmpDir, fname))
				if err != nil {
					t.Errorf("failed to read file %q after Run: %v", fname, err)
					continue
				}
				for _, pattern := range patterns {
					re := regexp.MustCompile(pattern)
					if !re.Match(b) {
						t.Errorf("failed to match regexp on file %q: %s", fname, pattern)
					}
				}
			}

			for fname, patterns := range tc.outNot {
				b, err := os.ReadFile(filepath.Join(tmpDir, fname))
				if err != nil {
					t.Errorf("failed to read file %q after Run: %v", fname, err)
					continue
				}
				for _, pattern := range patterns {
					re := regexp.MustCompile(pattern)
					if re.Match(b) {
						t.Errorf("incorrectly matched regexp on file %q: %s", fname, pattern)
					}
				}
			}

			for fname, wantImports := range tc.outImports {
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, filepath.Join(tmpDir, fname), nil, 0)
				if err != nil {
					t.Errorf("failed to parse output file %q: %v", fname, err)
					continue
				}
				var gotImports []string
				for _, imp := range f.Imports {
					gotImports = append(gotImports, renderImportSpec(imp))
				}
				sort.Strings(gotImports)
				sortedWant := append([]string(nil), wantImports...)
				sort.Strings(sortedWant)
				if !strSliceEqual(gotImports, sortedWant) {
					t.Errorf("file %q imports = %v, want %v", fname, gotImports, sortedWant)
				}
			}

			if debug {
				outb, _ := os.ReadFile(filepath.Join(tmpDir, "out.go"))
				t.Logf("OUTPUT:\n%s", outb)
			}

		})
	}

}

// renderImportSpec renders an import spec as [name] "path" for comparison.
func renderImportSpec(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name + " " + imp.Path.Value
	}
	return imp.Path.Value
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
