package gitfake

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// On disk the fake keeps the shape consumers check with os.Stat: a bare
// repository is a directory, a non-bare one has a .git directory, and a
// linked worktree has a .git file naming its own directory under the
// repository's worktrees/. Objects, refs and config stay in memory.

// gitDirOf is r's git directory: the repository itself when bare, else its
// .git.
func gitDirOf(r *repo) string {
	if r.bare {
		return r.path
	}
	return filepath.Join(r.path, ".git")
}

// worktreeGitDir is the private git directory of the linked worktree at path.
func worktreeGitDir(r *repo, path string) string {
	return filepath.Join(gitDirOf(r), "worktrees", filepath.Base(path))
}

func layoutRepo(r *repo) error {
	return os.MkdirAll(gitDirOf(r), 0o755)
}

func layoutWorktree(r *repo, path string) error {
	dir := worktreeGitDir(r, path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: "+dir+"\n"), 0o644) //nolint:gosec // G306: git writes .git world-readable
}

// at finds the repository or linked worktree whose path is exactly dir.
// Callers hold f.mu.
func (f *Fake) at(dir string) (*repo, *worktree) {
	if r := f.repos[dir]; r != nil {
		return r, r.main
	}
	for _, r := range f.repos {
		if wt := r.worktrees[dir]; wt != nil {
			return r, wt
		}
	}
	return nil, nil
}

// repoAt is the repository dir is, or is a worktree or subdirectory of.
// Callers hold f.mu.
func (f *Fake) repoAt(dir string) *repo {
	for d := clean(dir); ; d = filepath.Dir(d) {
		if r, _ := f.at(d); r != nil {
			return r
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// onDisk reports whether git would find the repository or checkout: a bare
// repository's directory, or a checkout's .git (git discovers a repository
// through it, so a checkout whose .git is gone is no repository at all).
func onDisk(r *repo, wt *worktree) bool {
	path := r.path
	if wt != nil {
		path = filepath.Join(wt.path, ".git")
	}
	_, err := os.Stat(path)
	return err == nil
}

// InitRepo makes an empty non-bare repository at dir with HEAD on main.
func (f *Fake) InitRepo(t testing.TB, dir string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = clean(dir)
	r := &repo{path: dir, refs: map[string]string{}, head: "refs/heads/main",
		remotes: map[string]string{}, has: map[string]bool{}, worktrees: map[string]*worktree{}}
	r.main = &worktree{path: dir}
	f.repos[dir] = r
	if err := layoutRepo(r); err != nil {
		t.Fatalf("gitfake: InitRepo: %v", err)
	}
}

// AddRemote configures remote name in the repository at dir to point at
// url, a path in the world.
func (f *Fake) AddRemote(t testing.TB, dir, name, url string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repoAt(dir)
	if r == nil {
		t.Fatalf("gitfake: AddRemote: no repository at %s", dir)
	}
	r.remotes[name] = url
}

// OpenDir is Open for git.NewGitWithDir(gitDir, workDir): the worktree at
// workDir when there is one, else the repository at gitDir.
func (f *Fake) OpenDir(gitDir, workDir string) WorktreeRepo {
	if workDir != "" {
		return &handle{f: f, dir: clean(workDir)}
	}
	return &handle{f: f, dir: clean(gitDir)}
}

// OpenWorktreeRepo is Open with the wider WorktreeRepo surface.
func (f *Fake) OpenWorktreeRepo(dir string) WorktreeRepo {
	return &handle{f: f, dir: clean(dir)}
}

// CommitWorktree commits the checkout at dir as it is on disk (what git add
// -A; git commit -m message would record, less ignored files) onto its HEAD:
// the branch it is on, or a detached HEAD. It returns the commit's id.
func (f *Fake) CommitWorktree(t testing.TB, dir, message string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &handle{f: f, dir: clean(dir)}
	r, wt, err := h.workTree("commit")
	if err != nil {
		t.Fatalf("gitfake: CommitWorktree: %v", err)
	}
	tree, err := readWorktree(wt.path)
	if err != nil {
		t.Fatalf("gitfake: CommitWorktree: %v", err)
	}
	var parents []string
	if head := headCommit(r, wt); head != "" {
		parents = []string{head}
	}
	id := f.newCommit(parents, tree, message)
	r.has[id] = true
	setHead(r, wt, id)
	return id
}

// readWorktree reads the files of the checkout at dir that git would track:
// everything but .git and what the checkout's .gitignore ignores.
func readWorktree(dir string) (map[string]string, error) {
	ignore := readIgnore(dir)
	tree := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || ignore.matches(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = string(data)
		return nil
	})
	return tree, err
}

// ignoreRules is the checkout's top-level .gitignore plus its repository's
// info/exclude: plain names or globs, matched against a path's name or whole
// path, a trailing / for directories only. Negation and nested .gitignore
// files are not modeled.
type ignoreRules []string

func readIgnore(dir string) ignoreRules {
	rules := parseIgnore(filepath.Join(dir, ".gitignore"))
	if common := commonDirOf(dir); common != "" {
		rules = append(rules, parseIgnore(filepath.Join(common, "info", "exclude"))...)
	}
	return rules
}

// commonDirOf finds the git directory the checkout at dir shares with its
// repository's other worktrees, the way git does: a .git directory, or the
// directory a linked worktree's .git file names, less its worktrees/<name>.
func commonDirOf(dir string) string {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return dotGit
	}
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	return filepath.Dir(filepath.Dir(gitdir))
}

// ExcludePath is the info/exclude file git reads for the checkout at dir,
// in its repository's common git directory.
func (f *Fake) ExcludePath(dir string) (string, error) {
	common := commonDirOf(dir)
	if common == "" {
		return "", fmt.Errorf("gitfake: no checkout at %s", dir)
	}
	return filepath.Join(common, "info", "exclude"), nil
}

func parseIgnore(path string) ignoreRules {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rules ignoreRules
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		rules = append(rules, line)
	}
	return rules
}

func (rules ignoreRules) matches(rel string, isDir bool) bool {
	for _, rule := range rules {
		pattern, dirOnly := strings.CutSuffix(rule, "/")
		if dirOnly && !isDir {
			continue
		}
		anchored := strings.HasPrefix(pattern, "/")
		pattern = strings.TrimPrefix(pattern, "/")
		if ok, _ := filepath.Match(pattern, rel); ok {
			return true
		}
		if !anchored && !strings.Contains(pattern, "/") {
			if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
				return true
			}
		}
	}
	return false
}

func (h *handle) TopLevel() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	_, wt, err := h.workTree("rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return wt.path, nil
}

func (h *handle) GitDir() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if wt != nil && wt != r.main {
		return worktreeGitDir(r, wt.path), nil
	}
	return gitDirOf(r), nil
}

// abs resolves path against the handle's directory, as git resolves a path
// argument against its working directory.
func (h *handle) abs(path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(h.dir, path)
	}
	return clean(path)
}

// checkedOutAt returns the worktree path that has branch checked out, or "".
func checkedOutAt(r *repo, branch string) string {
	ref := "refs/heads/" + branch
	if r.main != nil && r.head == ref {
		return r.main.path
	}
	for p, wt := range r.worktrees {
		if wt.head == ref {
			return p
		}
	}
	return ""
}

// addWorktree registers and checks out a linked worktree at path with HEAD
// head (a ref name or a commit id) showing tree.
func (h *handle) addWorktree(r *repo, path, head string, tree map[string]string, args ...string) error {
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		return gitErr(128, fmt.Sprintf("fatal: '%s' already exists", path), args...)
	}
	if err := materialize(path, tree); err != nil {
		return gitErr(128, "fatal: could not create work tree dir '"+path+"': "+err.Error(), args...)
	}
	r.worktrees[path] = &worktree{path: path, head: head}
	if err := layoutWorktree(r, path); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	return nil
}

func (h *handle) WorktreeAddFromRef(path, branch, startPoint string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"worktree", "add", "-b", branch, path, startPoint}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, startPoint)
	if !ok {
		return gitErr(128, "fatal: invalid reference: "+startPoint, args...)
	}
	if _, exists := r.refs["refs/heads/"+branch]; exists {
		return gitErr(255, fmt.Sprintf("fatal: a branch named '%s' already exists", branch), args...)
	}
	if err := h.addWorktree(r, h.abs(path), "refs/heads/"+branch, h.f.treeOf(id), args...); err != nil {
		return err
	}
	r.refs["refs/heads/"+branch] = id
	return nil
}

func (h *handle) WorktreeAddExistingForce(path, branch string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"worktree", "add", "--force", path, branch}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	id, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return gitErr(128, "fatal: invalid reference: "+branch, args...)
	}
	return h.addWorktree(r, h.abs(path), "refs/heads/"+branch, h.f.treeOf(id), args...)
}

func (h *handle) WorktreeMove(oldPath, newPath string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"worktree", "move", oldPath, newPath}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	from, to := h.abs(oldPath), h.abs(newPath)
	wt := r.worktrees[from]
	if wt == nil {
		return gitErr(128, fmt.Sprintf("fatal: '%s' is not a working tree", oldPath), args...)
	}
	if _, err := os.Stat(to); err == nil {
		return gitErr(128, fmt.Sprintf("fatal: '%s' already exists", newPath), args...)
	}
	if err := os.Rename(from, to); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	_ = os.RemoveAll(worktreeGitDir(r, from))
	delete(r.worktrees, from)
	wt.path = to
	r.worktrees[to] = wt
	if err := layoutWorktree(r, to); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	return nil
}

func (h *handle) WorktreeList() ([]git.Worktree, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	entry := func(path, head string) git.Worktree {
		w := git.Worktree{Path: path}
		if strings.HasPrefix(head, "refs/") {
			w.Branch = strings.TrimPrefix(head, "refs/heads/")
			w.Commit = r.refs[head]
		} else {
			w.Commit = head
		}
		return w
	}
	var list []git.Worktree
	if r.main != nil {
		list = append(list, entry(r.path, r.head))
	} else {
		// A bare repository lists itself first, with no branch or commit.
		list = append(list, git.Worktree{Path: r.path})
	}
	paths := make([]string, 0, len(r.worktrees))
	for p := range r.worktrees {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		list = append(list, entry(p, r.worktrees[p].head))
	}
	return list, nil
}

// DeleteRef removes ref (a full ref name) from the repository at dir, as git
// push origin --delete or git update-ref -d does.
func (f *Fake) DeleteRef(t testing.TB, dir, ref string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repoAt(dir)
	if r == nil {
		t.Fatalf("gitfake: DeleteRef: no repository at %s", dir)
	}
	delete(r.refs, ref)
}

// RemoveRepo takes the repository at dir out of the world and off disk, so
// every remote call to it fails the way git fails on a missing repository.
func (f *Fake) RemoveRepo(t testing.TB, dir string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = clean(dir)
	delete(f.repos, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("gitfake: RemoveRepo: %v", err)
	}
}

// RemoveRemote drops remote name from the repository at dir, as git remote
// remove does; its remote-tracking refs go with it.
func (f *Fake) RemoveRemote(t testing.TB, dir, name string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repoAt(dir)
	if r == nil {
		t.Fatalf("gitfake: RemoveRemote: no repository at %s", dir)
	}
	delete(r.remotes, name)
	for ref := range r.refs {
		if strings.HasPrefix(ref, "refs/remotes/"+name+"/") {
			delete(r.refs, ref)
		}
	}
	delete(r.configMap(), remoteHeadKey(name))
}
