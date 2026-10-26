// Package seed plans and stages the sources the polecat image's Go caches are
// built from: the rig the image is built at, at the build commit, and every
// other rig at the commit deploy/polecat/seed-pins names (gt-1rz2r, gt-ieuvf).
package seed

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Source is one rig in a seed: the rig's name and the commit its sources are
// read from.
type Source struct {
	Rig    string
	Commit string
}

// Git runs git with args in dir and returns its standard output; a directory
// that is not a git checkout fails the way git does.
type Git func(dir string, args ...string) ([]byte, error)

// ExecGit runs the host's git.
func ExecGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}
	return out, nil
}

// Pins parses a seed-pins file: one "<rig> <commit>" per line, with blank lines
// and "#" comments skipped. A file naming no rig is an error, because an empty
// seed is never what an image build meant.
func Pins(data []byte) ([]Source, error) {
	var out []Source
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<rig> <commit>\", got %q", i+1, line)
		}
		out = append(out, Source{Rig: fields[0], Commit: fields[1]})
	}
	if len(out) == 0 {
		return nil, errors.New("names no rig")
	}
	return out, nil
}

// Plan returns the rigs a stage writes, buildRig first: buildRig at its pin when
// seed-pins names one, else at buildCommit; then each other pinned rig at its pin.
// A pinned seed keeps the image's cache identical across commits, so the seed
// layer is reused (the build commit changes every commit, which rebuilt it).
// Guard checks the pin against buildCommit, so the image still builds.
func Plan(pins []Source, buildRig, buildCommit string) ([]Source, error) {
	if buildRig == "" {
		return nil, errors.New("no build rig named")
	}
	if !isCommit(buildCommit) {
		return nil, fmt.Errorf("cannot read the build commit: %q is not a 7+ character hex commit, so build from a git checkout", buildCommit)
	}
	seedAt := buildCommit
	for _, p := range pins {
		if p.Rig == buildRig {
			seedAt = p.Commit
		}
	}
	out := make([]Source, 0, len(pins)+1)
	out = append(out, Source{Rig: buildRig, Commit: seedAt})
	for _, p := range pins {
		if p.Rig != buildRig {
			out = append(out, p)
		}
	}
	return out, nil
}

// Stage writes each source into <dest>/<rig>, read from that rig's canonical
// checkout <town>/<rig>/mayor/rig at the source's commit, so the seed holds
// exactly that commit's tree.
func Stage(dest, town string, sources []Source, git Git) error {
	for _, s := range sources {
		checkout := filepath.Join(town, s.Rig, "mayor", "rig")
		tree, err := git(checkout, "archive", "--format=tar", s.Commit)
		if err != nil {
			return fmt.Errorf("cannot stage rig %s at %s from %s: %w", s.Rig, s.Commit, checkout, err)
		}
		// A rig's directory is written whole: a file the source's new commit
		// deleted must not survive from an earlier stage.
		dir := filepath.Join(dest, s.Rig)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("cannot clear %s: %w", dir, err)
		}
		if err := Unpack(dir, tree); err != nil {
			return fmt.Errorf("cannot unpack rig %s at %s: %w", s.Rig, s.Commit, err)
		}
	}
	return nil
}

// Unpack writes a `git archive --format=tar` stream into dir, which it creates.
// It refuses a tree that would write outside dir: the seed sits under the
// repository, so an escaping link followed by a file through it would unpack
// the tree elsewhere (gt-eu4u0).
func Unpack(dir string, tree []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(tree))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return checkSymlinks(dir)
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("archive entry %q escapes the seed directory", h.Name)
		}
		target := filepath.Join(dir, name)
		// A symlink entry may replace the last component, and only it: every
		// ancestor must be a real directory.
		throughLast := h.Typeflag != tar.TypeSymlink
		if link, err := symlinkInPath(dir, name, throughLast); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		} else if link != "" {
			return fmt.Errorf("archive entry %q writes through symlink %q", h.Name, link)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			err = writeFile(target, tr, fs.FileMode(h.Mode).Perm())
		case tar.TypeSymlink:
			os.Remove(target)
			err = os.Symlink(h.Linkname, target)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
}

// maxLinkHops bounds the symlinks followed while resolving one path: a seed
// whose links form a cycle names no target that can be checked.
const maxLinkHops = 40

// checkSymlinks refuses a seed that holds a symlink resolving outside dir: a
// link's name alone does not say where it points, because the link can route
// through another link in the same seed, so each is resolved as the kernel
// would.
func checkSymlinks(dir string) error {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		if _, inside, err := resolvePath(root, p); err != nil {
			return err
		} else if !inside {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				rel = p
			}
			return fmt.Errorf("archive symlink %q escapes the seed directory", rel)
		}
		return nil
	})
}

// resolvePath returns the path that p, an absolute path under root, refers to
// once every symlink in the seed is followed, and whether that path stays under
// root. A component that does not exist, or one under a regular file, ends the
// link walk: nothing below it can be a link, so the rest is read as written. An
// absolute target is outside root: it names a path on the host, and the seed
// must unpack the same tree wherever it is built, so the target's ".." cannot
// be resolved against the host and compared with root's text.
func resolvePath(root, p string) (resolved string, inside bool, err error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	exists := true
	hops := maxLinkHops
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, part)
		if !exists {
			cur = next
			continue
		}
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
			exists = false
		case err != nil:
			return "", false, err
		case fi.Mode()&fs.ModeSymlink != 0:
			if hops--; hops < 0 {
				return "", false, fmt.Errorf("%s: too many levels of symbolic links", rel)
			}
			link, err := os.Readlink(next)
			if err != nil {
				return "", false, err
			}
			if filepath.IsAbs(link) {
				return filepath.Clean(link), false, nil
			}
			parts = append(strings.Split(link, string(filepath.Separator)), parts...)
			continue
		}
		cur = next
	}
	return cur, within(root, cur), nil
}

// within reports whether p is root itself or a path under it. Rel resolves ".."
// in both paths, so a textual prefix of root does not count as inside.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// symlinkInPath returns the first component of name inside dir that is a
// symlink, checking the last component too when last is set, or "" when none
// is. It stops at the first component that does not exist: the entries below
// it have not been written yet.
func symlinkInPath(dir, name string, last bool) (string, error) {
	parts := strings.Split(name, string(filepath.Separator))
	if !last {
		parts = parts[:len(parts)-1]
	}
	rel := ""
	for _, part := range parts {
		rel = filepath.Join(rel, part)
		fi, err := os.Lstat(filepath.Join(dir, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", nil
		case err != nil:
			return "", err
		case fi.Mode()&fs.ModeSymlink != 0:
			return rel, nil
		}
	}
	return "", nil
}

// writeFile writes r to target at mode, making its directory.
func writeFile(target string, r io.Reader, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// isCommit reports whether s has the shape of git's abbreviated or full commit
// sha: the shape `git rev-parse HEAD` prints.
func isCommit(s string) bool {
	if len(s) < 7 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// lastLine prints the last non-blank line of s: git's reason for a failure is
// its last line, and everything before is noise.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	lines := strings.Split(s, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
