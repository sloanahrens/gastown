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

	"github.com/steveyegge/gastown/internal/callercwd"
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

// checkoutRoot is the module root the tests run in: the nearest directory at or
// above the working directory that holds go.mod, or "" when there is none. A test
// binary runs in its package's directory, so this is the checkout being scanned.
// The directory comes from callercwd.Getwd, so GT_CALLER_CWD names it when set.
var checkoutRoot = sync.OnceValue(func() string {
	dir, err := callercwd.Getwd()
	if err != nil {
		return ""
	}
	return moduleRootAbove(dir)
})

// moduleRootAbove returns the nearest directory at or above dir that holds go.mod,
// or "" when none does.
func moduleRootAbove(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Clean(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// cacheable reports whether dir is part of the checkout rather than a test's own
// fixture. A fixture lives under the temporary directory; but the checkout may
// live under it too (a sweep or CI container whose TMPDIR holds the workspace), so
// being under the temporary directory decides nothing by itself. The more specific
// of the two wins: dir is a fixture when the temporary directory is the checkout
// root or nested inside it and dir is under it, and is the checkout when it is
// under the checkout root and no temporary directory nested in that root holds it.
// When the temporary directory is the checkout root, a fixture and a checkout
// directory are both under it and cannot be told apart, so both are read fresh:
// the scan is slower, and a fixture is never read stale.
func cacheable(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	return cacheableUnder(checkoutRoot(), tempRoots(), abs)
}

// cacheableUnder is cacheable for an absolute dir, over a checkout root and the
// temporary directory's spellings.
func cacheableUnder(root string, temps []string, dir string) bool {
	if root == "" || !within(root, dir) {
		return false
	}
	for _, t := range temps {
		if within(root, t) && within(t, dir) {
			return false // the temporary directory is the checkout or inside it: dir is a fixture
		}
	}
	return true
}

// within reports whether path is dir or below it.
func within(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
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
