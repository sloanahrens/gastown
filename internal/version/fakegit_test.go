package version

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGit is an in-memory model of the git repositories a staleness check
// reads: commits with parents and file trees, refs, a HEAD, and remotes. It
// answers exactly the git commands checker issues and fails the test on any
// other, so the unit tests run no processes. Real-git behaviour is pinned by
// the integration tier (stale_integration_test.go).
//
// Every repository in one test shares a fakeGit, the way clones share commit
// hashes; each repository sees only the objects it created or fetched.
type fakeGit struct {
	t     testing.TB
	mu    sync.Mutex
	seq   int
	store map[string]*fakeCommit
	repos map[string]*fakeRepo
}

type fakeCommit struct {
	id      string
	seq     int
	parents []string
	tree    map[string]string // path -> content
}

type fakeRepo struct {
	g       *fakeGit
	dir     string
	objects map[string]bool
	refs    map[string]string // full refname -> commit id
	head    string            // full refname HEAD points at; "" when detached
	detach  string            // commit id when detached
	remotes map[string]string // remote name -> repo dir; "" = unreachable
	calls   [][]string
}

func newFakeGit(t testing.TB) *fakeGit {
	return &fakeGit{t: t, store: map[string]*fakeCommit{}, repos: map[string]*fakeRepo{}}
}

// repo creates a repository at dir whose HEAD is the unborn branch main.
func (g *fakeGit) repo(dir string) *fakeRepo {
	r := &fakeRepo{
		g: g, dir: dir,
		objects: map[string]bool{},
		refs:    map[string]string{},
		head:    "refs/heads/main",
		remotes: map[string]string{},
	}
	g.repos[dir] = r
	return r
}

// checker returns a checker that runs git against g and judges binaryCommit.
func (g *fakeGit) checker(binaryCommit string) checker {
	return checker{git: g.run, commit: binaryCommit}
}

// headCommit returns the commit HEAD resolves to, or "" on an unborn branch.
func (r *fakeRepo) headCommit() string {
	if r.head == "" {
		return r.detach
	}
	return r.refs[r.head]
}

// commit records a commit on HEAD that writes files (path, content pairs) and
// returns its full hash.
func (r *fakeRepo) commit(pathContent ...string) string {
	r.g.t.Helper()
	if len(pathContent)%2 != 0 {
		r.g.t.Fatalf("commit wants path/content pairs, got %q", pathContent)
	}
	return r.commitWithParents(pathContent, r.headCommit())
}

// merge records a merge commit of other into HEAD and returns its hash.
func (r *fakeRepo) merge(other string) string {
	return r.commitWithParents(nil, r.headCommit(), r.resolveOrFail(other))
}

func (r *fakeRepo) commitWithParents(pathContent []string, parents ...string) string {
	g := r.g
	g.seq++
	sum := sha1.Sum([]byte("fake-commit-" + strconv.Itoa(g.seq)))
	c := &fakeCommit{id: hex.EncodeToString(sum[:]), seq: g.seq, tree: map[string]string{}}
	for _, p := range parents {
		if p != "" {
			c.parents = append(c.parents, p)
		}
	}
	if len(c.parents) > 0 {
		for k, v := range g.store[c.parents[0]].tree {
			c.tree[k] = v
		}
	}
	for i := 0; i < len(pathContent); i += 2 {
		c.tree[pathContent[i]] = pathContent[i+1]
	}
	g.store[c.id] = c
	r.objects[c.id] = true
	r.moveHead(c.id)
	return c.id
}

func (r *fakeRepo) moveHead(id string) {
	if r.head == "" {
		r.detach = id
		return
	}
	r.refs[r.head] = id
}

// renameBranch is git branch -M: the current branch takes the new name.
func (r *fakeRepo) renameBranch(name string) {
	old := r.head
	r.head = "refs/heads/" + name
	if id, ok := r.refs[old]; ok {
		delete(r.refs, old)
		r.refs[r.head] = id
	}
}

// branch creates branch name at HEAD without switching to it.
func (r *fakeRepo) branch(name string) {
	r.refs["refs/heads/"+name] = r.headCommit()
}

// checkoutNew is git checkout -b name.
func (r *fakeRepo) checkoutNew(name string) {
	r.branch(name)
	r.head = "refs/heads/" + name
}

// checkout switches HEAD to an existing branch.
func (r *fakeRepo) checkout(name string) {
	r.g.t.Helper()
	ref := "refs/heads/" + name
	if _, ok := r.refs[ref]; !ok {
		r.g.t.Fatalf("checkout %s: no such branch", name)
	}
	r.head = ref
}

// setRef is git update-ref.
func (r *fakeRepo) setRef(ref, id string) {
	r.refs[ref] = id
}

// addRemote configures a remote that fetches from the repo at dir. An empty
// dir, or one no repo was created at, makes the remote unreachable.
func (r *fakeRepo) addRemote(name, dir string) {
	r.remotes[name] = dir
}

// push is git push <remote> <local>:<remoteBranch>, updating this repo's
// remote-tracking ref the way a real push does.
func (r *fakeRepo) push(remote, local, remoteBranch string) {
	r.g.t.Helper()
	dst := r.g.repos[r.remotes[remote]]
	if dst == nil {
		r.g.t.Fatalf("push: remote %s unreachable", remote)
	}
	id := r.resolveOrFail(local)
	r.copyObjects(dst, id)
	dst.refs["refs/heads/"+remoteBranch] = id
	r.refs["refs/remotes/"+remote+"/"+remoteBranch] = id
}

// fetches returns the fetch commands this repo received.
func (r *fakeRepo) fetches() [][]string {
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if c[0] == "fetch" {
			out = append(out, c)
		}
	}
	return out
}

func (r *fakeRepo) resolveOrFail(rev string) string {
	r.g.t.Helper()
	id, ok := r.resolve(rev)
	if !ok {
		r.g.t.Fatalf("resolve %s: unknown revision", rev)
	}
	return id
}

// firstParentPaths returns the paths a commit changed against its first
// parent: the file list `git log --name-only --diff-merges=first-parent`
// prints for it. A commit with no parents lists its whole tree, as git does
// for a root commit.
func (r *fakeRepo) firstParentPaths(id string) []string {
	c := r.g.store[id]
	if len(c.parents) == 0 {
		paths := make([]string, 0, len(c.tree))
		for p := range c.tree {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		return paths
	}
	return treeDiff(r.g.store[c.parents[0]].tree, c.tree, []string{"."})
}

// resolve resolves HEAD, a full refname, a short branch name, or a hash
// prefix of at least four characters to a commit this repo holds.
func (r *fakeRepo) resolve(rev string) (string, bool) {
	if rev == "HEAD" {
		id := r.headCommit()
		return id, id != ""
	}
	if id, ok := r.refs[rev]; ok {
		return id, true
	}
	for _, prefix := range []string{"refs/", "refs/tags/", "refs/heads/", "refs/remotes/"} {
		if id, ok := r.refs[prefix+rev]; ok {
			return id, true
		}
	}
	if len(rev) < 4 {
		return "", false
	}
	var match string
	for id := range r.objects {
		if strings.HasPrefix(id, rev) {
			if match != "" {
				return "", false // ambiguous
			}
			match = id
		}
	}
	return match, match != ""
}

// rangeCommits returns the commits reachable from to but not from from, newest
// first, the way git log walks a range.
func (g *fakeGit) rangeCommits(from, to string) []string {
	exclude := g.ancestors(from)
	var ids []string
	for id := range g.ancestors(to) {
		if !exclude[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return g.store[ids[i]].seq > g.store[ids[j]].seq })
	return ids
}

// ancestors returns every commit reachable from id, id included.
func (g *fakeGit) ancestors(id string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c] {
			continue
		}
		seen[c] = true
		stack = append(stack, g.store[c].parents...)
	}
	return seen
}

func (r *fakeRepo) copyObjects(dst *fakeRepo, id string) {
	for c := range r.g.ancestors(id) {
		dst.objects[c] = true
	}
}

// errExit stands in for git exiting non-zero.
var errExit = errors.New("exit status 1")

// errExit128 stands in for git's fatal exit.
var errExit128 = errors.New("exit status 128")

// run is the gitRunner. It dispatches on the exact argument lists checker
// sends.
func (g *fakeGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := g.repos[dir]
	if r == nil {
		// Not a repository: every command fails the way git's does.
		return nil, errExit128
	}
	r.calls = append(r.calls, args)
	out, err := r.dispatch(args)
	if errors.Is(err, errUnsupported) {
		g.t.Errorf("fake git: unexpected command in %s: git %s", dir, strings.Join(args, " "))
		return nil, errExit128
	}
	return []byte(out), err
}

// errUnsupported marks a command the fake does not model.
var errUnsupported = errors.New("fake git: unsupported command")

func (r *fakeRepo) dispatch(args []string) (string, error) {
	switch {
	case eq(args, "rev-parse", "--is-inside-work-tree"):
		return "true\n", nil

	case len(args) == 5 && eq(args[:4], "rev-parse", "--verify", "--quiet", "--end-of-options"):
		rev, ok := strings.CutSuffix(args[4], "^{commit}")
		if !ok {
			return "", errUnsupported
		}
		id, found := r.resolve(rev)
		if !found {
			return "", errExit
		}
		return id + "\n", nil

	case eq(args, "symbolic-ref", "--short", "HEAD"):
		if r.head == "" {
			return "", errExit128
		}
		return strings.TrimPrefix(r.head, "refs/heads/") + "\n", nil

	case len(args) == 5 && eq(args[:4], "log", gitLogFormat, "--name-only", "--diff-merges=first-parent"):
		from, to, err := r.rangeEnds(args[4])
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, id := range r.g.rangeCommits(from, to) {
			b.WriteString(gitLogRecordSep + id + "\n")
			if paths := r.firstParentPaths(id); len(paths) > 0 {
				b.WriteString("\n" + strings.Join(paths, "\n") + "\n")
			}
		}
		return b.String(), nil

	case len(args) == 4 && eq(args[:2], "merge-base", "--is-ancestor"):
		a, okA := r.resolve(args[2])
		b, okB := r.resolve(args[3])
		if !okA || !okB {
			return "", errExit128
		}
		if r.g.ancestors(b)[a] {
			return "", nil
		}
		return "", errExit

	case len(args) == 3 && eq(args[:2], "for-each-ref", "--format=%(refname)"):
		prefix := args[2]
		var refs []string
		for ref := range r.refs {
			if strings.HasPrefix(ref, prefix) {
				refs = append(refs, ref)
			}
		}
		sort.Strings(refs)
		return lines(refs), nil

	case len(args) == 3 && eq(args[:2], "rev-list", "--count"):
		from, to, err := r.rangeEnds(args[2])
		if err != nil {
			return "", err
		}
		n := 0
		exclude := r.g.ancestors(from)
		for c := range r.g.ancestors(to) {
			if !exclude[c] {
				n++
			}
		}
		return fmt.Sprintf("%d\n", n), nil

	case len(args) >= 4 && eq(args[:2], "diff", "--name-only") && args[3] == "--":
		from, to, err := r.rangeEnds(args[2])
		if err != nil {
			return "", err
		}
		return lines(treeDiff(r.g.store[from].tree, r.g.store[to].tree, args[4:])), nil

	case eq(args, "remote"):
		var names []string
		for name := range r.remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		return lines(names), nil

	case len(args) == 5 && eq(args[:3], "fetch", "--quiet", "--no-tags"):
		return "", r.fetch(args[3], args[4])
	}
	return "", errUnsupported
}

// fetch handles +refs/heads/<b>:refs/remotes/<remote>/<b>.
func (r *fakeRepo) fetch(remote, refspec string) error {
	dir, ok := r.remotes[remote]
	if !ok {
		return errExit128
	}
	src := r.g.repos[dir]
	if src == nil {
		return errExit128
	}
	spec := strings.TrimPrefix(refspec, "+")
	from, to, ok := strings.Cut(spec, ":")
	if !ok {
		return errExit128
	}
	id, ok := src.refs[from]
	if !ok {
		return errExit128
	}
	src.copyObjects(r, id)
	r.refs[to] = id
	return nil
}

func (r *fakeRepo) rangeEnds(spec string) (string, string, error) {
	a, b, ok := strings.Cut(spec, "..")
	if !ok {
		return "", "", errExit128
	}
	from, okA := r.resolve(a)
	to, okB := r.resolve(b)
	if !okA || !okB {
		return "", "", errExit128
	}
	return from, to, nil
}

// treeDiff lists the paths that differ between two trees and match the
// pathspecs: a plain entry includes by prefix ("." is everything) and a
// ":!" entry excludes by prefix.
func treeDiff(a, b map[string]string, pathspecs []string) []string {
	var include, exclude []string
	for _, p := range pathspecs {
		if rest, ok := strings.CutPrefix(p, ":!"); ok {
			exclude = append(exclude, rest)
		} else {
			include = append(include, p)
		}
	}
	under := func(path, dir string) bool {
		return dir == "." || path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
	}
	keep := func(path string) bool {
		in := len(include) == 0
		for _, d := range include {
			in = in || under(path, d)
		}
		for _, d := range exclude {
			if under(path, d) {
				return false
			}
		}
		return in
	}
	changed := map[string]bool{}
	for p, v := range a {
		if b[p] != v {
			changed[p] = true
		}
	}
	for p, v := range b {
		if a[p] != v {
			changed[p] = true
		}
	}
	var out []string
	for p := range changed {
		if keep(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func eq(args []string, want ...string) bool {
	if len(args) != len(want) {
		return false
	}
	for i := range args {
		if args[i] != want[i] {
			return false
		}
	}
	return true
}

func lines(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.Join(items, "\n") + "\n"
}
