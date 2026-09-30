package gitfake

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// InitRepo makes a repository with an unborn branch at the handle's
// directory, which must exist; on an existing repository it does nothing,
// as git init does.
func (h *handle) InitRepo(branch string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"init", "-b", branch}
	if _, _, err := h.locate(args...); err == nil {
		return nil
	}
	if info, err := os.Stat(h.dir); err != nil || !info.IsDir() {
		return gitErr(128, "fatal: cannot change to '"+h.dir+"': No such file or directory", args...)
	}
	if err := os.MkdirAll(filepath.Join(h.dir, ".git"), 0o755); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	r := &repo{path: h.dir, refs: map[string]string{}, head: "refs/heads/" + branch,
		remotes: map[string]string{}, has: map[string]bool{}, worktrees: map[string]*worktree{}}
	r.main = &worktree{path: h.dir}
	h.f.repos[h.dir] = r
	return nil
}

// ConfigSet sets a repository config value.
func (h *handle) ConfigSet(key, value string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("config", key, value)
	if err != nil {
		return err
	}
	if h.f.wt.config == nil {
		h.f.wt.config = map[*repo]map[string]string{}
	}
	if h.f.wt.config[r] == nil {
		h.f.wt.config[r] = map[string]string{}
	}
	h.f.wt.config[r][key] = value
	return nil
}

// ConfigGet returns a config value ConfigSet set, or "" when it is unset or
// the directory is no repository, as git.Git.ConfigGet reports both.
func (h *handle) ConfigGet(key string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("config", "--get", key)
	if err != nil {
		return "", nil
	}
	return h.f.wt.config[r][key], nil
}

// CommitWithAuthor is Commit; the fake records no authors.
func (h *handle) CommitWithAuthor(message, _ string) error {
	return h.Commit(message)
}

// PackSize is "0": the fake keeps no packs, and a repository git has not
// packed reports 0 too.
func (h *handle) PackSize() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if _, _, err := h.locate("count-objects", "-v"); err != nil {
		return "", err
	}
	return "0", nil
}

// LogAll lists up to max commits reachable from any ref or HEAD, newest
// first (the order they were made, which is an ancestry order).
func (h *handle) LogAll(max int) ([]git.LogEntry, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("log", "--all", "--topo-order")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var commits []*commit
	tips := []string{headCommit(r, wt)}
	for _, id := range r.refs {
		tips = append(tips, id)
	}
	for _, tip := range tips {
		for _, id := range h.f.ancestors(tip) {
			if !seen[id] {
				seen[id] = true
				commits = append(commits, h.f.objects[id])
			}
		}
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].seq > commits[j].seq })
	if len(commits) > max {
		commits = commits[:max]
	}
	entries := make([]git.LogEntry, 0, len(commits))
	for _, c := range commits {
		entries = append(entries, git.LogEntry{Hash: c.id, Time: commitTime(c), Subject: firstLine(c.message)})
	}
	return entries, nil
}

// commitTime is a commit's committer time: commitEpoch plus its number, in
// seconds.
func commitTime(c *commit) time.Time {
	return time.Unix(int64(commitEpoch+c.seq), 0).UTC()
}

// firstLine is a commit message's subject.
func firstLine(message string) string {
	subject, _, _ := strings.Cut(message, "\n")
	return subject
}
