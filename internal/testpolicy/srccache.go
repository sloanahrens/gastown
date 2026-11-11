package testpolicy

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The policy tests (TestPolicy, TestDockerTier, TestUnitTierMain, TestAllowedTools,
// TestGitFree, TestRealGit, the contracts check) each walk the same package
// directories. Without a cache each of them asked go/build for every file's build
// constraints (an open and a read) and parsed every file again, so one run read
// and parsed each source file a dozen times; on a runner with a cold file cache
// and few cores that was most of the package's wall time.
//
// repoSources holds one parse of each file and one build-constraint answer, for
// the files of the checkout the tests scan. Those do not change while the tests
// run. A directory under the temporary directory is a test's own fixture, which a
// test may rewrite between two scans, so it is read fresh every time. Every
// checker reads the tree and none edits it, which is what lets one parse be
// shared; a checker that edited a node would change what the next one saw.

// parseMode is the one parse mode the cached files use: comments kept, and
// identifiers resolved, which the checkers that read an identifier's Obj need.
const parseMode = parser.ParseComments

// repoSources is the process-wide cache of the scanned checkout.
var repoSources = &sourceCache{
	fset:   token.NewFileSet(),
	files:  map[string]*cachedFile{},
	builds: map[string]*cachedMatch{},
}

type sourceCache struct {
	fset *token.FileSet

	mu     sync.Mutex
	files  map[string]*cachedFile
	builds map[string]*cachedMatch
}

type cachedFile struct {
	once sync.Once
	file *ast.File
	err  error
}

type cachedMatch struct {
	once sync.Once
	ok   bool
	err  error
}

// tempRoots are the spellings of the temporary directory a fixture lives under.
var tempRoots = sync.OnceValue(func() []string {
	t := os.TempDir()
	roots := []string{filepath.Clean(t)}
	if r, err := filepath.EvalSymlinks(t); err == nil {
		roots = append(roots, filepath.Clean(r))
	}
	return roots
})

// cacheable reports whether dir is part of the checkout: not under the temporary
// directory, where tests build their fixtures.
func cacheable(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for _, root := range tempRoots() {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return false
		}
	}
	return true
}

// sourceSet is how one scan of a directory reads its files: the FileSet its
// positions belong to, and whether the parses come from the cache.
type sourceSet struct {
	fset   *token.FileSet
	cached bool
}

// sourcesFor returns the source set for a scan of dir.
func sourcesFor(dir string) sourceSet {
	if cacheable(dir) {
		return sourceSet{fset: repoSources.fset, cached: true}
	}
	return sourceSet{fset: token.NewFileSet()}
}

// matches reports whether the file dir/name builds for the current platform with
// no extra tags, as go/build's MatchFile answers.
func (s sourceSet) matches(dir, name string) (bool, error) {
	if !s.cached {
		return build.Default.MatchFile(dir, name)
	}
	path := filepath.Join(dir, name)
	repoSources.mu.Lock()
	m := repoSources.builds[path]
	if m == nil {
		m = &cachedMatch{}
		repoSources.builds[path] = m
	}
	repoSources.mu.Unlock()
	m.once.Do(func() {
		m.ok, m.err = build.Default.MatchFile(dir, name)
		if m.err != nil {
			m.err = fmt.Errorf("%s: %w", path, m.err)
		}
	})
	return m.ok, m.err
}

// parse returns the parsed file at path, with comments and resolved identifiers.
// A cached tree is shared: the caller must not edit it.
func (s sourceSet) parse(path string) (*ast.File, error) {
	if !s.cached {
		return parser.ParseFile(s.fset, path, nil, parseMode)
	}
	repoSources.mu.Lock()
	c := repoSources.files[path]
	if c == nil {
		c = &cachedFile{}
		repoSources.files[path] = c
	}
	repoSources.mu.Unlock()
	c.once.Do(func() {
		c.file, c.err = parser.ParseFile(repoSources.fset, path, nil, parseMode)
	})
	return c.file, c.err
}
