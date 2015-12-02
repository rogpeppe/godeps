package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kisielk/gotool"

	"launchpad.net/godeps/pkgrepo"
)

var (
	revFile       = flag.String("u", "", "update dependencies")
	testDeps      = flag.Bool("t", false, "include testing dependencies")
	printCommands = flag.Bool("x", false, "show executed commands")
	dryRun        = flag.Bool("n", false, "print but do not execute update commands")
	_             = flag.Bool("f", true, "(deprecated, superceded by -F) when updating, try to fetch deps if the update fails")
	noFetch       = flag.Bool("F", false, "when updating, do not try to fetch deps if the update fails")
	parallel      = flag.Int("P", 1, "max number of concurrent updates")
	ifNewer       = flag.Bool("N", false, "when updating, only update if the dependency is newer")
)

var exitCode = 0

var buildContext = build.Default

var usage = `
Usage:
	godeps [flags] [pkg ...]
	godeps -u file [flags]

In the first form of usage (without the -u flag), godeps prints to
standard output a list of all the source dependencies of the named
packages (or the package in the current directory if none is given).
If there is ambiguity in the source-control systems used, godeps will
print all the available versions and an error, exiting with a false
status. It is up to the user to remove lines from the output to make
the output suitable for input to godeps -u.

In the second form, godeps updates source to versions specified by
the -u file argument, which should hold version information in the
same form printed by godeps. It is an error if the file contains more
than one line for the same package root. If a specified revision is not
currently available, godeps will attempt to fetch it, unless the -F flag
is provided.
`[1:]

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "%s\n", usage)
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if *revFile != "" {
		if flag.NArg() != 0 {
			flag.Usage()
		}
		update(*revFile)
	} else {
		pkgs := flag.Args()
		if len(pkgs) == 0 {
			pkgs = []string{"."}
		}
		pkgs = gotool.ImportPaths(pkgs)
		for _, info := range list(pkgs, *testDeps) {
			fmt.Println(info)
		}
	}
	os.Exit(exitCode)
}

func update(file string) {
	projects, err := parseDepFile(file)
	if err != nil {
		errorf("cannot parse %q: %v", file, err)
		return
	}
	// First get info on all the projects, make sure their working
	// directories are all clean and prune out the ones which
	// don't need updating.
	for proj, info := range projects {
		if info.notThere {
			if *noFetch {
				errorf("%q does not exist", proj)
				delete(projects, proj)
			}
			continue
		}
		currentInfo, err := info.vcs.Info(info.dir)
		if err != nil {
			errorf("cannot get information on %q: %v", info.dir, err)
			delete(projects, proj)
			continue
		}
		if !currentInfo.clean {
			errorf("%q is not clean; will not update", info.dir)
			delete(projects, proj)
			continue
		}
		if currentInfo.revid == info.revid || *ifNewer && !info.newer(currentInfo) {
			// No need to update.
			delete(projects, proj)
		}
	}
	updateProjects(projects)
}

func updateProjects(projects map[string]*depInfo) {
	limit := make(chan struct{}, *parallel)
	type result struct {
		info *depInfo
		err  error
	}
	results := make(chan result)
	for _, info := range projects {
		info := info
		go func() {
			limit <- struct{}{}
			err := updateProject(info)
			<-limit
			results <- result{info, err}
		}()
	}
	n := 0
	for i := 0; i < len(projects); i++ {
		r := <-results
		if r.err != nil {
			errorf("cannot update %q: %v", r.info.dir, r.err)
			continue
		}
		n++
		fmt.Printf("%s now at %s\n", r.info.project, r.info.revid)
	}
	if n < len(projects) {
		fmt.Printf("%d repositories updated; %d failed\n", n, len(projects)-n)
	}
}

func updateProject(info *depInfo) error {
	err := info.vcs.Update(info.dir, info.revid)
	if err == nil || *noFetch {
		return nil
	}
	fmt.Printf("update %s failed; trying to fetch newer version\n", info.project)
	if info.notThere {
		if err := createRepo(info); err != nil {
			return fmt.Errorf("cannot create repo: %v", err)
		}
	} else {
		if err := info.vcs.Fetch(info.dir); err != nil {
			return err
		}
	}
	return info.vcs.Update(info.dir, info.revid)
}

func createRepo(info *depInfo) error {
	// We would much prefer to just do:
	//
	//	_, err := runCmd(".", "go", "get", "-nodeps", "-d", project)
	//
	// here, but there's no way to prevent go get from downloading
	// dependencies recursively, which means that we can get
	// extraneous dependencies when the package tip has dependencies
	// not mentioned by the target revision, which can in turn cause
	// build scripts that are particular about such things to fail.
	// See also https://go-review.googlesource.com/#/c/8725/
	//
	// Instead, we use code abstracted from the go tool to do the
	// job.
	root, err := pkgrepo.Root(info.project)
	if err != nil {
		return fmt.Errorf("cannot find project root: %v", err)
	}
	if string(root.VCS) != info.vcs.Kind() {
		return fmt.Errorf("project has unexpected VCS kind %s; want %s", root.VCS, info.vcs.Kind())
	}

	gopath := filepath.SplitList(buildContext.GOPATH)
	if len(gopath) == 0 {
		return fmt.Errorf("GOPATH not set")
	}
	rootDir := filepath.Join(gopath[0], "src", filepath.FromSlash(root.Root))

	// The rest of this function is also taken directly from
	// the downloadPackage function in the go tool.

	// Some version control tools require the parent of the target to exist.
	parent, _ := filepath.Split(rootDir)
	if err := os.MkdirAll(parent, 0777); err != nil {
		return err
	}
	if err := info.vcs.Create(root.Repo, rootDir); err != nil {
		return err
	}
	return nil
}

func parseDepFile(file string) (map[string]*depInfo, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	deps := make(map[string]*depInfo)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		info, err := parseDepInfo(line)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q: %v", line, err)
		}
		if deps[info.project] != nil {
			return nil, fmt.Errorf("project %q has more than one entry", info.project)
		}
		deps[info.project] = info
		gopath := filepath.SplitList(buildContext.GOPATH)
		if len(gopath) == 0 {
			return nil, fmt.Errorf("GOPATH not set")
		}
		info.dir, err = projectToDir(info.project, gopath)
		if err != nil {
			if err != errProjectNotFound {
				return nil, fmt.Errorf("cannot find directory for %q: %v", info.project, err)
			}
			// The project does not currently exist but
			// we may try to fetch it later. When we do, go
			// get will fetch it to the first element of
			// $GOPATH, so set the directory to that.
			info.dir = filepath.Join(gopath[0], "src", filepath.FromSlash(info.project))
			info.notThere = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read error: %v", err)
	}
	return deps, nil
}

func list(pkgs []string, testDeps bool) []*depInfo {
	infoByDir := make(map[string][]*depInfo)
	// We want to ignore the go core source and the projects
	// for the root packages. Do this by getting leaf dependency info
	// for all those things and adding them to an ignore list.
	ignoreDirs := make(map[string]bool)

	// Add a dummy dependency info entry for GOROOT.
	// This prevents findVCSInfo from failing and ensures
	// that GOROOT projects will be ignored, even though
	// there may not be a VCS under $GOROOT.
	infoByDir[filepath.Clean(buildContext.GOROOT)] = []*depInfo{}
	for _, pkgPath := range pkgs {
		pkg, err := buildContext.Import(pkgPath, ".", build.FindOnly)
		if err != nil {
			errorf("cannot find %q: %v", pkgPath, err)
			continue
		}
		if !findVCSInfo(pkg.Dir, infoByDir) {
			ignoreDirs[pkg.Dir] = true
		}
	}
	// Ignore the packages directly specified on the
	// command line, as we want to print the versions
	// of their dependencies, not their versions themselves.
	for dir := range infoByDir {
		ignoreDirs[dir] = true
	}
	walkDeps(pkgs, testDeps, func(pkg *build.Package, err error) bool {
		if err != nil {
			errorf("cannot import %q: %v", pkg.Name, err)
			return false
		}
		if !findVCSInfo(pkg.Dir, infoByDir) && !ignoreDirs[pkg.Dir] {
			errorf("no version control system found for %q", pkg.Dir)
		}
		return true
	})
	// We make a new map because dependency information
	// can be ambiguous not only through there being two
	// or more metadata directories in one directory, but
	// also because there can be different packages with
	// the same project name under different GOPATH
	// elements.
	infoByProject := make(map[string][]*depInfo)
	for dir, infos := range infoByDir {
		if ignoreDirs[dir] {
			continue
		}
		proj, err := dirToProject(dir)
		if err != nil {
			errorf("cannot get relative repo root for %q: %v", dir, err)
			continue
		}
		infoByProject[proj] = append(infoByProject[proj], infos...)
	}
	var deps depInfoSlice
	for proj, infos := range infoByProject {
		if len(infos) > 1 {
			for _, info := range infos {
				errorf("ambiguous VCS (%s) for %q at %q", info.vcs.Kind(), proj, info.dir)
			}
		}
		for _, info := range infos {
			if ignoreDirs[info.dir] {
				continue
			}
			if !info.clean {
				errorf("%s repository at %q is not clean; revision id may not reflect the code", info.vcs.Kind(), info.dir)
			}
			info.project = proj
			deps = append(deps, info)
		}
	}
	sort.Sort(deps)
	return deps
}

func dirToProject(dir string) (string, error) {
	if ok, _ := relativeToParent(buildContext.GOROOT, dir); ok {
		return "go", nil
	}
	for _, p := range filepath.SplitList(buildContext.GOPATH) {
		if ok, rel := relativeToParent(filepath.Join(p, "src"), dir); ok {
			return rel, nil
		}
	}
	return "", fmt.Errorf("project directory not found in GOPATH or GOROOT")
}

func projectToDir(proj string, gopath []string) (string, error) {
	for _, p := range gopath {
		dir := filepath.Join(p, "src", filepath.FromSlash(proj))
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", errProjectNotFound
}

var errProjectNotFound = errors.New("not found in GOPATH")

// relativeToParent returns whether the child
// path is under (or the same as) the parent path,
// and if so returns the trailing portion of the
// child path that is under the parent path.
func relativeToParent(parent, child string) (ok bool, rel string) {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)

	if parent == child {
		return true, ""
	}

	if !strings.HasPrefix(child, parent+string(filepath.Separator)) {
		return false, ""
	}
	return true, child[len(parent)+1:]
}

type depInfoSlice []*depInfo

func (s depInfoSlice) Len() int      { return len(s) }
func (s depInfoSlice) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s depInfoSlice) Less(i, j int) bool {
	p, q := s[i], s[j]
	if p.project != q.project {
		return p.project < q.project
	}
	if p.vcs.Kind() != q.vcs.Kind() {
		return p.vcs.Kind() < q.vcs.Kind()
	}
	return p.dir < q.dir
}

type depInfo struct {
	project  string
	notThere bool
	dir      string
	vcs      VCS
	VCSInfo
}

func (info *depInfo) String() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s", info.project, info.vcs.Kind(), info.revid, info.revno)
}

// parseDepInfo parses a dependency info line as printed by
// depInfo.String.
func parseDepInfo(s string) (*depInfo, error) {
	fields := strings.Split(s, "\t")
	if len(fields) != 4 {
		return nil, fmt.Errorf("expected 4 tab-separated fields, got %d", len(fields))
	}
	info := &depInfo{
		project: fields[0],
		vcs:     kindToVCS[fields[1]],
		VCSInfo: VCSInfo{
			revid: fields[2],
			revno: fields[3],
		},
	}
	if info.vcs == nil {
		return nil, fmt.Errorf("unknown VCS kind %q", fields[1])
	}
	if info.project == "" {
		return nil, fmt.Errorf("empty project field")
	}
	if info.revid == "" {
		return nil, fmt.Errorf("empty revision id")
	}
	return info, nil
}

// findVCSInfo searches for VCS info for the given directory
// and adds any found to infoByDir, searching each parent
// directory in turn. It returns whether any information was
// found.
func findVCSInfo(dir string, infoByDir map[string][]*depInfo) bool {
	dir, err := filepath.Abs(dir)
	if err != nil {
		errorf("cannot find absolute path of %q", dir)
		return false
	}
	dirs := parents(dir)
	// Check from the root down that there is no
	// existing information for any parent directory.
	for i := len(dirs) - 1; i >= 0; i-- {
		if info := infoByDir[dirs[i]]; info != nil {
			return true
		}
	}
	// Check from dir upwards to find a VCS directory
	for _, dir := range dirs {
		nfound := 0
		for metaDir, vcs := range metadataDirs {
			if dirInfo, err := os.Stat(filepath.Join(dir, metaDir)); err == nil && dirInfo.IsDir() {
				info, err := vcs.Info(dir)
				if err != nil {
					errorf("cannot get version information for %q: %v", dir, err)
					continue
				}
				infoByDir[dir] = append(infoByDir[dir], &depInfo{
					dir:     dir,
					vcs:     vcs,
					VCSInfo: info,
				})
				nfound++
			}
		}
		if nfound > 0 {
			return true
		}
	}
	return false
}

// parents returns the given path and all its parents.
// For instance, given /usr/rog/foo,
// it will return []string{"/usr/rog/foo", "/usr/rog", "/usr", "/"}
func parents(path string) []string {
	var all []string
	path = filepath.Clean(path)
	for {
		all = append(all, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return all
}

type walkContext struct {
	checked map[string]bool
	visit   func(*build.Package, error) bool
}

// walkDeps traverses the import dependency tree of the
// given package, calling the given function for each dependency,
// including the package for pkgPath itself. If the function
// returns true, the dependencies of the given package
// will themselves be visited.
// The includeTests flag specifies whether test-related dependencies
// will be considered when walking the hierarchy.
// Each package will be visited at most once.
func walkDeps(paths []string, includeTests bool, visit func(*build.Package, error) bool) {
	ctxt := &walkContext{
		checked: make(map[string]bool),
		visit:   visit,
	}
	for _, path := range paths {
		ctxt.walkDeps(path, includeTests)
	}
}

func (ctxt *walkContext) walkDeps(pkgPath string, includeTests bool) {
	if pkgPath == "C" {
		return
	}
	if !includeTests && ctxt.checked[pkgPath] {
		// The package has already been, is or being, checked.
		// Note that when tests are being included (it's a top level
		// package), we don't return here, because we may have encountered
		// the package before as a dependency of a previous package
		// and so not have included tests for it.
		return
	}
	// BUG(rog) This ignores files that are excluded by
	// as part of the current build. Unfortunately
	// we can't use UseAllFiles as that includes other
	// files that break the build (for instance unrelated
	// helper commands in package main).
	// The solution is to avoid using build.Import but it's convenient
	// at the moment.
	pkg, err := buildContext.Import(pkgPath, ".", 0)
	ctxt.checked[pkg.ImportPath] = true
	descend := ctxt.visit(pkg, err)
	if err != nil || !descend {
		return
	}
	// N.B. is it worth eliminating duplicates here?
	var allImports []string
	allImports = append(allImports, pkg.Imports...)
	if includeTests {
		allImports = append(allImports, pkg.TestImports...)
		allImports = append(allImports, pkg.XTestImports...)
	}
	for _, impPath := range allImports {
		// testing dependencies are not transitively included.
		ctxt.walkDeps(impPath, false)
	}
}

type VCS interface {
	Kind() string
	Info(dir string) (VCSInfo, error)
	Update(dir, revid string) error
	Create(repo, rootDir string) error
	Fetch(dir string) error
}

type VCSInfo struct {
	revid string
	revno string // optional
	clean bool
}

// newer reports whether i0 is newer than i1.
// This will only work for VCSs that use a date stamp
// (currently only git) or a single integer.
func (i0 VCSInfo) newer(i1 VCSInfo) bool {
	n0, err0 := strconv.Atoi(i0.revno)
	n1, err1 := strconv.Atoi(i1.revno)
	if err0 == nil && err1 == nil {
		return n0 > n1
	}
	t0, err0 := time.Parse(time.RFC3339, i0.revno)
	t1, err1 := time.Parse(time.RFC3339, i1.revno)
	if err0 == nil && err1 == nil {
		return t0.After(t1)
	}
	return false
}

var metadataDirs = map[string]VCS{
	".bzr": bzrVCS{},
	".hg":  hgVCS{},
	".git": gitVCS{},
}

var kindToVCS = map[string]VCS{
	"bzr": bzrVCS{},
	"hg":  hgVCS{},
	"git": gitVCS{},
}

type gitVCS struct{}

func (gitVCS) Kind() string {
	return "git"
}

func (gitVCS) Info(dir string) (VCSInfo, error) {
	out, err := runCmd(dir, "git", "log", "-n", "1", "--pretty=format:%H %ct", "HEAD")
	if err != nil {
		return VCSInfo{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return VCSInfo{}, fmt.Errorf("unexpected git log output %q", out)
	}
	revid := fields[0]
	// validate the revision hash
	revhash, err := hex.DecodeString(revid)
	if err != nil || len(revhash) == 0 {
		return VCSInfo{},
			fmt.Errorf("git rev-parse provided invalid revision %q", revid)
	}
	unixTime, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return VCSInfo{},
			fmt.Errorf("git rev-parse provided invalid time %q", fields[1])
	}

	// `git status --porcelain` outputs one line per changed or untracked file.
	out, err = runCmd(dir, "git", "status", "--porcelain")
	if err != nil {
		return VCSInfo{}, err
	}
	return VCSInfo{
		revid: revid,
		// Empty output (with rc=0) indicates no changes in working copy.
		clean: out == "",
		revno: time.Unix(unixTime, 0).UTC().Format(time.RFC3339),
	}, nil
}

func (gitVCS) Create(repo, rootDir string) error {
	_, err := runUpdateCmd("", "git", "clone", repo, rootDir)
	return err
}

func (gitVCS) Update(dir string, revid string) error {
	_, err := runUpdateCmd(dir, "git", "checkout", revid)
	return err
}

func (gitVCS) Fetch(dir string) error {
	_, err := runCmd(dir, "git", "fetch")
	return err
}

type bzrVCS struct{}

func (bzrVCS) Kind() string {
	return "bzr"
}

var validBzrInfo = regexp.MustCompile(`^([0-9.]+) ([^ \t]+)$`)
var shelveLine = regexp.MustCompile(`^[0-9]+ (shelves exist|shelf exists)\.`)

func (bzrVCS) Info(dir string) (VCSInfo, error) {
	out, err := runCmd(dir, "bzr", "revision-info", "--tree")
	if err != nil {
		return VCSInfo{}, err
	}
	m := validBzrInfo.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return VCSInfo{}, fmt.Errorf("bzr revision-info has unexpected result %q", out)
	}

	out, err = runCmd(dir, "bzr", "status", "-S")
	if err != nil {
		return VCSInfo{}, err
	}
	clean := true
	statusLines := strings.Split(out, "\n")
	for _, line := range statusLines {
		if line == "" || shelveLine.MatchString(line) {
			continue
		}
		clean = false
		break
	}
	return VCSInfo{
		revid: m[2],
		revno: m[1],
		clean: clean,
	}, nil
}

func (bzrVCS) Create(repo, rootDir string) error {
	_, err := runUpdateCmd("", "bzr", "branch", repo, rootDir)
	return err
}

func (bzrVCS) Update(dir string, revid string) error {
	_, err := runUpdateCmd(dir, "bzr", "update", "-r", "revid:"+revid)
	return err
}

func (bzrVCS) Fetch(dir string) error {
	_, err := runCmd(dir, "bzr", "pull")
	return err
}

var validHgInfo = regexp.MustCompile(`^([a-f0-9]+) ([0-9]+)$`)

type hgVCS struct{}

func (hgVCS) Info(dir string) (VCSInfo, error) {
	out, err := runCmd(dir, "hg", "log", "-l", "1", "-r", ".", "--template", "{node} {rev}")
	if err != nil {
		return VCSInfo{}, err
	}
	m := validHgInfo.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return VCSInfo{}, fmt.Errorf("hg identify has unexpected result %q", out)
	}
	out, err = runCmd(dir, "hg", "status")
	if err != nil {
		return VCSInfo{}, err
	}
	// TODO(rog) check that tree is clean
	return VCSInfo{
		revid: m[1],
		revno: m[2],
		clean: out == "",
	}, nil
}

func (hgVCS) Kind() string {
	return "hg"
}

func (hgVCS) Create(repo, rootDir string) error {
	_, err := runUpdateCmd("", "hg", "clone", "-U", repo, rootDir)
	return err
}

func (hgVCS) Update(dir string, revid string) error {
	_, err := runUpdateCmd(dir, "hg", "update", revid)
	return err
}

func (hgVCS) Fetch(dir string) error {
	_, err := runCmd(dir, "hg", "pull")
	return err
}

func runUpdateCmd(dir string, name string, args ...string) (string, error) {
	if *dryRun {
		printShellCommand(dir, name, args)
		return "", nil
	}
	return runCmd(dir, name, args...)
}

func runCmd(dir string, name string, args ...string) (string, error) {
	var outData, errData bytes.Buffer
	if *printCommands {
		printShellCommand(dir, name, args)
	}
	c := exec.Command(name, args...)
	c.Stdout = &outData
	c.Stderr = &errData
	c.Dir = dir
	err := c.Run()
	if err == nil {
		return outData.String(), nil
	}
	if _, ok := err.(*exec.ExitError); ok && errData.Len() > 0 {
		return "", errors.New(strings.TrimSpace(errData.String()))
	}
	return "", fmt.Errorf("cannot run %q: %v", append([]string{name}, args...), err)
}

var errorf = func(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "godeps: %s\n", fmt.Sprintf(f, a...))
	exitCode = 1
}

var (
	outputDirMutex sync.Mutex
	outputDir      string
)

func printShellCommand(dir, name string, args []string) {
	outputDirMutex.Lock()
	defer outputDirMutex.Unlock()
	if dir != outputDir {
		fmt.Fprintf(os.Stderr, "cd %s\n", shquote(dir))
		outputDir = dir
	}
	var buf bytes.Buffer
	buf.WriteString(name)
	for _, arg := range args {
		buf.WriteString(" ")
		buf.WriteString(shquote(arg))
	}
	fmt.Fprintf(os.Stderr, "%s\n", buf.Bytes())
}

func shquote(s string) string {
	// single-quote becomes single-quote, double-quote, single-quote, double-quote, single-quote
	return `'` + strings.Replace(s, `'`, `'"'"'`, -1) + `'`
}
