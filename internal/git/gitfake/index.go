package gitfake

import (
	"crypto/sha1" //nolint:gosec // G505: git's own object ids
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// worktreeState is the staging model: each checkout's index and the trees
// WriteTree wrote. Its zero value is empty; callers hold f.mu.
//
// A checkout's working tree is the files on disk under its path. Its index
// is a staged tree recorded against the HEAD commit it was staged on; once
// HEAD moves by anything but Commit (a merge, a worktree reset), the index
// reads as HEAD's tree again, as git's own merge and checkout rewrite it.
// Known limits: file modes, symlinks and submodules are not modeled, every
// path is a regular file, and .gitignore supports blank lines, comments,
// negation, a leading or inner "/" (anchored), a trailing "/" (directories
// only) and path.Match globs, but not "**".
type worktreeState struct {
	indexes map[*worktree]*index
	trees   map[string]map[string]string // tree id to its tree
}

type index struct {
	base string // the HEAD commit the tree was staged against
	tree map[string]string
}

// stage returns wt's index tree for reading and writing.
func (f *Fake) stage(r *repo, wt *worktree) *index {
	head := headCommit(r, wt)
	if f.wt.indexes == nil {
		f.wt.indexes = map[*worktree]*index{}
	}
	ix := f.wt.indexes[wt]
	if ix == nil || ix.base != head {
		ix = &index{base: head, tree: copyTree(f.treeOf(head))}
		f.wt.indexes[wt] = ix
	}
	return ix
}

// blobID is git's id for a blob holding content.
func blobID(content string) string {
	h := sha1.New() //nolint:gosec // G401: git's object id
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

// treeID names a tree the way the fake names commits: unique to its content,
// not git's hash of it.
func treeID(tree map[string]string) string {
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha1.New() //nolint:gosec // G401: ids, not security
	h.Write([]byte("tree"))
	for _, p := range paths {
		fmt.Fprintf(h, "\x00%s\x00%s", p, blobID(tree[p]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readWorkTree reads every file under dir but .git, keyed by slash path.
func readWorkTree(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: a checkout's own files
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return files, err
}

// ignoreRule is one .gitignore line, read from the file in dir.
type ignoreRule struct {
	dir      string // "" for the checkout's root
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
}

// ignoreRules reads the .gitignore files among a checkout's files, outermost
// first so a deeper file's rules are checked after (and win over) its
// parents'.
func ignoreRules(files map[string]string) []ignoreRule {
	var dirs []string
	for p := range files {
		if path.Base(p) == ".gitignore" {
			dirs = append(dirs, path.Dir(p))
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/")
		if dirs[i] == "." {
			di = -1
		}
		if dirs[j] == "." {
			dj = -1
		}
		if di != dj {
			return di < dj
		}
		return dirs[i] < dirs[j]
	})
	var rules []ignoreRule
	for _, d := range dirs {
		file := ".gitignore"
		if d != "." {
			file = d + "/.gitignore"
		} else {
			d = ""
		}
		for _, line := range strings.Split(files[file], "\n") {
			line = strings.TrimRight(line, " \r")
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			rule := ignoreRule{dir: d}
			if strings.HasPrefix(line, "!") {
				rule.negate, line = true, line[1:]
			}
			if strings.HasSuffix(line, "/") {
				rule.dirOnly, line = true, strings.TrimSuffix(line, "/")
			}
			if strings.Contains(line, "/") {
				rule.anchored, line = true, strings.TrimPrefix(line, "/")
			}
			rule.pattern = line
			rules = append(rules, rule)
		}
	}
	return rules
}

// matches reports whether rule matches the path p (a directory when isDir).
func (rule ignoreRule) matches(p string, isDir bool) bool {
	if rule.dirOnly && !isDir {
		return false
	}
	rel := p
	if rule.dir != "" {
		var ok bool
		if rel, ok = strings.CutPrefix(p, rule.dir+"/"); !ok {
			return false
		}
	}
	subject := rel
	if !rule.anchored {
		subject = path.Base(rel)
	}
	ok, _ := path.Match(rule.pattern, subject)
	return ok
}

// isIgnored reports whether the untracked file p is ignored: a directory on
// its path is ignored (which nothing inside can undo), or the last rule
// matching the file itself ignores it.
func isIgnored(rules []ignoreRule, p string) bool {
	parts := strings.Split(p, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		isDir := i < len(parts)
		ignored := false
		for _, rule := range rules {
			if rule.matches(prefix, isDir) {
				ignored = !rule.negate
			}
		}
		if ignored {
			return true
		}
	}
	return false
}

// underSpec reports whether p is named by pathspec spec: the path itself, a
// directory holding it, or "." (everything).
func underSpec(p, spec string) bool {
	spec = strings.TrimSuffix(filepath.ToSlash(spec), "/")
	return spec == "." || spec == "" || p == spec || strings.HasPrefix(p, spec+"/")
}

// Status reports the checkout's changes as git status --porcelain -uall
// does: staged against HEAD, unstaged against the index, and every untracked
// file that is not ignored.
func (h *handle) Status() (*git.GitStatus, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("status", "--porcelain", "-uall")
	if err != nil {
		return nil, err
	}
	head := h.f.treeOf(headCommit(r, wt))
	ix := h.f.stage(r, wt).tree
	disk, err := readWorkTree(wt.path)
	if err != nil {
		return nil, gitErr(128, "fatal: "+err.Error(), "status")
	}
	rules := ignoreRules(disk)
	unmerged := map[string]bool{}
	if wt.merge != nil {
		for _, p := range wt.merge.conflicts {
			unmerged[p] = true
		}
	}
	paths := map[string]bool{}
	for _, t := range []map[string]string{head, ix, disk} {
		for p := range t {
			paths[p] = true
		}
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	st := &git.GitStatus{Clean: true}
	for _, p := range sorted {
		hc, inHead := head[p]
		ic, inIndex := ix[p]
		dc, onDisk := disk[p]
		if unmerged[p] {
			st.Unmerged = append(st.Unmerged, p)
			continue
		}
		if !inIndex && !inHead {
			if onDisk && !isIgnored(rules, p) {
				st.Untracked = append(st.Untracked, p)
			}
			continue
		}
		x := byte(' ')
		switch {
		case inIndex && !inHead:
			x = 'A'
		case !inIndex && inHead:
			x = 'D'
		case ic != hc:
			x = 'M'
		}
		y := byte(' ')
		switch {
		case inIndex && !onDisk:
			y = 'D'
		case inIndex && dc != ic:
			y = 'M'
		}
		if !inIndex && onDisk {
			// Staged as deleted, yet on disk: git reports the deletion and the
			// file as untracked.
			st.Untracked = append(st.Untracked, p)
		}
		code := string([]byte{x, y})
		switch {
		case code == "  ":
			continue
		case strings.Contains(code, "M"):
			st.Modified = append(st.Modified, p)
		case strings.Contains(code, "A"):
			st.Added = append(st.Added, p)
		case strings.Contains(code, "D"):
			st.Deleted = append(st.Deleted, p)
		}
		if x != ' ' && y == ' ' {
			st.StagedOnly = append(st.StagedOnly, p)
		}
	}
	st.Clean = len(st.Modified) == 0 && len(st.Added) == 0 && len(st.Deleted) == 0 &&
		len(st.Untracked) == 0 && len(st.Unmerged) == 0
	return st, nil
}

// Add stages the files pathspecs name from disk, deletions included, as git
// add does. -A and --all with no pathspec, or ".", stage the whole checkout.
// An untracked file .gitignore ignores is staged only when named exactly,
// which git refuses.
func (h *handle) Add(pathspecs ...string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := append([]string{"add"}, pathspecs...)
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	var specs []string
	all := false
	for _, s := range pathspecs {
		switch s {
		case "-A", "--all":
			all = true
		default:
			specs = append(specs, s)
		}
	}
	if len(specs) == 0 {
		if !all {
			return nil // git prints "Nothing specified, nothing added." and succeeds
		}
		specs = []string{"."}
	}
	ix := h.f.stage(r, wt).tree
	disk, err := readWorkTree(wt.path)
	if err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	rules := ignoreRules(disk)
	for _, spec := range specs {
		matched := false
		for p, content := range disk {
			if !underSpec(p, spec) {
				continue
			}
			if _, tracked := ix[p]; !tracked && isIgnored(rules, p) {
				if p == filepath.ToSlash(spec) {
					return gitErr(1, "The following paths are ignored by one of your .gitignore files:\n"+p, args...)
				}
				continue
			}
			ix[p] = content
			matched = true
		}
		for p := range ix {
			if !underSpec(p, spec) {
				continue
			}
			matched = true
			if _, ok := disk[p]; !ok {
				delete(ix, p)
			}
		}
		if !matched {
			return gitErr(128, fmt.Sprintf("fatal: pathspec '%s' did not match any files", spec), args...)
		}
	}
	return nil
}

// ResetFiles puts the index entries pathspecs name back to HEAD's (git
// reset HEAD -- <paths>), leaving the files on disk as they are.
func (h *handle) ResetFiles(pathspecs ...string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := append([]string{"reset", "HEAD", "--"}, pathspecs...)
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	headID := headCommit(r, wt)
	if headID == "" {
		return gitErr(128, "fatal: ambiguous argument 'HEAD': unknown revision or path not in the working tree.", args...)
	}
	head := h.f.treeOf(headID)
	ix := h.f.stage(r, wt).tree
	for _, spec := range pathspecs {
		for p := range ix {
			if _, inHead := head[p]; underSpec(p, spec) && !inHead {
				delete(ix, p)
			}
		}
		for p, c := range head {
			if underSpec(p, spec) {
				ix[p] = c
			}
		}
	}
	return nil
}

// StagedChanges lists the index's differences from HEAD, by path.
func (h *handle) StagedChanges() ([]git.StagedChange, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("diff", "--cached", "--name-status", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}
	head := h.f.treeOf(headCommit(r, wt))
	ix := h.f.stage(r, wt).tree
	var changes []git.StagedChange
	for _, p := range changedPaths(head, ix) {
		hc, inHead := head[p]
		ic, inIndex := ix[p]
		switch {
		case !inHead:
			changes = append(changes, git.StagedChange{Status: 'A', Path: p})
		case !inIndex:
			changes = append(changes, git.StagedChange{Status: 'D', Path: p})
		case hc != ic:
			changes = append(changes, git.StagedChange{Status: 'M', Path: p})
		}
	}
	return changes, nil
}

// WriteTree records the index as a tree and returns its id, which
// TreeFileBlobs reads.
func (h *handle) WriteTree() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("write-tree")
	if err != nil {
		return "", err
	}
	tree := copyTree(h.f.stage(r, wt).tree)
	id := treeID(tree)
	if h.f.wt.trees == nil {
		h.f.wt.trees = map[string]map[string]string{}
	}
	h.f.wt.trees[id] = tree
	return id, nil
}

// Commit commits the index on HEAD. An index equal to HEAD's tree is
// nothing to commit, which git exits 1 for.
func (h *handle) Commit(message string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"commit", "-m", message}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	if wt.merge != nil {
		return gitErr(128, "error: Committing is not possible because you have unmerged files.", args...)
	}
	headID := headCommit(r, wt)
	ix := h.f.stage(r, wt)
	if headID != "" && sameTree(ix.tree, h.f.treeOf(headID)) {
		return &git.GitError{Command: "commit", Args: args, Stdout: "nothing to commit, working tree clean", Err: exitError(1)}
	}
	var parents []string
	if headID != "" {
		parents = []string{headID}
	}
	id := h.f.newCommit(parents, ix.tree, message)
	r.has[id] = true
	setHead(r, wt, id)
	ix.base = id
	return nil
}

// TreeFileBlobs maps every file of rev's tree (a commit, or a tree
// WriteTree wrote) to its blob id.
func (h *handle) TreeFileBlobs(rev string) (map[string]string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"ls-tree", "-r", "-z", "--full-tree", rev}
	tree, err := h.treeAt(rev, args...)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]string, len(tree))
	for p, c := range tree {
		blobs[p] = blobID(c)
	}
	return blobs, nil
}

// treeAt resolves rev to a tree: a tree WriteTree wrote, or a commit's.
func (h *handle) treeAt(rev string, args ...string) (map[string]string, error) {
	r, wt, err := h.locate(args...)
	if err != nil {
		return nil, err
	}
	if tree, ok := h.f.wt.trees[rev]; ok {
		return tree, nil
	}
	id, ok := h.resolve(r, wt, rev)
	if !ok || h.f.objects[id] == nil {
		return nil, gitErr(128, "fatal: Not a valid object name "+rev, args...)
	}
	return h.f.treeOf(id), nil
}

// blobContent finds the content whose blob id is sha among every tree and
// index the world holds.
func (h *handle) blobContent(sha string) (string, bool) {
	for _, c := range h.f.objects {
		for _, content := range c.tree {
			if blobID(content) == sha {
				return content, true
			}
		}
	}
	for _, t := range h.f.wt.trees {
		for _, content := range t {
			if blobID(content) == sha {
				return content, true
			}
		}
	}
	for _, ix := range h.f.wt.indexes {
		for _, content := range ix.tree {
			if blobID(content) == sha {
				return content, true
			}
		}
	}
	return "", false
}

// BlobContent returns a blob's text, trimmed the way git.Git trims output.
func (h *handle) BlobContent(sha string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"cat-file", "blob", sha}
	if _, _, err := h.locate(args...); err != nil {
		return "", err
	}
	content, ok := h.blobContent(sha)
	if !ok {
		return "", gitErr(128, "fatal: Not a valid object name "+sha, args...)
	}
	return strings.TrimSpace(content), nil
}

// BlobDiffLines counts the lines a line diff of two blobs adds and removes,
// keyed by line text. Either blob "" is no lines at all.
func (h *handle) BlobDiffLines(oldBlob, newBlob string) (added, removed map[string]int, err error) {
	empty := map[string]int{}
	if oldBlob == "" || newBlob == "" {
		return empty, empty, nil
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"diff", "--unified=0", "--no-color", oldBlob, newBlob}
	if _, _, err := h.locate(args...); err != nil {
		return nil, nil, err
	}
	a, okA := h.blobContent(oldBlob)
	b, okB := h.blobContent(newBlob)
	if !okA || !okB {
		return nil, nil, gitErr(128, "fatal: bad revision", args...)
	}
	rm, add := lineDiff(a, b)
	added, removed = map[string]int{}, map[string]int{}
	for _, l := range add {
		added[strings.TrimSuffix(l, "\n")]++
	}
	for _, l := range rm {
		removed[strings.TrimSuffix(l, "\n")]++
	}
	return added, removed, nil
}

// CommitFileChanges reports, for the limit newest commits reachable from
// rev, each file a commit changed against its parent. Root commits and
// merges report nothing, as git log --raw prints no diff for them.
func (h *handle) CommitFileChanges(rev string, limit int) ([]git.CommitFileChange, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"log", "--raw", "--no-abbrev", "--no-renames", "-z", "--format=C %H %P", rev, "-n", fmt.Sprint(limit)}
	r, wt, err := h.locate(args...)
	if err != nil {
		return nil, err
	}
	id, ok := h.resolve(r, wt, rev)
	if !ok {
		return nil, unknownRevision(rev, args...)
	}
	commits := h.f.rangeCommits("", id)
	if len(commits) > limit {
		commits = commits[:limit]
	}
	var changes []git.CommitFileChange
	for _, c := range commits {
		if len(c.parents) != 1 {
			continue
		}
		parent := h.f.treeOf(c.parents[0])
		for _, p := range changedPaths(parent, c.tree) {
			ch := git.CommitFileChange{Commit: c.id, Path: p}
			if content, ok := parent[p]; ok {
				ch.OldBlob = blobID(content)
			}
			if content, ok := c.tree[p]; ok {
				ch.NewBlob = blobID(content)
			}
			changes = append(changes, ch)
		}
	}
	return changes, nil
}

// MergeBase returns a best common ancestor of a and b; git exits 1 when
// there is none.
func (h *handle) MergeBase(a, b string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"merge-base", a, b}
	r, wt, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	ida, okA := h.resolve(r, wt, a)
	idb, okB := h.resolve(r, wt, b)
	if !okA || !okB {
		bad := a
		if okA {
			bad = b
		}
		return "", gitErr(128, "fatal: Not a valid object name "+bad, args...)
	}
	base := h.f.mergeBase(ida, idb)
	if base == "" {
		return "", gitErr(1, "", args...)
	}
	return base, nil
}

// CurrentBranch is HEAD's branch name, or "HEAD" when detached.
func (h *handle) CurrentBranch() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"rev-parse", "--abbrev-ref", "HEAD"}
	r, wt, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	head := headOf(r, wt)
	if branch, ok := strings.CutPrefix(head, "refs/heads/"); ok {
		if _, born := r.refs[head]; !born {
			return "", gitErr(128, "fatal: ambiguous argument 'HEAD': unknown revision or path not in the working tree.", args...)
		}
		return branch, nil
	}
	return "HEAD", nil
}

// GetUpstreamURL is the upstream remote's URL, or "" when there is none.
func (h *handle) GetUpstreamURL() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("remote", "get-url", "upstream")
	if err != nil {
		return "", err
	}
	return r.remotes["upstream"], nil
}

// CleanDefaultBranchBaseRef is upstream/<branch> in a fork-backed checkout
// (an upstream remote whose URL differs from remote's), else
// <remote>/<branch>.
func (h *handle) CleanDefaultBranchBaseRef(remote, defaultBranch string) string {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	fetchURL, fetchErr := h.RemoteURL(remote)
	upstreamURL, upstreamErr := h.GetUpstreamURL()
	if fetchErr == nil && upstreamErr == nil && upstreamURL != "" && !sameURL(fetchURL, upstreamURL) {
		return "upstream/" + defaultBranch
	}
	return remote + "/" + defaultBranch
}

// sameURL compares two remote URLs the way the fake names repositories: by
// cleaned path, a trailing .git ignored.
func sameURL(a, b string) bool {
	norm := func(u string) string { return strings.TrimSuffix(clean(u), ".git") }
	return norm(a) == norm(b)
}
