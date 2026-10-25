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

// Plan returns the rigs a stage writes, buildRig first: buildRig at
// buildCommit, then each pinned rig at its pin. A pin for buildRig is an error:
// that rig's seed is the build commit, and a pin for it would be a line that
// reads as the source while nothing reads it (gt-1rz2r).
func Plan(pins []Source, buildRig, buildCommit string) ([]Source, error) {
	if buildRig == "" {
		return nil, errors.New("no build rig named")
	}
	if !isCommit(buildCommit) {
		return nil, fmt.Errorf("cannot read the build commit: %q is not a 7+ character hex commit, so build from a git checkout", buildCommit)
	}
	out := make([]Source, 0, len(pins)+1)
	out = append(out, Source{Rig: buildRig, Commit: buildCommit})
	for _, p := range pins {
		if p.Rig == buildRig {
			return nil, fmt.Errorf("seed-pins pins %s, but that rig is seeded from the build commit: drop the line (make polecat-seed-pins)", p.Rig)
		}
		out = append(out, p)
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
func Unpack(dir string, tree []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(tree))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
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
