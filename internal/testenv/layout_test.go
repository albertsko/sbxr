package testenv_test

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// targets maps the file suffix of each target to its build tag and test name prefix.
var targets = map[string]struct{ tag, prefix string }{
	"_vm_test.go":   {tag: "vm", prefix: "TestVM"},
	"_kube_test.go": {tag: "kube", prefix: "TestKube"},
}

// oldTags are build tags that no file may use anymore.
var oldTags = []string{"hv", "kind", "sbxrfake"}

// skipDirs are directories without source files of the module.
var skipDirs = []string{"testdata", "vendor", "bin", "dist"}

// hosts are the GOOS values that the tests run on.
var hosts = []string{"darwin", "linux"}

// TestTargetFiles fails for target test files that a target task would silently not run.
func TestTargetFiles(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || slices.Contains(skipDirs, d.Name())) {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		rel, _ := filepath.Rel(root, path)
		for _, problem := range checkFile(rel, src) {
			t.Error(problem)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCheckFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, file, src string
		want            int
	}{
		{name: "vm file", file: "a_vm_test.go", src: "//go:build vm && (darwin || linux)\n\npackage a\n\nfunc TestMain(m *testing.M) {}\nfunc TestVMBoot(t *testing.T) {}\nfunc helper() {}\n", want: 0},
		{name: "kube file", file: "a_kube_test.go", src: "//go:build kube\n\npackage a\n\nfunc TestKubeCriteria(t *testing.T) {}\n", want: 0},
		{name: "unit file", file: "a_test.go", src: "package a\n\nfunc TestUnit(t *testing.T) {}\n", want: 0},
		{name: "unit file with a target prefix", file: "a_test.go", src: "package a\n\nfunc TestKubeletRange(t *testing.T) {}\n", want: 1},
		{name: "vm file without constraint", file: "a_vm_test.go", src: "package a\n\nfunc TestVMBoot(t *testing.T) {}\n", want: 1},
		{name: "vm file with kube tag", file: "a_vm_test.go", src: "//go:build kube\n\npackage a\n", want: 1},
		{name: "vm file built without vm", file: "a_vm_test.go", src: "//go:build vm || linux\n\npackage a\n", want: 1},
		{name: "vm file built without vm on linux only", file: "a_vm_test.go", src: "//go:build vm || (linux && !darwin)\n\npackage a\n", want: 1},
		{name: "vm file built without vm on darwin only", file: "a_vm_test.go", src: "//go:build vm || (darwin && !linux)\n\npackage a\n", want: 1},
		{name: "vm file built without vm on unix", file: "a_vm_test.go", src: "//go:build vm || unix\n\npackage a\n", want: 1},
		{name: "kube file built without kube with another tag", file: "a_kube_test.go", src: "//go:build kube || vm\n\npackage a\n", want: 1},
		{name: "kube file on linux only", file: "a_kube_test.go", src: "//go:build kube && linux\n\npackage a\n", want: 0},
		{name: "vm file with an unselected test", file: "a_vm_test.go", src: "//go:build vm\n\npackage a\n\nfunc TestBoot(t *testing.T) {}\nfunc FuzzBoot(f *testing.F) {}\n", want: 2},
		{name: "kube file with a vm test", file: "a_kube_test.go", src: "//go:build kube\n\npackage a\n\nfunc TestVMBoot(t *testing.T) {}\n", want: 2},
		{name: "old tag", file: "a_test.go", src: "//go:build hv\n\npackage a\n", want: 1},
		{name: "old negated tag", file: "a.go", src: "//go:build !sbxrfake\n\npackage a\n", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := checkFile(tt.file, []byte(tt.src)); len(got) != tt.want {
				t.Errorf("checkFile found %d problems %q, want %d", len(got), got, tt.want)
			}
		})
	}
}

// checkFile returns the problems in the Go file name with content src.
func checkFile(name string, src []byte) []string {
	fset := token.NewFileSet()

	f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return []string{err.Error()}
	}

	expr := buildConstraint(f)

	var problems []string

	for _, tag := range tags(expr) {
		if slices.Contains(oldTags, tag) {
			problems = append(problems, fmt.Sprintf("%s: uses the old build tag %q", name, tag))
		}
	}

	for suffix, target := range targets {
		if !strings.HasSuffix(name, suffix) {
			for _, fn := range testFuncs(f) {
				if strings.HasPrefix(fn, target.prefix) {
					problems = append(problems, fmt.Sprintf("%s: %s must be in a %s file, because only %s tests start with %s", name, fn, suffix, target.tag, target.prefix))
				}
			}

			continue
		}

		if !requires(expr, target.tag) {
			problems = append(problems, fmt.Sprintf("%s: needs a //go:build constraint that requires the tag %q", name, target.tag))
		}

		for _, fn := range testFuncs(f) {
			if !strings.HasPrefix(fn, target.prefix) {
				problems = append(problems, fmt.Sprintf("%s: %s must start with %s, or the task skips it", name, fn, target.prefix))
			}
		}
	}

	return problems
}

func buildConstraint(f *ast.File) constraint.Expr {
	for _, group := range f.Comments {
		if group.Pos() > f.Package {
			break
		}

		for _, c := range group.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}

			expr, err := constraint.Parse(c.Text)
			if err == nil {
				return expr
			}
		}
	}

	return nil
}

// requires reports whether expr excludes the file when tag is unset, on every host and whatever the other tags are.
func requires(expr constraint.Expr, tag string) bool {
	if expr == nil {
		return false
	}

	for _, goos := range hosts {
		for _, others := range []bool{false, true} {
			if expr.Eval(func(t string) bool { return hostTag(t, tag, goos, others) }) {
				return false
			}
		}
	}

	return true
}

// hostTag reports whether t is set when the target tag is off and the file builds for goos.
func hostTag(t, tag, goos string, others bool) bool {
	switch {
	case t == tag:
		return false
	case t == goos || t == "unix":
		return true
	case slices.Contains(hosts, t):
		return false
	default:
		return others
	}
}

func tags(expr constraint.Expr) []string {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return []string{e.Tag}
	case *constraint.NotExpr:
		return tags(e.X)
	case *constraint.AndExpr:
		return append(tags(e.X), tags(e.Y)...)
	case *constraint.OrExpr:
		return append(tags(e.X), tags(e.Y)...)
	default:
		return nil
	}
}

// testFuncs returns the top-level functions that go test runs, except TestMain.
func testFuncs(f *ast.File) []string {
	var names []string

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name == "TestMain" {
			continue
		}

		for _, prefix := range []string{"Test", "Fuzz", "Benchmark", "Example"} {
			if strings.HasPrefix(fn.Name.Name, prefix) {
				names = append(names, fn.Name.Name)

				break
			}
		}
	}

	return names
}

func moduleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}

		dir = parent
	}
}
