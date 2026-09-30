// Package gitfake is an in-memory model of git repositories that stands in
// for *git.Git in other packages' unit tests (docs/testing.md, "Seams for
// external tools"). It models commits (parents, a flat tree of path to
// content, a message), refs, HEAD, remotes and linked worktrees, and answers
// the methods of *git.Git that converted consumers call. RunRepoContract pins
// it to real git; the integration tier runs the same contract against
// *git.Git.
//
// It grows only by the methods a converted consumer needs. Known limits:
//   - A path both sides of a merge changed conflicts, even where git's
//     line-level merge would combine the edits.
//   - Commit ids are unique per commit made, not content hashes, and log
//     order is the order commits were made (git's is commit date).
//   - The town-root guard and hooks are not modeled.
//   - A repository is the directory it was made at: a subdirectory of a
//     checkout is not one. A clone of a remote whose HEAD names a missing
//     branch, or that holds only tags, is not modeled.
//   - Clone filters, references and depth are accepted and ignored.
package gitfake

import (
	"crypto/sha1" //nolint:gosec // G505: ids, not security
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// commit is one commit: its parents, its tree (path to content) and message.
// seq orders commits the way commit dates order git's log.
type commit struct {
	id      string
	parents []string
	tree    map[string]string
	message string
	seq     int
}

// repo is one repository at a path: its refs, HEAD, remotes, the commits it
// holds, and its worktrees. A non-bare repository's own checkout is main.
type repo struct {
	path      string
	bare      bool
	refs      map[string]string
	head      string // a full ref name, or a commit id when detached
	remotes   map[string]string
	pushURLs  map[string]string // remote to push URL, where one is set
	has       map[string]bool
	main      *worktree
	worktrees map[string]*worktree
}

// worktree is a checkout: its own HEAD (for a repository's main checkout,
// the repository's HEAD), and a merge in progress.
type worktree struct {
	path  string
	head  string // linked worktrees only; main uses repo.head
	merge *mergeState
}

// mergeState is a merge stopped on conflicts. A squash merge records no
// MERGE_HEAD, so git refuses to abort it.
type mergeState struct {
	squash    bool
	conflicts []string
}

// Fake is a world of repositories keyed by path. Its zero value is not
// usable; call New.
type Fake struct {
	mu      sync.Mutex
	objects map[string]*commit
	repos   map[string]*repo
	seq     int
	wt      worktreeState // staging and trees (index.go)
}

// New returns an empty world.
func New() *Fake {
	return &Fake{objects: map[string]*commit{}, repos: map[string]*repo{}}
}

func clean(dir string) string { return filepath.Clean(dir) }

// InitBare makes an empty bare repository at dir with HEAD on main.
func (f *Fake) InitBare(t testing.TB, dir string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = clean(dir)
	f.repos[dir] = &repo{path: dir, bare: true, refs: map[string]string{}, head: "refs/heads/main",
		remotes: map[string]string{}, has: map[string]bool{}, worktrees: map[string]*worktree{}}
}

// Commit makes a commit in the repository at dir on branch: its parent is
// the branch's tip (none when the branch is new), its tree the parent's with
// files written over it, and it becomes the branch's tip. It returns the
// commit's id. A worktree checked out on the branch is not updated.
func (f *Fake) Commit(t testing.TB, dir, branch, message string, files map[string]string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[clean(dir)]
	if r == nil {
		t.Fatalf("gitfake: no repository at %s", dir)
	}
	ref := "refs/heads/" + branch
	tree := map[string]string{}
	var parents []string
	if tip, ok := r.refs[ref]; ok {
		parents = []string{tip}
		for p, c := range f.objects[tip].tree {
			tree[p] = c
		}
	}
	for p, c := range files {
		tree[p] = c
	}
	id := f.newCommit(parents, tree, message)
	r.has[id] = true
	r.refs[ref] = id
	return id
}

// SetRef points ref (a full ref name) at id in the repository at dir.
func (f *Fake) SetRef(t testing.TB, dir, ref, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[clean(dir)]
	if r == nil || f.objects[id] == nil {
		t.Fatalf("gitfake: SetRef %s %s in %s: no such repository or commit", ref, id, dir)
	}
	f.copyObjects(r, id)
	r.refs[ref] = id
}

// Clone clones the repository at src to a non-bare repository at dest with
// src as origin: every branch becomes a remote-tracking ref, and src's HEAD
// branch is checked out (written to dest).
func (f *Fake) Clone(t testing.TB, src, dest string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.repos[clean(src)]
	if s == nil {
		t.Fatalf("gitfake: Clone: no repository at %s", src)
	}
	dest = clean(dest)
	r := &repo{path: dest, refs: map[string]string{}, head: s.head, remotes: map[string]string{"origin": s.path},
		has: map[string]bool{}, worktrees: map[string]*worktree{}}
	r.main = &worktree{path: dest}
	for ref, id := range s.refs {
		if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			f.copyObjects(r, id)
			r.refs["refs/remotes/origin/"+branch] = id
		}
	}
	if id, ok := s.refs[s.head]; ok {
		r.refs[s.head] = id
	}
	f.repos[dest] = r
	if err := materializeCheckout(dest, f.treeOf(r.refs[r.head])); err != nil {
		t.Fatalf("gitfake: Clone: %v", err)
	}
}

// Open returns the implementation of Repo for the repository or worktree at
// dir, as git.NewGit(dir) does. Every call on a directory that is neither
// fails the way git does outside a repository.
func (f *Fake) Open(dir string) Repo {
	return &handle{f: f, dir: clean(dir)}
}

// OpenWithDir is Open for git.NewGitWithDir(gitDir, workDir): the repository
// at gitDir (a bare repository, or a checkout's .git) when it is set, else
// workDir.
func (f *Fake) OpenWithDir(gitDir, workDir string) Repo {
	if gitDir == "" {
		return f.Open(workDir)
	}
	gitDir = clean(gitDir)
	if filepath.Base(gitDir) == ".git" {
		gitDir = filepath.Dir(gitDir)
	}
	return &handle{f: f, dir: gitDir}
}

// Parents returns the parents of commit id, or nil when it has none or is
// unknown.
func (f *Fake) Parents(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.objects[id]; c != nil {
		return append([]string(nil), c.parents...)
	}
	return nil
}

// Tree returns a copy of commit id's tree, or nil when it is unknown.
func (f *Fake) Tree(id string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.objects[id]; c != nil {
		return copyTree(c.tree)
	}
	return nil
}

// Ref returns what ref (a full ref name) points at in the repository at dir,
// or "" when it is unset.
func (f *Fake) Ref(dir, ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.repos[clean(dir)]; r != nil {
		return r.refs[ref]
	}
	return ""
}

func (f *Fake) newCommit(parents []string, tree map[string]string, message string) string {
	f.seq++
	h := sha1.New() //nolint:gosec // G401: ids, not security
	fmt.Fprintf(h, "%d\x00%s\x00%s", f.seq, strings.Join(parents, " "), message)
	id := hex.EncodeToString(h.Sum(nil))
	f.objects[id] = &commit{id: id, parents: parents, tree: copyTree(tree), message: message, seq: f.seq}
	return id
}

func (f *Fake) treeOf(id string) map[string]string {
	if c := f.objects[id]; c != nil {
		return c.tree
	}
	return nil
}

// copyObjects gives r every commit reachable from id.
func (f *Fake) copyObjects(r *repo, id string) {
	for _, a := range f.ancestors(id) {
		r.has[a] = true
	}
}

// ancestors returns id and every commit reachable from it.
func (f *Fake) ancestors(id string) []string {
	seen := map[string]bool{}
	stack := []string{id}
	var out []string
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c] || f.objects[c] == nil {
			continue
		}
		seen[c] = true
		out = append(out, c)
		stack = append(stack, f.objects[c].parents...)
	}
	return out
}

func (f *Fake) isAncestor(a, b string) bool {
	for _, c := range f.ancestors(b) {
		if c == a {
			return true
		}
	}
	return false
}

// mergeBase returns a best common ancestor of a and b: a common ancestor no
// other common ancestor descends from. It is "" for unrelated histories.
func (f *Fake) mergeBase(a, b string) string {
	inA := map[string]bool{}
	for _, c := range f.ancestors(a) {
		inA[c] = true
	}
	var common []string
	for _, c := range f.ancestors(b) {
		if inA[c] {
			common = append(common, c)
		}
	}
	for _, c := range common {
		best := true
		for _, o := range common {
			if o != c && f.isAncestor(c, o) {
				best = false
				break
			}
		}
		if best {
			return c
		}
	}
	return ""
}

// rangeCommits returns the commits reachable from head and not from base,
// newest first.
func (f *Fake) rangeCommits(base, head string) []*commit {
	excluded := map[string]bool{}
	for _, c := range f.ancestors(base) {
		excluded[c] = true
	}
	var out []*commit
	for _, c := range f.ancestors(head) {
		if !excluded[c] {
			out = append(out, f.objects[c])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

func copyTree(t map[string]string) map[string]string {
	out := make(map[string]string, len(t))
	for p, c := range t {
		out[p] = c
	}
	return out
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for p, c := range a {
		if bc, ok := b[p]; !ok || bc != c {
			return false
		}
	}
	return true
}

// merge3 merges ours and theirs from base path by path. A path only one side
// changed takes that side; a path both changed the same way takes it; a path
// both changed differently conflicts and keeps ours in result.
func merge3(base, ours, theirs map[string]string) (result map[string]string, conflicts []string) {
	result = map[string]string{}
	paths := map[string]bool{}
	for _, t := range []map[string]string{base, ours, theirs} {
		for p := range t {
			paths[p] = true
		}
	}
	for p := range paths {
		b, inB := base[p]
		o, inO := ours[p]
		th, inT := theirs[p]
		switch {
		case inO == inT && o == th:
			if inO {
				result[p] = o
			}
		case inO == inB && o == b:
			if inT {
				result[p] = th
			}
		case inT == inB && th == b:
			if inO {
				result[p] = o
			}
		default:
			conflicts = append(conflicts, p)
			if inO {
				result[p] = o
			}
		}
	}
	sort.Strings(conflicts)
	return result, conflicts
}

// materialize replaces everything in dir but .git with tree.
func materialize(dir string, tree map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	for p, content := range tree {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: a checkout's files are world-readable, as git writes them
			return err
		}
	}
	return nil
}

// materializeCheckout writes a new checkout at dir: its .git directory (held
// empty; the model is in memory) and tree.
func materializeCheckout(dir string, tree map[string]string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		return err
	}
	return materialize(dir, tree)
}

// lineDiff returns the lines removed from a and added in b by a minimal edit
// script (a longest common subsequence), each line with its newline.
func lineDiff(a, b string) (removed, added []string) {
	al, bl := splitLines(a), splitLines(b)
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			removed = append(removed, al[i])
			i++
		default:
			added = append(added, bl[j])
			j++
		}
	}
	removed = append(removed, al[i:]...)
	added = append(added, bl[j:]...)
	return removed, added
}

func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// gitErr is the error git.Git returns for a failed command: a *git.GitError
// carrying git's stderr and exit status.
func gitErr(code int, stderr string, args ...string) error {
	command := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			command = a
			break
		}
	}
	return &git.GitError{Command: command, Args: args, Stderr: strings.TrimSpace(stderr), Err: exitError(code)}
}

// exitError is a git exit status, matched through ExitCode() like
// *exec.ExitError.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }
