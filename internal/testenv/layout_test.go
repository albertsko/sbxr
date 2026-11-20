package testenv_test

import (
	"fmt"
	"go/ast"
	"go/build"
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

// skipDirs are directories without source files of the module.
var skipDirs = []string{"testdata", "vendor", "bin", "dist"}

// platforms are the supported GOOS and GOARCH pairs.
var platforms = [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}}

// platformTags are the build tags that the platform sets. unix is set on every supported GOOS.
var platformTags = []string{"darwin", "linux", "amd64", "arm64", "unix"}

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
		{name: "vm file with kube tag", file: "a_vm_test.go", src: "//go:build kube\n\npackage a\n", want: 2},
		{name: "vm file built without vm", file: "a_vm_test.go", src: "//go:build vm || linux\n\npackage a\n", want: 1},
		{name: "vm file built without vm on linux only", file: "a_vm_test.go", src: "//go:build vm || (linux && !darwin)\n\npackage a\n", want: 1},
		{name: "vm file built without vm on darwin only", file: "a_vm_test.go", src: "//go:build vm || (darwin && !linux)\n\npackage a\n", want: 1},
		{name: "vm file built without vm on unix", file: "a_vm_test.go", src: "//go:build vm || unix\n\npackage a\n", want: 1},
		{name: "kube file built without kube with another tag", file: "a_kube_test.go", src: "//go:build kube || vm\n\npackage a\n", want: 2},
		{name: "kube file on linux only", file: "a_kube_test.go", src: "//go:build kube && linux\n\npackage a\n", want: 0},
		{name: "vm file built without vm on amd64", file: "a_vm_test.go", src: "//go:build vm || (amd64 && !arm64)\n\npackage a\n", want: 1},
		{name: "vm file that needs the kube tag too", file: "a_vm_test.go", src: "//go:build vm && kube\n\npackage a\n", want: 2},
		{name: "vm file built without vm for one other tag", file: "a_vm_test.go", src: "//go:build vm || (kube && !foo)\n\npackage a\n", want: 2},
		{name: "vm file that never builds with the gc compiler", file: "a_vm_test.go", src: "//go:build vm && !gc\n\npackage a\n", want: 1},
		{name: "vm file for the current Go release", file: "a_vm_test.go", src: "//go:build vm && gc && go1.21 && cgo\n\npackage a\n", want: 0},
		{name: "vm file on no supported platform", file: "a_vm_test.go", src: "//go:build vm && windows\n\npackage a\n", want: 1},
		{name: "vm file with an unselected test", file: "a_vm_test.go", src: "//go:build vm\n\npackage a\n\nfunc TestBoot(t *testing.T) {}\nfunc FuzzBoot(f *testing.F) {}\n", want: 2},
		{name: "kube file with a vm test", file: "a_kube_test.go", src: "//go:build kube\n\npackage a\n\nfunc TestVMBoot(t *testing.T) {}\n", want: 2},
		{name: "test file with the vm tag", file: "a_test.go", src: "//go:build vm\n\npackage a\n\nfunc TestVMBoot(t *testing.T) {}\n", want: 2},
		{name: "test file without the vm tag", file: "a_test.go", src: "//go:build !vm\n\npackage a\n", want: 1},
		{name: "source file with the kube tag", file: "a.go", src: "//go:build kube && linux\n\npackage a\n", want: 1},
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

	for suffix, target := range targets {
		if !strings.HasSuffix(name, suffix) {
			if slices.Contains(tags(expr), target.tag) {
				problems = append(problems, fmt.Sprintf("%s: the tag %q is only for %s files, because the %s task runs no other file", name, target.tag, suffix, target.tag))
			}

			for _, fn := range testFuncs(f) {
				if strings.HasPrefix(fn, target.prefix) {
					problems = append(problems, fmt.Sprintf("%s: %s must be in a %s file, because only %s tests start with %s", name, fn, suffix, target.tag, target.prefix))
				}
			}

			continue
		}

		if !requires(expr, target.tag) {
			problems = append(problems, fmt.Sprintf("%s: needs a //go:build constraint that requires the tag %q and builds with only that tag", name, target.tag))
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

// requires reports whether expr never builds without tag, on any platform and with any other tags, and builds
// with tag and the toolchain's implicit tags on at least one platform, with or without cgo.
func requires(expr constraint.Expr, tag string) bool {
	if expr == nil {
		return false
	}

	var free []string

	for _, t := range tags(expr) {
		if t != tag && !slices.Contains(platformTags, t) && !slices.Contains(free, t) {
			free = append(free, t)
		}
	}

	implicit := append([]string{build.Default.Compiler, "unix"}, build.Default.ReleaseTags...)
	builds := false

	for _, platform := range platforms {
		for set := range 1 << len(free) {
			on := func(t string) bool {
				if i := slices.Index(free, t); i >= 0 {
					return set&(1<<i) != 0
				}

				return t == "unix" || slices.Contains(platform[:], t)
			}

			if expr.Eval(on) {
				return false
			}
		}

		for _, cgo := range []bool{false, true} {
			builds = builds || expr.Eval(func(t string) bool {
				return t == tag || (t == "cgo" && cgo) || slices.Contains(implicit, t) || slices.Contains(platform[:], t)
			})
		}
	}

	return builds
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
