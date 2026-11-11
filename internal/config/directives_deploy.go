package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// centralDirectiveRig is the rig whose bare repo carries the central
// directives: directives/*.md at the installed commit.
const centralDirectiveRig = "gastown"

// directiveGitTimeout bounds one git call the deploy makes against the bare repo.
const directiveGitTimeout = 30 * time.Second

// ErrDirectiveCommitMissing is wrapped when the bare repo does not hold the
// commit asked for. The doctor reports it as a warning, not a failed check.
var ErrDirectiveCommitMissing = errors.New("no such commit")

// directiveCommitPattern is a commit id as the deploy accepts it: hex, so it
// cannot start with "-" or carry anything but an object name into argv.
var directiveCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// CentralDirectiveRepo is the bare repo the central directives deploy from,
// <town>/gastown/.repo.git. The install and the deploy both build the path here.
func CentralDirectiveRepo(townRoot string) string {
	return constants.RigBareRepoPath(filepath.Join(townRoot, centralDirectiveRig))
}

// DeployCentralDirectives writes <town>/directives from the directives/*.md
// files at commit in the gastown bare repo. A deployed *.md the commit no longer
// has is removed; other files in the directory are left alone. An empty commit
// resolves to buildCommit(). config cannot import version (version imports
// config), so the caller passes version.BuildCommit. It returns the paths it
// wrote; a second run over the same commit writes none.
func DeployCentralDirectives(townRoot, commit string, buildCommit func() string) ([]string, error) {
	return deployFrom(townRoot, commit, buildCommit, defaultDirectiveSource(townRoot), realDirFS())
}

// dirFS is the part of the filesystem the deploy lists and removes through. The
// real one is realDirFS; a unit test injects a failing one, because a permission
// bit cannot fail a read in the unit tier and root reads anything regardless.
type dirFS struct {
	readDir func(name string) ([]fs.DirEntry, error)
	remove  func(name string) error
}

func realDirFS() dirFS {
	return dirFS{readDir: os.ReadDir, remove: os.Remove}
}

// directiveSource is where the central directives are read from: one commit and
// its directives/ tree. The git-backed source reads the gastown bare repo; a
// unit test supplies its own.
type directiveSource interface {
	// hasCommit returns an error when the source does not hold commit.
	hasCommit(commit string) error
	// listDirectives returns the directives/*.md entries at commit.
	listDirectives(commit string) ([]directiveEntry, error)
	// readDirective returns the bytes of directives/<name> at commit.
	readDirective(commit, name string) ([]byte, error)
}

// directiveEntry is one directives/*.md entry at a commit. A symlink is listed
// so the deploy can refuse it by name; its target is never read.
type directiveEntry struct {
	name    string
	symlink bool
}

// defaultDirectiveSource is the source DeployCentralDirectives reads: the
// gastown bare repo under townRoot, through real git.
func defaultDirectiveSource(townRoot string) directiveSource {
	return gitDirectiveSource{repo: CentralDirectiveRepo(townRoot), run: execGit}
}

// ReadCentralDirectives returns the directives/*.md files at commit in the
// gastown bare repo, name to bytes. It makes the same checks as the deploy (a
// commit id, a commit the repo holds, flat names, no symlinks) and writes
// nothing; the doctor compares its result with the deployed copy. An empty
// commit resolves to buildCommit(), as in DeployCentralDirectives.
func ReadCentralDirectives(townRoot, commit string, buildCommit func() string) (map[string][]byte, error) {
	return readDirectives(commit, buildCommit, defaultDirectiveSource(townRoot))
}

// deployFrom is DeployCentralDirectives over an explicit source, build commit
// and filesystem. Every read happens before the first write, so a refused or
// unreadable entry leaves the deployed directory as it was.
func deployFrom(townRoot, commit string, buildCommit func() string, src directiveSource, fsys dirFS) ([]string, error) {
	want, err := readDirectives(commit, buildCommit, src)
	if err != nil {
		return nil, err
	}

	dir := CentralDirectiveDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("central directives: creating %s: %w", dir, err)
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)

	var written []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		if directiveFileHolds(path, want[name]) {
			continue
		}
		if err := atomicfile.WriteFile(path, want[name], 0o644); err != nil {
			return written, fmt.Errorf("central directives: writing %s: %w", path, err)
		}
		written = append(written, path)
	}

	deployed, err := fsys.readDir(dir)
	if err != nil {
		return written, fmt.Errorf("central directives: reading %s: %w", dir, err)
	}
	for _, de := range deployed {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if _, ok := want[name]; ok {
			continue
		}
		if err := fsys.remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return written, fmt.Errorf("central directives: removing %s: %w", name, err)
		}
	}
	return written, nil
}

// readDirectives returns the directives/*.md files at commit from src, name to
// bytes. It refuses the whole commit on the first bad entry, so no caller ever
// holds a partial set.
func readDirectives(commit string, buildCommit func() string, src directiveSource) (map[string][]byte, error) {
	if commit == "" && buildCommit != nil {
		commit = buildCommit()
	}
	if commit == "" {
		return nil, errors.New("central directives: no commit to deploy from (the build has none, and none was given)")
	}
	if !directiveCommitPattern.MatchString(commit) {
		return nil, fmt.Errorf("central directives: %q is not a commit id", commit)
	}
	if err := src.hasCommit(commit); err != nil {
		return nil, fmt.Errorf("central directives: %w", err)
	}
	entries, err := src.listDirectives(commit)
	if err != nil {
		return nil, fmt.Errorf("central directives: %w", err)
	}

	for _, e := range entries {
		if !validDirectiveName(e.name) {
			return nil, fmt.Errorf("central directives: %q at commit %s is not a directive file name", e.name, commit)
		}
		if e.symlink {
			return nil, fmt.Errorf("central directives: directives/%s at commit %s is a symlink; refusing to deploy it", e.name, commit)
		}
	}
	want := make(map[string][]byte, len(entries))
	for _, e := range entries {
		data, err := src.readDirective(commit, e.name)
		if err != nil {
			return nil, fmt.Errorf("central directives: reading directives/%s at commit %s: %w", e.name, commit, err)
		}
		want[e.name] = data
	}
	return want, nil
}

// validDirectiveName reports whether name is a flat *.md file name, the only
// shape the deploy writes into <town>/directives.
func validDirectiveName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		filepath.Base(name) == name && !strings.ContainsAny(name, "\\\x00") &&
		strings.HasSuffix(name, ".md")
}

// directiveFileHolds reports whether path already holds exactly data. Anything
// else counts as not held, so the write replaces it: a symlink is not followed
// to its target's bytes, and a file that cannot be read is rewritten, where a
// real failure of the write is still reported.
func directiveFileHolds(path string, data []byte) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	cur, err := os.ReadFile(path) //nolint:gosec // G304: path is under the town's directives dir
	return err == nil && bytes.Equal(cur, data)
}

// gitRunner runs one git command against a bare repo and returns its stdout.
type gitRunner func(repo string, args ...string) ([]byte, error)

// gitDirectiveSource reads directives from a bare repo with git plumbing: the
// commit's object store, never a working tree. run is the git it calls, so a
// unit test can script git.
type gitDirectiveSource struct {
	repo string
	run  gitRunner
}

func (s gitDirectiveSource) hasCommit(commit string) error {
	if _, err := s.run(s.repo, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not in %s: %w (%w)", commit, s.repo, ErrDirectiveCommitMissing, err)
	}
	return nil
}

func (s gitDirectiveSource) listDirectives(commit string) ([]directiveEntry, error) {
	out, err := s.run(s.repo, "ls-tree", "-z", commit+":directives")
	if err != nil {
		return nil, fmt.Errorf("commit %s has no directives/ tree in %s: %w", commit, s.repo, err)
	}
	entries, err := parseDirectiveTree(out)
	if err != nil {
		return nil, fmt.Errorf("commit %s in %s: %w", commit, s.repo, err)
	}
	return entries, nil
}

// parseDirectiveTree reads `git ls-tree -z` output: one NUL-terminated record per
// entry, "<mode> SP <type> SP <object> TAB <name>". It keeps the blobs named
// *.md and marks the ones that are symlinks.
func parseDirectiveTree(out []byte) ([]directiveEntry, error) {
	var entries []directiveEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, name, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, errors.New("malformed directives/ entry in ls-tree output")
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 || fields[1] != "blob" || !strings.HasSuffix(name, ".md") {
			continue
		}
		entries = append(entries, directiveEntry{name: name, symlink: fields[0] == "120000"})
	}
	return entries, nil
}

func (s gitDirectiveSource) readDirective(commit, name string) ([]byte, error) {
	return s.run(s.repo, "cat-file", "blob", commit+":directives/"+name)
}

// execGit is the gitRunner the deploy uses: real git, bounded by
// directiveGitTimeout. Its error is git's exit status, never its output, which
// can hold anything.
func execGit(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), directiveGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + repo}, args...)...)
	return cmd.Output()
}
