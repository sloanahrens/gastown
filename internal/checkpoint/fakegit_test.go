package checkpoint

import (
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

// fakeRepo is an in-memory git repository: commits with parents, messages and
// file trees, branches, HEAD, and the index a soft reset leaves behind. It
// answers exactly the git commands this package sends and fails the test on
// any other, so the unit tests start no process. Real-git behaviour is pinned
// by the integration tier (checkpoint_integration_test.go).
type fakeRepo struct {
	t       testing.TB
	mu      sync.Mutex
	seq     int
	commits map[string]*fakeCommit
	refs    map[string]string // "refs/heads/<name>" -> commit id
	head    string            // branch HEAD points at
	index   map[string]string // staged tree; nil means "same as HEAD"
	status  string            // what git status --porcelain prints
	calls   [][]string
}

type fakeCommit struct {
	id      string
	seq     int
	parents []string
	message string
	tree    map[string]string // path -> content
}

// fakeDir is the working directory every fake call must name.
const fakeDir = "/fake/polecat"

// newFakeRepo returns a repository on main holding one commit, "initial
// commit", that adds README.md.
func newFakeRepo(t testing.TB) *fakeRepo {
	r := &fakeRepo{
		t:       t,
		commits: map[string]*fakeCommit{},
		refs:    map[string]string{},
		head:    "refs/heads/main",
	}
	r.commit("initial commit", "README.md", "# Test\n")
	return r
}

func (r *fakeRepo) headCommit() string { return r.refs[r.head] }

// commit records a commit on HEAD with message and files (path, content
// pairs) and returns its full hash.
func (r *fakeRepo) commit(message string, pathContent ...string) string {
	r.t.Helper()
	if len(pathContent)%2 != 0 {
		r.t.Fatalf("commit wants path/content pairs, got %q", pathContent)
	}
	tree := r.treeOf(r.headCommit())
	for i := 0; i < len(pathContent); i += 2 {
		tree[pathContent[i]] = pathContent[i+1]
	}
	return r.record(message, tree, r.headCommit())
}

// remove records a commit on HEAD that deletes path.
func (r *fakeRepo) remove(message, path string) string {
	tree := r.treeOf(r.headCommit())
	delete(tree, path)
	return r.record(message, tree, r.headCommit())
}

// merge records a merge of branch into HEAD.
func (r *fakeRepo) merge(message, branch string) string {
	r.t.Helper()
	other := r.refs["refs/heads/"+branch]
	if other == "" {
		r.t.Fatalf("merge %s: no such branch", branch)
	}
	tree := r.treeOf(other)
	for k, v := range r.treeOf(r.headCommit()) {
		tree[k] = v
	}
	return r.record(message, tree, r.headCommit(), other)
}

func (r *fakeRepo) record(message string, tree map[string]string, parents ...string) string {
	r.seq++
	sum := sha1.Sum([]byte("fake-commit-" + strconv.Itoa(r.seq)))
	c := &fakeCommit{id: hex.EncodeToString(sum[:]), seq: r.seq, message: message, tree: tree}
	for _, p := range parents {
		if p != "" {
			c.parents = append(c.parents, p)
		}
	}
	r.commits[c.id] = c
	r.refs[r.head] = c.id
	r.index = nil
	return c.id
}

func (r *fakeRepo) treeOf(id string) map[string]string {
	tree := map[string]string{}
	if c := r.commits[id]; c != nil {
		for k, v := range c.tree {
			tree[k] = v
		}
	}
	return tree
}

// checkoutNew is git checkout -b name.
func (r *fakeRepo) checkoutNew(name string) {
	r.refs["refs/heads/"+name] = r.headCommit()
	r.head = "refs/heads/" + name
}

// checkout switches HEAD to an existing branch.
func (r *fakeRepo) checkout(name string) {
	r.t.Helper()
	if _, ok := r.refs["refs/heads/"+name]; !ok {
		r.t.Fatalf("checkout %s: no such branch", name)
	}
	r.head = "refs/heads/" + name
}

// subjectsSince returns the subjects of base..HEAD, newest first, the way
// git log --format=%s would list them.
func (r *fakeRepo) subjectsSince(base string) []string {
	r.t.Helper()
	from, ok := r.resolve(base)
	if !ok {
		r.t.Fatalf("resolve %s", base)
	}
	var subjects []string
	for _, c := range r.between(from, r.headCommit()) {
		subjects = append(subjects, subject(c.message))
	}
	return subjects
}

// wrote reports whether the fake received a command that changes the
// repository.
func (r *fakeRepo) wrote() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if c[0] == "reset" || c[0] == "commit" {
			out = append(out, c)
		}
	}
	return out
}

// resolve resolves HEAD, a branch name, a full refname, a hash of at least
// four characters, and a trailing ~N (first-parent steps).
func (r *fakeRepo) resolve(rev string) (string, bool) {
	steps := 0
	if base, n, ok := strings.Cut(rev, "~"); ok {
		k, err := strconv.Atoi(n)
		if err != nil {
			return "", false
		}
		rev, steps = base, k
	}
	var id string
	switch {
	case rev == "HEAD":
		id = r.headCommit()
	case r.refs[rev] != "":
		id = r.refs[rev]
	case r.refs["refs/heads/"+rev] != "":
		id = r.refs["refs/heads/"+rev]
	case len(rev) >= 4:
		for cid := range r.commits {
			if strings.HasPrefix(cid, rev) {
				if id != "" {
					return "", false
				}
				id = cid
			}
		}
	}
	for ; id != "" && steps > 0; steps-- {
		c := r.commits[id]
		if len(c.parents) == 0 {
			return "", false
		}
		id = c.parents[0]
	}
	return id, id != ""
}

func (r *fakeRepo) ancestors(id string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[c] {
			continue
		}
		seen[c] = true
		stack = append(stack, r.commits[c].parents...)
	}
	return seen
}

// between is from..to, newest first.
func (r *fakeRepo) between(from, to string) []*fakeCommit {
	exclude := r.ancestors(from)
	var out []*fakeCommit
	for id := range r.ancestors(to) {
		if !exclude[id] {
			out = append(out, r.commits[id])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}

// mergeBase is the newest common ancestor of a and b.
func (r *fakeRepo) mergeBase(a, b string) string {
	inA := r.ancestors(a)
	var best *fakeCommit
	for id := range r.ancestors(b) {
		if inA[id] && (best == nil || r.commits[id].seq > best.seq) {
			best = r.commits[id]
		}
	}
	if best == nil {
		return ""
	}
	return best.id
}

func subject(message string) string {
	first, _, _ := strings.Cut(message, "\n")
	return first
}

// errFatal stands in for git's exit 128 with a fatal message.
var errFatal = errors.New("exit status 128: fatal: bad revision")

// errUnsupported marks a command the fake does not model.
var errUnsupported = errors.New("fake git: unsupported command")

// run is the gitRunner.
func (r *fakeRepo) run(workDir string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if workDir != fakeDir {
		r.t.Errorf("fake git: ran in %q, want %q", workDir, fakeDir)
	}
	r.calls = append(r.calls, args)
	out, err := r.dispatch(args)
	if errors.Is(err, errUnsupported) {
		r.t.Errorf("fake git: unexpected command: git %s", strings.Join(args, " "))
	}
	return out, err
}

func (r *fakeRepo) dispatch(args []string) (string, error) {
	switch {
	case len(args) == 3 && args[0] == "merge-base":
		a, okA := r.resolve(args[1])
		b, okB := r.resolve(args[2])
		if !okA || !okB {
			return "", errFatal
		}
		mb := r.mergeBase(a, b)
		if mb == "" {
			return "", errors.New("exit status 1")
		}
		return mb + "\n", nil

	case len(args) == 3 && args[0] == "log" && (args[1] == "--format=%s" || args[1] == "--format=%s%x1e"):
		fromRev, toRev, ok := strings.Cut(args[2], "..")
		if !ok {
			return "", errUnsupported
		}
		from, okA := r.resolve(fromRev)
		to, okB := r.resolve(toRev)
		if !okA || !okB {
			return "", errFatal
		}
		var b strings.Builder
		for _, c := range r.between(from, to) {
			b.WriteString(subject(c.message))
			if args[1] == "--format=%s%x1e" {
				b.WriteString("\x1e")
			}
			b.WriteString("\n")
		}
		return b.String(), nil

	case len(args) == 4 && args[0] == "log" && args[1] == "-1" && args[2] == "--format=%p":
		id, ok := r.resolve(args[3])
		if !ok {
			return "", errFatal
		}
		var short []string
		for _, p := range r.commits[id].parents {
			short = append(short, p[:7])
		}
		return strings.Join(short, " ") + "\n", nil

	case len(args) == 3 && args[0] == "reset" && args[1] == "--soft":
		id, ok := r.resolve(args[2])
		if !ok {
			return "", errFatal
		}
		if r.index == nil {
			r.index = r.treeOf(r.headCommit())
		}
		r.refs[r.head] = id
		return "", nil

	case len(args) == 3 && args[0] == "commit" && args[1] == "-m":
		tree := r.index
		if tree == nil {
			return "", errors.New("exit status 1: nothing to commit")
		}
		r.record(args[2], tree, r.headCommit())
		return "", nil

	case len(args) == 7 && args[0] == "diff" &&
		strings.Join(args[1:5], " ") == "--name-only --no-renames --diff-filter=A -z":
		base, okA := r.resolve(args[5])
		head, okB := r.resolve(args[6])
		if !okA || !okB {
			return "", errFatal
		}
		baseTree := r.commits[base].tree
		var added []string
		for p := range r.commits[head].tree {
			if _, ok := baseTree[p]; !ok {
				added = append(added, p)
			}
		}
		sort.Strings(added)
		var b strings.Builder
		for _, p := range added {
			b.WriteString(p)
			b.WriteByte(0)
		}
		return b.String(), nil

	case len(args) == 2 && args[0] == "status" && args[1] == "--porcelain":
		return r.status, nil

	case len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD":
		return r.headCommit() + "\n", nil

	case len(args) == 3 && args[0] == "rev-parse" && args[1] == "--abbrev-ref" && args[2] == "HEAD":
		return strings.TrimPrefix(r.head, "refs/heads/") + "\n", nil
	}
	return "", fmt.Errorf("%w: %v", errUnsupported, args)
}
