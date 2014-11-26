package main

import (
	"bufio"
	"fmt"
	"go/build"
	gc "launchpad.net/gocheck"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestPackage(t *testing.T) {
	gc.TestingT(t)
}

type suite struct {
	savedBuildContext build.Context
	savedErrorf       func(string, ...interface{})
	errors            []string
}

var _ = gc.Suite(&suite{})

func (s *suite) SetUpTest(c *gc.C) {
	s.savedBuildContext = buildContext
	s.savedErrorf = errorf
	errorf = func(f string, a ...interface{}) {
		s.errors = append(s.errors, fmt.Sprintf(f, a...))
	}
}

func (s *suite) TearDownTest(c *gc.C) {
	buildContext = s.savedBuildContext
	errorf = s.savedErrorf
	s.errors = nil
}

type listResult struct {
	project string
}

var listTests = []struct {
	about    string
	args     []string
	testDeps bool
	result   string
	errors   []string
}{{
	about: "easy case",
	args:  []string{"foo/foo1"},
	result: `
bar bzr 1
foo hg 0
foo/foo2 hg 0
`[1:],
}, {
	about:    "with test dependencies",
	args:     []string{"foo/foo1"},
	testDeps: true,
	result: `
bar bzr 1
baz bzr 1
foo hg 0
foo/foo2 hg 0
khroomph bzr 1
`[1:],
}, {
	about:    "test dependencies included from packages early in list",
	args:     []string{"foo/foo99", "foo/foo1"},
	testDeps: true,
	result: `
bar bzr 1
baz bzr 1
foo hg 0
foo/foo2 hg 0
khroomph bzr 1
`[1:],
}, {
	about: "ambiguous dependency",
	args:  []string{"ambiguous1"},
	result: `
multirepo bzr 1
multirepo hg 0
`[1:],
	errors: []string{
		`ambiguous VCS (bzr) for "multirepo" at "$tmp/p1/src/multirepo"`,
		`ambiguous VCS (hg) for "multirepo" at "$tmp/p1/src/multirepo"`,
		`bzr repository at "$tmp/p1/src/multirepo" is not clean; revision id may not reflect the code`,
		`hg repository at "$tmp/p1/src/multirepo" is not clean; revision id may not reflect the code`,
	}}, {
	about: "ambiguous dependency across different GOPATH elements",
	args:  []string{"ambiguous2"},
	result: `
multirepo bzr 1
multirepo hg 0
multirepo hg 0
`[1:],
	errors: []string{
		`ambiguous VCS (bzr) for "multirepo" at "$tmp/p1/src/multirepo"`,
		`ambiguous VCS (hg) for "multirepo" at "$tmp/p1/src/multirepo"`,
		`ambiguous VCS (hg) for "multirepo" at "$tmp/p2/src/multirepo"`,
		`bzr repository at "$tmp/p1/src/multirepo" is not clean; revision id may not reflect the code`,
		`hg repository at "$tmp/p1/src/multirepo" is not clean; revision id may not reflect the code`,
	}}, {
	about: "unclean hg",
	args:  []string{"hgunclean-root"},
	result: `
hgunclean hg 0
`[1:],
	errors: []string{
		`hg repository at "$tmp/p1/src/hgunclean" is not clean; revision id may not reflect the code`,
	},
}}

func (s *suite) TestList(c *gc.C) {
	dir := c.MkDir()
	gopath := []string{filepath.Join(dir, "p1"), filepath.Join(dir, "p2")}
	writePackages(c, gopath[0], "v1", map[string]packageSpec{
		"foo/foo1": {
			deps:      []string{"foo/foo2"},
			testDeps:  []string{"baz/baz1"},
			xTestDeps: []string{"khroomph/khr"},
		},
		"foo/foo2": {
			deps: []string{"bar/bar1"},
		},
		"foo/foo99": {
			deps: []string{"foo/foo1"},
		},
		"baz/baz1":     {},
		"khroomph/khr": {},
		"ambiguous1": {
			deps: []string{"multirepo/x"},
		},
		"ambiguous2": {
			deps: []string{"multirepo/x", "multirepo/y"},
		},
		"multirepo/x": {},
		"bzrunclean-root": {
			deps: []string{"bzrunclean"},
		},
		"bzrunclean": {},
		"hgunclean-root": {
			deps: []string{"hgunclean"},
		},
		"hgunclean": {},
	})
	writePackages(c, gopath[1], "v1", map[string]packageSpec{
		"bar/bar1": {
			deps: []string{"foo/foo3", "bar/bar2"},
		},
		"bar/bar2": {
			deps: []string{"bar/bar3"},
		},
		"bar/bar3":    {},
		"bar/bar4":    {},
		"foo/foo1":    {},
		"foo/foo3":    {},
		"multirepo/y": {},
	})
	var wg sync.WaitGroup
	goInitRepo := func(kind string, rootDir string, pkg string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			initRepo(c, kind, rootDir, pkg)
		}()
	}
	goInitRepo("bzr", gopath[0], "foo/foo1")
	goInitRepo("hg", gopath[0], "foo/foo2")
	goInitRepo("bzr", gopath[0], "foo/foo99")
	goInitRepo("bzr", gopath[0], "baz")
	goInitRepo("bzr", gopath[0], "khroomph")
	goInitRepo("bzr", gopath[0], "ambiguous1")
	// deliberately omit ambiguous2
	goInitRepo("bzr", gopath[0], "multirepo")
	goInitRepo("hg", gopath[0], "multirepo")
	goInitRepo("bzr", gopath[0], "bzrunclean")
	goInitRepo("hg", gopath[0], "hgunclean")

	goInitRepo("bzr", gopath[1], "bar")
	goInitRepo("hg", gopath[1], "foo")
	goInitRepo("hg", gopath[1], "multirepo")
	wg.Wait()

	// unclean repos
	for _, pkg := range []string{"hgunclean", "bzrunclean"} {
		f, err := os.Create(filepath.Join(pkgDir(gopath[0], pkg), "extra"))
		c.Assert(err, gc.IsNil)
		f.Close()
	}

	buildContext.GOPATH = strings.Join(gopath, string(filepath.ListSeparator))

	for i, test := range listTests {
		c.Logf("test %d. %s", i, test.about)
		s.errors = nil
		deps := list(test.args, test.testDeps)

		for i, e := range s.errors {
			s.errors[i] = strings.Replace(e, dir, "$tmp", -1)
		}
		sort.Strings(test.errors)
		sort.Strings(s.errors)
		c.Assert(s.errors, gc.DeepEquals, test.errors)

		// Check that rev ids are non-empty, but don't check specific values.
		result := ""
		for i, info := range deps {
			c.Check(info.revid, gc.Not(gc.Equals), "", gc.Commentf("info %d: %v", i, info))
			info.revid = ""
			result += fmt.Sprintf("%s %s %s\n", info.project, info.vcs.Kind(), info.revno)
		}
		c.Check(result, gc.Equals, test.result)
	}
}

func pkgDir(rootDir string, pkg string) string {
	return filepath.Join(rootDir, "src", filepath.FromSlash(pkg))
}

func initRepo(c *gc.C, kind, rootDir, pkg string) {
	// This relies on the fact that hg, bzr and git
	// all use the same command to initialize a directory.
	dir := pkgDir(rootDir, pkg)
	_, err := runCmd(dir, kind, "init")
	if !c.Check(err, gc.IsNil) {
		return
	}

	args := []string{"add"}
	files, _ := filepath.Glob(dir + "/[^.]*")
	args = append(args, files...)
	_, err = runCmd(dir, kind, args...)
	if !c.Check(err, gc.IsNil) {
		return
	}
	commitRepo(c, dir, kind, "initial commit")
}

func commitRepo(c *gc.C, dir, kind string, message string) {
	// This relies on the fact that hg, bzr and git
	// all use the same command to initialize a directory.
	_, err := runCmd(dir, kind, "commit", "-m", message)
	c.Check(err, gc.IsNil)
}

type packageSpec struct {
	deps      []string
	testDeps  []string
	xTestDeps []string
}

func writePackages(c *gc.C, rootDir string, version string, pkgs map[string]packageSpec) {
	for name, pkg := range pkgs {
		dir := pkgDir(rootDir, name)
		err := os.MkdirAll(dir, 0777)
		c.Assert(err, gc.IsNil)
		writeFile := func(fileName, pkgIdent string, deps []string) {
			err := writePackageFile(filepath.Join(dir, fileName), pkgIdent, version, deps)
			c.Assert(err, gc.IsNil)
		}
		writeFile("x.go", "x", pkg.deps)
		writeFile("internal_test.go", "x", pkg.testDeps)
		writeFile("x_test.go", "x_test", pkg.xTestDeps)
	}
}

func writePackageFile(fileName string, pkgIdent string, version string, deps []string) error {
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, "package %s\nimport (\n", pkgIdent)
	for _, dep := range deps {
		fmt.Fprintf(w, "\t_ %q\n", dep)
	}
	fmt.Fprintf(w, ")\n")
	fmt.Fprintf(w, "const Version = %q\n", version)
	return w.Flush()
}
