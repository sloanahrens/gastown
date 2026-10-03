package git

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// File-read fast paths for the cheapest, most-repeated questions: the current
// branch, whether any stash exists, and whether a directory is a worktree.
//
// Each question is one `git` child process, and a status consumer that asks
// about every polecat in the town asks it dozens of times per call (gt polecat
// list --all spawned 290 git children, a third of them for these three
// answers). The answers are also plain files in .git, so they are read
// directly. Every fast path is conservative: it answers only when the layout
// is the ordinary one and every file it reads is where git puts it, and it
// reports "not sure" otherwise, so the caller falls back to the subprocess and
// gets git's own answer, including git's own errors (an unborn branch, an
// unreadable repository).

// plain reports whether g runs the real git against its working directory with
// nothing redirecting it. An injected runner, an explicit git dir (bare
// repositories), or added environment (GIT_DIR, GIT_WORK_TREE) could each make
// the files under workDir the wrong place to look.
func (g *Git) plain() bool {
	return g.exec == nil && g.gitDir == "" && len(g.env) == 0 && g.workDir != ""
}

// worktreeGitDirs resolves the per-worktree git directory and the common
// directory shared by all worktrees of the repository at dir, by reading dir's
// .git. ok is false for anything but a well-formed checkout or linked
// worktree.
func worktreeGitDirs(dir string) (gitDir, commonDir string, ok bool) {
	dotGit := filepath.Join(dir, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return "", "", false
	}
	if info.IsDir() {
		return dotGit, dotGit, true
	}
	// A linked worktree: ".git" is a file reading "gitdir: <path>".
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return "", "", false
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) || strings.ContainsAny(line, "\n\r") {
		return "", "", false
	}
	gitDir = strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	if st, err := os.Stat(gitDir); err != nil || !st.IsDir() {
		return "", "", false
	}
	// commondir is relative to the worktree's git dir ("../.." in practice).
	commonDir = gitDir
	if cb, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(cb))
		if c == "" {
			return "", "", false
		}
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		commonDir = filepath.Clean(c)
	}
	return gitDir, commonDir, true
}

// IsWorktreeRootFast reports whether dir is itself a git worktree root, read
// from its .git. known is false when the answer needs git: a directory with no
// .git of its own may still sit inside a repository, and git decides that.
func IsWorktreeRootFast(dir string) (isRoot, known bool) {
	if _, _, ok := worktreeGitDirs(dir); ok {
		return true, true
	}
	return false, false
}

// refExists reports whether the ref name is a loose file or a packed-refs entry.
func refExists(commonDir, name string) bool {
	if _, err := os.Stat(filepath.Join(commonDir, filepath.FromSlash(name))); err == nil {
		return true
	}
	packed, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		return false
	}
	suffix := []byte(" " + name + "\n")
	return bytes.Contains(append(packed, '\n'), suffix)
}

// currentBranchFast is `git rev-parse --abbrev-ref HEAD` read from HEAD. It
// answers only for a symbolic HEAD on a branch whose ref exists and whose name
// no tag shadows (git would print "heads/<name>" for an ambiguous one), and for
// a detached HEAD, where git prints "HEAD". An unborn branch is left to git,
// which reports an error for it.
func (g *Git) currentBranchFast() (string, bool) {
	if !g.plain() {
		return "", false
	}
	gitDir, commonDir, ok := worktreeGitDirs(g.workDir)
	if !ok {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	head := strings.TrimSpace(string(b))
	const refPrefix = "ref: refs/heads/"
	if strings.HasPrefix(head, refPrefix) {
		name := strings.TrimPrefix(head, refPrefix)
		if name == "" || strings.ContainsAny(name, " \t\r\n") {
			return "", false
		}
		if !refExists(commonDir, "refs/heads/"+name) || refExists(commonDir, "refs/tags/"+name) {
			return "", false
		}
		return name, true
	}
	if isHexObjectID(head) {
		return "HEAD", true
	}
	return "", false
}

func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// noStashFast reports that the repository has no stash at all: `git stash
// list` walks the reflog of refs/stash, and with no reflog file there is
// nothing to list. known is false when a stash may exist, and git counts it.
func (g *Git) noStashFast() (none, known bool) {
	if !g.plain() {
		return false, false
	}
	_, commonDir, ok := worktreeGitDirs(g.workDir)
	if !ok {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(commonDir, "logs", "refs", "stash")); os.IsNotExist(err) {
		return true, true
	}
	return false, false
}

// refSHAFast resolves a fully qualified ref (refs/remotes/origin/x) to its
// object id from the loose ref file or packed-refs. known is false for a
// symbolic ref, a reftable repository, or anything not plainly a sha; found is
// false, with known true, when the ref is in neither place, which `git
// rev-parse` reports as an error.
func refSHAFast(commonDir, ref string) (sha string, found, known bool) {
	if !strings.HasPrefix(ref, "refs/") {
		return "", false, false
	}
	if _, err := os.Stat(filepath.Join(commonDir, "reftable")); err == nil {
		return "", false, false
	}
	b, err := os.ReadFile(filepath.Join(commonDir, filepath.FromSlash(ref)))
	switch {
	case err == nil:
		s := strings.TrimSpace(string(b))
		if isHexObjectID(s) {
			return s, true, true
		}
		return "", false, false
	case !os.IsNotExist(err):
		return "", false, false
	}
	packed, err := os.ReadFile(filepath.Join(commonDir, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, true
		}
		return "", false, false
	}
	suffix := " " + ref
	for _, line := range strings.Split(string(packed), "\n") {
		if strings.HasSuffix(line, suffix) {
			if f := strings.Fields(line); len(f) == 2 && isHexObjectID(f[0]) {
				return f[0], true, true
			}
			return "", false, false
		}
	}
	return "", false, true
}

// remoteTrackingTipFast is the tip of a remote-tracking ref, read from files.
// ok is false when git must be asked.
func (g *Git) remoteTrackingTipFast(remote, branch string) (tip string, ok bool) {
	if !g.plain() {
		return "", false
	}
	_, commonDir, found := worktreeGitDirs(g.workDir)
	if !found {
		return "", false
	}
	sha, exists, known := refSHAFast(commonDir, "refs/remotes/"+remote+"/"+branch)
	if !known {
		return "", false
	}
	if !exists {
		return "", true
	}
	return sha, true
}

// headUpstreamFast is `git rev-parse --abbrev-ref --symbolic-full-name @{u}`
// answered from the config files and refs: the HEAD branch's upstream as
// "<remote>/<branch>", or no upstream at all. known is false whenever git must
// decide: an unusual config, an upstream whose tracking ref is missing or
// whose short name is ambiguous, or anything that could add configuration the
// files do not show.
func (g *Git) headUpstreamFast() (upstream string, has, known bool) {
	home, _ := os.UserHomeDir()
	return g.headUpstreamWith(os.Getenv, home)
}

// headUpstreamWith is headUpstreamFast with the environment and home directory
// it reads passed in.
func (g *Git) headUpstreamWith(getenv func(string) string, home string) (upstream string, has, known bool) {
	if !g.plain() {
		return "", false, false
	}
	branch, ok := g.currentBranchFast()
	if !ok {
		return "", false, false
	}
	if branch == "HEAD" {
		return "", false, true // a detached HEAD has no upstream
	}
	if strings.ContainsAny(branch, `"\`) {
		return "", false, false
	}
	gitDir, commonDir, ok := worktreeGitDirs(g.workDir)
	if !ok {
		return "", false, false
	}
	for _, v := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG"} {
		if getenv(v) != "" {
			return "", false, false
		}
	}
	if _, err := os.Stat(filepath.Join(gitDir, "config.worktree")); err == nil {
		return "", false, false
	}
	// Other config files may only stay silent about this branch.
	if home != "" {
		xdg := getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		for _, p := range []string{filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config")} {
			b, err := os.ReadFile(p)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || configMayDefineUpstream(string(b), branch) {
				return "", false, false
			}
		}
	}
	cfgBytes, err := os.ReadFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return "", false, false
	}
	config := string(cfgBytes)
	if configIncludes(config) {
		return "", false, false // an included file can override anything below
	}
	if !configMayDefineUpstream(config, branch) {
		return "", false, true // nothing names this branch: no upstream
	}
	remote, merge, ok := branchSection(config, branch)
	if !ok || remote == "" || remote == "." || !strings.HasPrefix(merge, "refs/heads/") {
		return "", false, false
	}
	name := strings.TrimPrefix(merge, "refs/heads/")
	if !remoteMapsHeads(config, remote) {
		return "", false, false
	}
	if _, found, kn := refSHAFast(commonDir, "refs/remotes/"+remote+"/"+name); !kn || !found {
		return "", false, false
	}
	// --abbrev-ref prints the shortest unambiguous name; a branch or tag named
	// like the short form would make git print a longer one.
	short := remote + "/" + name
	if refExists(commonDir, "refs/heads/"+short) || refExists(commonDir, "refs/tags/"+short) {
		return "", false, false
	}
	return short, true, true
}

// plainConfigValue reads "key = value" and returns the lower-cased key and the
// value, refusing any value git would parse specially (quotes, escapes,
// comments, continuations).
func plainConfigValue(line string) (key, value string, ok bool) {
	k, v, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "#;\"\\") {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(k)), v, true
}

// sectionLines returns the lines of the section whose header is exactly header,
// up to the next header, and whether exactly one such section exists.
func sectionLines(config, header string) (lines []string, ok bool) {
	count := 0
	in := false
	for _, raw := range strings.Split(config, "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "[") {
			in = t == header
			if in {
				count++
			}
			continue
		}
		if in && t != "" && !strings.HasPrefix(t, "#") && !strings.HasPrefix(t, ";") {
			lines = append(lines, t)
		}
	}
	return lines, count == 1
}

// branchSection reads remote and merge from the [branch "name"] section. ok is
// false for anything but exactly one section with one of each.
func branchSection(config, branch string) (remote, merge string, ok bool) {
	lines, one := sectionLines(config, `[branch "`+branch+`"]`)
	if !one {
		return "", "", false
	}
	seen := map[string]int{}
	for _, l := range lines {
		k, v, good := plainConfigValue(l)
		if !good {
			return "", "", false
		}
		seen[k]++
		switch k {
		case "remote":
			remote = v
		case "merge":
			merge = v
		}
	}
	if seen["remote"] != 1 || seen["merge"] != 1 {
		return "", "", false
	}
	return remote, merge, true
}

// remoteMapsHeads reports that the remote has exactly one fetch refspec, the
// default one, so refs/heads/X on it is refs/remotes/<remote>/X here.
func remoteMapsHeads(config, remote string) bool {
	if strings.ContainsAny(remote, `"\`) {
		return false
	}
	lines, one := sectionLines(config, `[remote "`+remote+`"]`)
	if !one {
		return false
	}
	fetches := 0
	for _, l := range lines {
		k, v, good := plainConfigValue(l)
		if !good {
			return false
		}
		if k == "fetch" {
			fetches++
			if v != "+refs/heads/*:refs/remotes/"+remote+"/*" {
				return false
			}
		}
	}
	return fetches == 1
}

// configIncludes reports whether the config includes another file, in either
// the [include] or the [includeIf] form.
func configIncludes(config string) bool {
	return strings.Contains(strings.ToLower(config), "[include")
}

// configMayDefineUpstream is a deliberately over-eager reading of a git config:
// true when the text includes another file, or names a section for the branch
// in either section syntax, or mentions a branch section it cannot tell apart.
func configMayDefineUpstream(config, branch string) bool {
	lower := strings.ToLower(config)
	if configIncludes(config) {
		return true
	}
	if strings.Contains(config, `[branch "`+branch+`"`) || strings.Contains(lower, "[branch."+strings.ToLower(branch)+"]") {
		return true
	}
	// Any section that would be a branch section written another way (escaped
	// quotes, odd spacing) is left to git.
	for _, line := range strings.Split(lower, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[branch") && !strings.HasPrefix(t, `[branch "`) {
			return true
		}
	}
	return false
}
