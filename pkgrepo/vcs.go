// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var buildV = false
var buildX = false

type VCS string

const (
	Git VCS = "git"
	Hg  VCS = "hg"
	Svn VCS = "svn"
	Bzr VCS = "bzr"
)

// A vcsCmd describes how to use a version control system
// like Mercurial, Git, or Subversion.
type vcsCmd struct {
	name string
	cmd  string // name of binary to invoke command

	scheme  []string
	pingCmd string
}

// vcsList lists the known version control systems
var vcsList = []*vcsCmd{
	vcsHg,
	vcsGit,
	vcsSvn,
	vcsBzr,
}

// vcsByCmd returns the version control system for the given
// command name (hg, git, svn, bzr).
func vcsByCmd(cmd string) *vcsCmd {
	for _, vcs := range vcsList {
		if vcs.cmd == cmd {
			return vcs
		}
	}
	return nil
}

// vcsHg describes how to use Mercurial.
var vcsHg = &vcsCmd{
	name: "Mercurial",
	cmd:  "hg",

	scheme:  []string{"https", "http", "ssh"},
	pingCmd: "identify {scheme}://{repo}",
}

// vcsGit describes how to use Git.
var vcsGit = &vcsCmd{
	name: "Git",
	cmd:  "git",

	scheme:  []string{"git", "https", "http", "git+ssh"},
	pingCmd: "ls-remote {scheme}://{repo}",
}

// vcsBzr describes how to use Bazaar.
var vcsBzr = &vcsCmd{
	name: "Bazaar",
	cmd:  "bzr",

	scheme:  []string{"https", "http", "bzr", "bzr+ssh"},
	pingCmd: "info {scheme}://{repo}",
}

// vcsSvn describes how to use Subversion.
var vcsSvn = &vcsCmd{
	name: "Subversion",
	cmd:  "svn",

	scheme:  []string{"https", "http", "svn", "svn+ssh"},
	pingCmd: "info {scheme}://{repo}",
}

func (v *vcsCmd) String() string {
	return v.name
}

// run runs the command line cmd in the given directory.
// keyval is a list of key, value pairs.  run expands
// instances of {key} in cmd into value, but only after
// splitting cmd into individual arguments.
// If an error occurs, run prints the command line and the
// command's combined stdout+stderr to standard error.
// Otherwise run discards the command's output.
func (v *vcsCmd) run(dir string, cmd string, keyval ...string) error {
	_, err := v.run1(dir, cmd, keyval, true)
	return err
}

// runVerboseOnly is like run but only generates error output to standard error in verbose mode.
func (v *vcsCmd) runVerboseOnly(dir string, cmd string, keyval ...string) error {
	_, err := v.run1(dir, cmd, keyval, false)
	return err
}

// runOutput is like run but returns the output of the command.
func (v *vcsCmd) runOutput(dir string, cmd string, keyval ...string) ([]byte, error) {
	return v.run1(dir, cmd, keyval, true)
}

// run1 is the generalized implementation of run and runOutput.
func (v *vcsCmd) run1(dir string, cmdline string, keyval []string, verbose bool) ([]byte, error) {
	m := make(map[string]string)
	for i := 0; i < len(keyval); i += 2 {
		m[keyval[i]] = keyval[i+1]
	}
	args := strings.Fields(cmdline)
	for i, arg := range args {
		args[i] = expand(m, arg)
	}

	_, err := exec.LookPath(v.cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"go: missing %s command. See http://golang.org/s/gogetcmd\n",
			v.name)
		return nil, err
	}

	cmd := exec.Command(v.cmd, args...)
	cmd.Dir = dir
	cmd.Env = envForDir(cmd.Dir)
	if buildX {
		fmt.Printf("cd %s\n", dir)
		fmt.Printf("%s %s\n", v.cmd, strings.Join(args, " "))
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	out := buf.Bytes()
	if err != nil {
		if verbose || buildV {
			fmt.Fprintf(os.Stderr, "# cd %s; %s %s\n", dir, v.cmd, strings.Join(args, " "))
			os.Stderr.Write(out)
		}
		return nil, err
	}
	return out, nil
}

// ping pings to determine scheme to use.
func (v *vcsCmd) ping(scheme, repo string) error {
	return v.runVerboseOnly(".", v.pingCmd, "scheme", scheme, "repo", repo)
}

// A vcsPath describes how to convert an import path into a
// version control system and repository name.
type vcsPath struct {
	prefix string                              // prefix this description applies to
	re     string                              // pattern for import path
	repo   string                              // repository to use (expand with match of re)
	vcs    string                              // version control system to use (expand with match of re)
	check  func(match map[string]string) error // additional checks
	ping   bool                                // ping for scheme to use to download repo

	regexp *regexp.Regexp // cached compiled form of re
}

// VCSRoot represents a version control system, a repo, and a root of
// where to put it on disk.
type VCSRoot struct {
	VCS VCS

	// Repo is the repository URL, including scheme
	Repo string

	// Root is the import path corresponding to the root of the
	// repository
	Root string
}

var httpPrefixRE = regexp.MustCompile(`^https?:`)

// VCSRootForImportPath analyzes importPath to determine the
// version control system, and code repository to use.
func Root(importPath string) (*VCSRoot, error) {
	rr, err := vcsRootForImportPathStatic(importPath, "")
	if err == errUnknownSite {
		// If there are wildcards, look up the thing before the wildcard,
		// hoping it applies to the wildcarded parts too.
		// This makes 'go get rsc.io/pdf/...' work in a fresh GOPATH.
		lookup := strings.TrimSuffix(importPath, "/...")
		if i := strings.Index(lookup, "/.../"); i >= 0 {
			lookup = lookup[:i]
		}
		rr, err = vcsRootForImportDynamic(lookup)

		// vcsRootForImportDynamic returns error detail
		// that is irrelevant if the user didn't intend to use a
		// dynamic import in the first place.
		// Squelch it.
		if err != nil {
			if buildV {
				log.Printf("import %q: %v", importPath, err)
			}
			err = fmt.Errorf("unrecognized import path %q", importPath)
		}
	}

	if err == nil && strings.Contains(importPath, "...") && strings.Contains(rr.Root, "...") {
		// Do not allow wildcards in the repo root.
		rr = nil
		err = fmt.Errorf("cannot expand ... in %q", importPath)
	}
	return rr, err
}

var errUnknownSite = errors.New("dynamic lookup required to find mapping")

// vcsRootForImportPathStatic attempts to map importPath to a
// VCSRoot using the commonly-used VCS hosting sites in vcsPaths
// (github.com/user/dir), or from a fully-qualified importPath already
// containing its VCS type (foo.com/repo.git/dir)
//
// If scheme is non-empty, that scheme is forced.
func vcsRootForImportPathStatic(importPath, scheme string) (*VCSRoot, error) {
	// A common error is to use https://packagepath because that's what
	// hg and git require. Diagnose this helpfully.
	if loc := httpPrefixRE.FindStringIndex(importPath); loc != nil {
		// The importPath has been cleaned, so has only one slash. The pattern
		// ignores the slashes; the error message puts them back on the RHS at least.
		return nil, fmt.Errorf("%q not allowed in import path", importPath[loc[0]:loc[1]]+"//")
	}
	for _, srv := range vcsPaths {
		if !strings.HasPrefix(importPath, srv.prefix) {
			continue
		}
		m := srv.regexp.FindStringSubmatch(importPath)
		if m == nil {
			if srv.prefix != "" {
				return nil, fmt.Errorf("invalid %s import path %q", srv.prefix, importPath)
			}
			continue
		}

		// Build map of named subexpression matches for expand.
		match := map[string]string{
			"prefix": srv.prefix,
			"import": importPath,
		}
		for i, name := range srv.regexp.SubexpNames() {
			if name != "" && match[name] == "" {
				match[name] = m[i]
			}
		}
		if srv.vcs != "" {
			match["vcs"] = expand(match, srv.vcs)
		}
		if srv.repo != "" {
			match["repo"] = expand(match, srv.repo)
		}
		if srv.check != nil {
			if err := srv.check(match); err != nil {
				return nil, err
			}
		}
		vcs := vcsByCmd(match["vcs"])
		if vcs == nil {
			return nil, fmt.Errorf("unknown version control system %q", match["vcs"])
		}
		if srv.ping {
			if scheme != "" {
				match["repo"] = scheme + "://" + match["repo"]
			} else {
				for _, scheme := range vcs.scheme {
					if vcs.ping(scheme, match["repo"]) == nil {
						match["repo"] = scheme + "://" + match["repo"]
						break
					}
				}
			}
		}
		rr := &VCSRoot{
			VCS:  VCS(vcs.cmd),
			Repo: match["repo"],
			Root: match["root"],
		}
		return rr, nil
	}
	return nil, errUnknownSite
}

// vcsRootForImportDynamic finds a *VCSRoot for a custom domain that's not
// statically known by vcsRootForImportPathStatic.
//
// This handles "vanity import paths" like "name.tld/pkg/foo".
func vcsRootForImportDynamic(importPath string) (*VCSRoot, error) {
	slash := strings.Index(importPath, "/")
	if slash < 0 {
		return nil, errors.New("import path does not contain a slash")
	}
	host := importPath[:slash]
	if !strings.Contains(host, ".") {
		return nil, errors.New("import path does not begin with hostname")
	}
	urlStr, body, err := httpsOrHTTP(importPath)
	if err != nil {
		return nil, fmt.Errorf("http/https fetch: %v", err)
	}
	defer body.Close()
	imports, err := parseMetaGoImports(body)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %v", importPath, err)
	}
	metaImport, err := matchGoImport(imports, importPath)
	if err != nil {
		if err != errNoMatch {
			return nil, fmt.Errorf("parse %s: %v", urlStr, err)
		}
		return nil, fmt.Errorf("parse %s: no go-import meta tags", urlStr)
	}
	if buildV {
		log.Printf("get %q: found meta tag %#v at %s", importPath, metaImport, urlStr)
	}
	// If the import was "uni.edu/bob/project", which said the
	// prefix was "uni.edu" and the RepoRoot was "evilroot.com",
	// make sure we don't trust Bob and check out evilroot.com to
	// "uni.edu" yet (possibly overwriting/preempting another
	// non-evil student).  Instead, first verify the root and see
	// if it matches Bob's claim.
	if metaImport.Prefix != importPath {
		if buildV {
			log.Printf("get %q: verifying non-authoritative meta tag", importPath)
		}
		urlStr0 := urlStr
		urlStr, body, err = httpsOrHTTP(metaImport.Prefix)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %v", urlStr, err)
		}
		imports, err := parseMetaGoImports(body)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %v", importPath, err)
		}
		if len(imports) == 0 {
			return nil, fmt.Errorf("fetch %s: no go-import meta tag", urlStr)
		}
		metaImport2, err := matchGoImport(imports, importPath)
		if err != nil || metaImport != metaImport2 {
			return nil, fmt.Errorf("%s and %s disagree about go-import for %s", urlStr0, urlStr, metaImport.Prefix)
		}
	}

	if !strings.Contains(metaImport.RepoRoot, "://") {
		return nil, fmt.Errorf("%s: invalid repo root %q; no scheme", urlStr, metaImport.RepoRoot)
	}
	rr := &VCSRoot{
		VCS:  VCS(vcsByCmd(metaImport.VCS).cmd),
		Repo: metaImport.RepoRoot,
		Root: metaImport.Prefix,
	}
	if rr.VCS == "" {
		return nil, fmt.Errorf("%s: unknown vcs %q", urlStr, metaImport.VCS)
	}
	return rr, nil
}

// metaImport represents the parsed <meta name="go-import"
// content="prefix vcs reporoot" /> tags from HTML files.
type metaImport struct {
	Prefix, VCS, RepoRoot string
}

// errNoMatch is returned from matchGoImport when there's no applicable match.
var errNoMatch = errors.New("no import match")

// matchGoImport returns the metaImport from imports matching importPath.
// An error is returned if there are multiple matches.
// errNoMatch is returned if none match.
func matchGoImport(imports []metaImport, importPath string) (_ metaImport, err error) {
	match := -1
	for i, im := range imports {
		if !strings.HasPrefix(importPath, im.Prefix) {
			continue
		}
		if match != -1 {
			err = fmt.Errorf("multiple meta tags match import path %q", importPath)
			return
		}
		match = i
	}
	if match == -1 {
		err = errNoMatch
		return
	}
	return imports[match], nil
}

// expand rewrites s to replace {k} with match[k] for each key k in match.
func expand(match map[string]string, s string) string {
	for k, v := range match {
		s = strings.Replace(s, "{"+k+"}", v, -1)
	}
	return s
}

// vcsPaths lists the known vcs paths.
var vcsPaths = []*vcsPath{
	// Google Code - new syntax
	{
		prefix: "code.google.com/",
		re:     `^(?P<root>code\.google\.com/p/(?P<project>[a-z0-9\-]+)(\.(?P<subrepo>[a-z0-9\-]+))?)(/[A-Za-z0-9_.\-]+)*$`,
		repo:   "https://{root}",
		check:  googleCodeVCS,
	},

	// Google Code - old syntax
	{
		re:    `^(?P<project>[a-z0-9_\-.]+)\.googlecode\.com/(git|hg|svn)(?P<path>/.*)?$`,
		check: oldGoogleCode,
	},

	// Github
	{
		prefix: "github.com/",
		re:     `^(?P<root>github\.com/[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
		check:  noVCSSuffix,
	},

	// Bitbucket
	{
		prefix: "bitbucket.org/",
		re:     `^(?P<root>bitbucket\.org/(?P<bitname>[A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`,
		repo:   "https://{root}",
		check:  bitbucketVCS,
	},

	// Launchpad
	{
		prefix: "launchpad.net/",
		re:     `^(?P<root>launchpad\.net/((?P<project>[A-Za-z0-9_.\-]+)(?P<series>/[A-Za-z0-9_.\-]+)?|~[A-Za-z0-9_.\-]+/(\+junk|[A-Za-z0-9_.\-]+)/[A-Za-z0-9_.\-]+))(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "bzr",
		repo:   "https://{root}",
		check:  launchpadVCS,
	},

	// IBM DevOps Services (JazzHub)
	{
		prefix: "hub.jazz.net/git",
		re:     `^(?P<root>hub.jazz.net/git/[a-z0-9]+/[A-Za-z0-9_.\-]+)(/[A-Za-z0-9_.\-]+)*$`,
		vcs:    "git",
		repo:   "https://{root}",
		check:  noVCSSuffix,
	},

	// General syntax for any server.
	{
		re:   `^(?P<root>(?P<repo>([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?/[A-Za-z0-9_.\-/]*?)\.(?P<vcs>bzr|git|hg|svn))(/[A-Za-z0-9_.\-]+)*$`,
		ping: true,
	},
}

func init() {
	// fill in cached regexps.
	// Doing this eagerly discovers invalid regexp syntax
	// without having to run a command that needs that regexp.
	for _, srv := range vcsPaths {
		srv.regexp = regexp.MustCompile(srv.re)
	}
}

// noVCSSuffix checks that the repository name does not
// end in .foo for any version control system foo.
// The usual culprit is ".git".
func noVCSSuffix(match map[string]string) error {
	repo := match["repo"]
	for _, vcs := range vcsList {
		if strings.HasSuffix(repo, "."+vcs.cmd) {
			return fmt.Errorf("invalid version control suffix in %s path", match["prefix"])
		}
	}
	return nil
}

var googleCheckout = regexp.MustCompile(`id="checkoutcmd">(hg|git|svn)`)

// googleCodeVCS determines the version control system for
// a code.google.com repository, by scraping the project's
// /source/checkout page.
func googleCodeVCS(match map[string]string) error {
	if err := noVCSSuffix(match); err != nil {
		return err
	}
	data, err := httpGET(expand(match, "https://code.google.com/p/{project}/source/checkout?repo={subrepo}"))
	if err != nil {
		return err
	}

	if m := googleCheckout.FindSubmatch(data); m != nil {
		if vcs := vcsByCmd(string(m[1])); vcs != nil {
			// Subversion requires the old URLs.
			// TODO: Test.
			if vcs == vcsSvn {
				if match["subrepo"] != "" {
					return fmt.Errorf("sub-repositories not supported in Google Code Subversion projects")
				}
				match["repo"] = expand(match, "https://{project}.googlecode.com/svn")
			}
			match["vcs"] = vcs.cmd
			return nil
		}
	}

	return fmt.Errorf("unable to detect version control system for code.google.com/ path")
}

// oldGoogleCode is invoked for old-style foo.googlecode.com paths.
// It prints an error giving the equivalent new path.
func oldGoogleCode(match map[string]string) error {
	return fmt.Errorf("invalid Google Code import path: use %s instead",
		expand(match, "code.google.com/p/{project}{path}"))
}

// bitbucketVCS determines the version control system for a
// Bitbucket repository, by using the Bitbucket API.
func bitbucketVCS(match map[string]string) error {
	if err := noVCSSuffix(match); err != nil {
		return err
	}

	var resp struct {
		SCM string `json:"scm"`
	}
	url := expand(match, "https://api.bitbucket.org/1.0/repositories/{bitname}")
	data, err := httpGET(url)
	if err != nil {
		if httpErr, ok := err.(*httpError); ok && httpErr.statusCode == 403 {
			// this may be a private repository. If so, attempt to determine which
			// VCS it uses. See issue 5375.
			root := match["root"]
			for _, vcs := range []string{"git", "hg"} {
				if vcsByCmd(vcs).ping("https", root) == nil {
					resp.SCM = vcs
					break
				}
			}
		}

		if resp.SCM == "" {
			return err
		}
	} else {
		if err := json.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("decoding %s: %v", url, err)
		}
	}

	if vcsByCmd(resp.SCM) != nil {
		match["vcs"] = resp.SCM
		if resp.SCM == "git" {
			match["repo"] += ".git"
		}
		return nil
	}

	return fmt.Errorf("unable to detect version control system for bitbucket.org/ path")
}

// launchpadVCS solves the ambiguity for "lp.net/project/foo". In this case,
// "foo" could be a series name registered in Launchpad with its own branch,
// and it could also be the name of a directory within the main project
// branch one level up.
func launchpadVCS(match map[string]string) error {
	if match["project"] == "" || match["series"] == "" {
		return nil
	}
	_, err := httpGET(expand(match, "https://code.launchpad.net/{project}{series}/.bzr/branch-format"))
	if err != nil {
		match["root"] = expand(match, "launchpad.net/{project}")
		match["repo"] = expand(match, "https://{root}")
	}
	return nil
}
