package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestMerge(t *testing.T) {

	type tcase struct {
		name    string
		infiles map[string]string   // file structure to start with
		inorder []string            // explicit order mergeGoFiles is called with
		out     map[string][]string // regexps to match in output files
		outNot  map[string][]string // regexps to NOT match in output files
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
			name: "import-dedup",
			infiles: map[string]string{
				"file1.go": "package main\nimport \"fmt\"\n// main comment here\nfunc main(){fmt.Println(1)}",
				"file2.go": "package main\nimport \"fmt\"\nvar a string // a comment here\n",
			},
			out: map[string][]string{
				"out.go": {`import "fmt"`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)import "fmt".*import "fmt"`},
			},
		},
		{
			name: "import-dedup-2",
			infiles: map[string]string{
				"file1.go": "package main\nimport \"fmt\"\n// main comment here\nfunc main(){fmt.Println(1)}",
				"file2.go": "package main\nimport \"fmt\"\nimport \"log\"\nvar a string // a comment here\n",
			},
			out: map[string][]string{
				"out.go": {`(?ms)import \(\s*"fmt"\s*"log"\s*\)`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)import "fmt".*import "fmt"`},
			},
		},
		{
			// the original text splitter mistook the words package/import
			// inside a block comment for block boundaries and produced
			// unparseable output
			name: "block-comment-keywords",
			infiles: map[string]string{
				"file1.go": "package main\n/*\n a block comment mentioning package\n and import across several lines\n*/\nfunc main(){}",
				"file2.go": "package main\n/* another\n import package comment */\nvar a int\n",
			},
			out: map[string][]string{
				"out.go": {`func main`, `var a int`, `(?ms)/\*.*package.*import.*\*/`},
			},
		},
		{
			name: "line-comment-keywords",
			infiles: map[string]string{
				"file1.go": "package main\n// this comment says package and import\nfunc main(){}",
			},
			out: map[string][]string{
				"out.go": {`func main`, `package and import`},
			},
		},
		{
			// same package imported under two different aliases must collapse
			// to a single alias with selectors rewritten in both files
			name: "alias-conflict-same-path",
			infiles: map[string]string{
				"file1.go": "package main\nimport vugu \"fmt\"\nfunc main(){ vugu.Println(1) }",
				"file2.go": "package main\nimport vugu2 \"fmt\"\nfunc f(){ vugu2.Println(2) }",
			},
			out: map[string][]string{
				"out.go": {`vugu\.Println`, `func f`},
			},
			outNot: map[string][]string{
				"out.go": {`vugu2`, `(?ms)vugu "fmt".*vugu "fmt"`},
			},
		},
		{
			// a canonical import alias that equals a package-level declaration
			// must be renamed aside with a numeric suffix
			name: "alias-collides-toplevel-name",
			infiles: map[string]string{
				"file1.go": "package main\nimport vugu \"fmt\"\nvar vugu2 = 9\nfunc main(){ vugu.Println(vugu2) }",
				"file2.go": "package main\nimport vugu2 \"fmt\"\nfunc f(){ vugu2.Println(2) }",
			},
			out: map[string][]string{
				"out.go": {`vugu\.Println`, `var vugu2 = 9`},
			},
			outNot: map[string][]string{
				"out.go": {`vugu2\.Println`, `vugu2 "fmt"`},
			},
		},
		{
			// two different packages imported with the same local name must be
			// given distinct aliases (the second one is suffixed)
			name: "alias-conflict-different-paths",
			infiles: map[string]string{
				"file1.go": "package main\nimport x \"fmt\"\nfunc main(){ x.Println(1) }",
				"file2.go": "package main\nimport x \"errors\"\nfunc f(){ _ = x.New(\"boom\") }",
			},
			out: map[string][]string{
				"out.go": {`x\.Println`, `x2\.New`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)x "fmt".*\n\s*x "fmt"`},
			},
		},
		{
			// a local variable that shadows the import alias must not be
			// rewritten, while real package selectors are still canonicalized
			name: "alias-rewrite-respects-shadow",
			infiles: map[string]string{
				"file1.go": "package main\nimport vugu \"fmt\"\nfunc main(){ vugu.Println(1) }",
				"file2.go": "package main\nimport vugu2 \"fmt\"\nfunc f(){ vugu2.Println(2) }\nfunc g(){ vugu2 := 5; _ = vugu2 }",
			},
			out: map[string][]string{
				"out.go": {`vugu2 := 5`, `vugu\.Println`},
			},
			outNot: map[string][]string{
				"out.go": {`vugu2\.Println`, `vugu := 5`},
			},
		},
		{
			name: "dot-and-blank-imports",
			infiles: map[string]string{
				"file1.go": "package main\nimport . \"strings\"\nimport _ \"errors\"\nfunc main(){ _ = Contains(\"a\", \"a\") }",
				"file2.go": "package main\nimport . \"strings\"\nimport _ \"errors\"\nvar a int\n",
			},
			out: map[string][]string{
				"out.go": {`\. "strings"`, `_ "errors"`},
			},
			outNot: map[string][]string{
				"out.go": {`(?ms)\. "strings".*\. "strings"`, `(?ms)_ "errors".*_ "errors"`},
			},
		},
		{
			// init functions must appear in sorted filename order regardless
			// of the order the files are passed in
			name: "init-order-sorted",
			infiles: map[string]string{
				"z_file.go": "package main\nfunc init(){ println(\"z\") }\n",
				"a_file.go": "package main\nfunc init(){ println(\"a\") }\nfunc main(){}\n",
				"m_file.go": "package main\nfunc init(){ println(\"m\") }\n",
			},
			inorder: []string{"z_file.go", "m_file.go", "a_file.go"},
			out: map[string][]string{
				"out.go": {`(?ms)println\("a"\).*println\("m"\).*println\("z"\)`},
			},
		},
		{
			name: "package-mismatch",
			infiles: map[string]string{
				"file1.go": "package main\nfunc main(){}",
				"file2.go": "package other\nvar a int\n",
			},
		},
	}

	for _, tc := range tcList {
		t.Run(tc.name, func(t *testing.T) {

			tmpDir, err := os.MkdirTemp("", "TestMerge")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)

			tstWriteFiles(tmpDir, tc.infiles)

			var in []string
			if len(tc.inorder) > 0 {
				in = tc.inorder
			} else {
				for k := range tc.infiles {
					in = append(in, k)
				}
			}

			mergeErr := mergeGoFiles(tmpDir, "out.go", in...)

			if tc.name == "package-mismatch" {
				if mergeErr == nil {
					t.Fatalf("expected an error merging files with differing package names, got nil")
				}
				return
			}
			if mergeErr != nil {
				if b, rerr := os.ReadFile(filepath.Join(tmpDir, "out.go")); rerr == nil {
					t.Logf("merged output:\n%s", b)
				}
				t.Fatal(mergeErr)
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
						t.Errorf("failed to match regexp on file %q: %s\n--- file ---\n%s", fname, pattern, b)
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
						t.Errorf("incorrectly matched regexp on file %q: %s\n--- file ---\n%s", fname, pattern, b)
					}
				}
			}
		})
	}

}

// TestMergeOutputCompiles verifies that merged files are not just syntactically
// valid but actually compile and run, exercising import alias conflict
// resolution, shadow handling and deterministic init order.
//
// Each case compiles the produced single file with the go tool and runs it, so
// it is skipped in short mode (it needs a working build toolchain).
func TestMergeOutputCompiles(t *testing.T) {

	if testing.Short() {
		t.Skip("skipping compile-driven merge test in short mode")
	}

	cases := []struct {
		name    string
		order   []string
		files   map[string]string
		wantOut string
	}{
		{
			name:  "same-path-different-alias",
			order: []string{"b.go", "a.go"},
			files: map[string]string{
				"a.go": "package main\nimport vugu \"fmt\"\nfunc main(){ vugu.Println(\"hello\") }",
				"b.go": "package main\nimport vugu2 \"fmt\"\nfunc init(){ vugu2.Println(\"init\") }\n",
			},
			wantOut: "init\nhello\n",
		},
		{
			name:  "alias-collides-toplevel-name",
			order: []string{"a.go", "b.go"},
			files: map[string]string{
				"a.go": "package main\nimport vugu \"fmt\"\nvar vugu2 = 9\nfunc main(){ vugu.Println(vugu2) }",
				"b.go": "package main\nimport vugu2 \"fmt\"\nfunc init(){ vugu2.Println(\"init\") }\n",
			},
			wantOut: "init\n9\n",
		},
		{
			name:  "different-paths-same-localname",
			order: []string{"a.go", "b.go"},
			files: map[string]string{
				"a.go": "package main\nimport q \"fmt\"\nfunc main(){ q.Println(errText()) }",
				"b.go": "package main\nimport q \"errors\"\nfunc errText() string { return q.New(\"boom\").Error() }",
			},
			wantOut: "boom\n",
		},
		{
			name:  "local-shadow-left-alone",
			order: []string{"a.go", "b.go"},
			files: map[string]string{
				"a.go": "package main\nimport vugu \"fmt\"\nfunc main(){ vugu.Println(\"hi\") }",
				"b.go": "package main\nimport vugu2 \"fmt\"\nfunc init(){ vugu2.Println(\"init\") }\nfunc f(){ vugu2 := 5; _ = vugu2 }\n",
			},
			wantOut: "init\nhi\n",
		},
		{
			name:  "init-order-independent-of-input",
			order: []string{"z.go", "m.go", "a.go"},
			files: map[string]string{
				"a.go": "package main\nimport (\"fmt\"; \"strings\")\nvar out []string\nfunc init(){ out = append(out, \"a\") }\nfunc main(){ fmt.Println(strings.Join(out, \" \")) }",
				"m.go": "package main\nfunc init(){ out = append(out, \"m\") }",
				"z.go": "package main\nimport s2 \"strings\"\nvar _ = s2.ToLower\nfunc init(){ out = append(out, \"z\") }",
			},
			wantOut: "a m z\n",
		},
		{
			name:  "block-comment-keywords-compiles",
			order: []string{"a.go", "b.go"},
			files: map[string]string{
				"a.go": "package main\n/*\n block comment with package and import\n across lines\n*/\nfunc main(){ println(\"ok\") }",
				"b.go": "package main\nvar a int\n",
			},
			wantOut: "ok\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tstWriteFiles(dir, tc.files)

			if err := mergeGoFiles(dir, "out.go", tc.order...); err != nil {
				if b, rerr := os.ReadFile(filepath.Join(dir, "out.go")); rerr == nil {
					t.Logf("merged output:\n%s", b)
				}
				t.Fatalf("mergeGoFiles: %v", err)
			}
			for name := range tc.files {
				if err := os.Remove(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}

			run := func(name string, args ...string) (string, error) {
				cmd := exec.Command(name, args...)
				cmd.Dir = dir
				b, err := cmd.CombinedOutput()
				return string(b), err
			}

			if out, err := run("go", "mod", "init", "tc"); err != nil {
				t.Fatalf("go mod init: %v\n%s", err, out)
			}
			if out, err := run("go", "build", "-o", "app", "."); err != nil {
				t.Fatalf("go build of merged file failed:\n%s", out)
			}

			cmd := exec.Command(filepath.Join(dir, "app"))
			cmd.Dir = dir
			b, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("running merged app failed: %v\n%s", err, b)
			}
			if got := string(b); got != tc.wantOut {
				t.Fatalf("merged program output mismatch:\n got: %q\nwant: %q", got, tc.wantOut)
			}
		})
	}
}
