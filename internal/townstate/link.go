package townstate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// maxLinkHops bounds the symlinks a link target may pass through, the same
// bound the kernel applies, so a loop of links is refused rather than followed.
const maxLinkHops = 40

// linkRefusal is a target the town root or the manifest refuses, as opposed to
// a tree that could not be read. Export skips a refused link and says why.
type linkRefusal struct {
	// class names the refusal without a path, which is what an export summary
	// prints: the summary never names a file below the top-level entry.
	class string
	err   error
}

func (e linkRefusal) Error() string { return e.err.Error() }
func (e linkRefusal) Unwrap() error { return e.err }

// refusal builds a linkRefusal whose class is a short phrase and whose error
// carries the detail an import prints.
func refusal(class, format string, args ...any) error {
	return linkRefusal{class: class, err: fmt.Errorf(format, args...)}
}

// linkTree answers what a tree holds at a town-root-relative name. Export and
// Import each supply one, so a link target is followed by the same traversal in
// both. A name the tree does not hold is absent; only the import's destination
// can fail to read one it does hold.
type linkTree interface {
	// at returns the entry at rel, the target a symlink carries, and whether
	// the name is there at all.
	at(rel string) (info fs.FileInfo, target string, ok bool, err error)
}

// fsLinkTree reads a real directory tree, the destination an import writes into.
type fsLinkTree struct {
	ops  townOps
	root string
}

func (t fsLinkTree) at(rel string) (fs.FileInfo, string, bool, error) {
	full := filepath.Join(t.root, filepath.FromSlash(rel))
	info, err := t.ops.lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", false, nil
	case err != nil:
		return nil, "", false, fmt.Errorf("reading %s in the destination: %w", rel, err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return info, "", true, nil
	}
	target, err := t.ops.readlink(full)
	if err != nil {
		return nil, "", false, fmt.Errorf("reading the symlink %s in the destination: %w", rel, err)
	}
	return info, target, true, nil
}

// resolveLinkTarget is the one lookup Export and Import share for a symlink: it
// accepts the link or refuses it. An empty or absolute target, one that climbs
// above the town root, one that follows more links than maxLinkHops, and one
// that does not land under one of entries are each refused. The target is
// followed link by link, never opened, so no file's content is read.
//
// The tree is the archive's written names for an export and the destination as
// extracted so far for an import, and both pass the same entries, so a link the
// import would refuse is never archived.
func resolveLinkTarget(tree linkTree, dir, link string, entries []Entry) error {
	if link == "" {
		return refusal("a link with an empty target", "has an empty target")
	}
	if path.IsAbs(link) || filepath.IsAbs(link) {
		return refusal("a link with an absolute target",
			"points at the absolute path %q; a copy keeps only links that stay within the town root", link)
	}
	target, err := resolveInTree(tree, dir, link)
	if err != nil {
		return err
	}
	if _, ok := Lookup(entries, target); !ok {
		return refusal("a link that lands outside the daemon-owned rows",
			"points at %q, which is not under a daemon-owned path of the manifest", target)
	}
	return nil
}

// resolveInTree follows link, written relative to the directory dir, through the
// tree and returns the town-root-relative path it names. Each symlink on the way
// is followed in that tree, so one an archive created earlier is followed too. A
// name that does not exist yet may end the target (a dangling link), but a ".."
// after one is refused: a later entry could create that name as a link and move
// where the ".." lands.
func resolveInTree(tree linkTree, dir, link string) (string, error) {
	cur := dir
	pending := strings.Split(link, "/")
	hops := 0
	missing := ""
	for len(pending) > 0 {
		seg := pending[0]
		pending = pending[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if missing != "" {
				return "", refusal("a link that climbs out of a name that does not exist yet",
					"passes %s, which does not exist yet, and then climbs out of it", missing)
			}
			if cur == "." {
				return "", refusal("a link that leaves the town root", "points outside the town root")
			}
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, seg)
		info, target, ok, err := tree.at(next)
		switch {
		case err != nil:
			return "", err
		case !ok:
			if missing == "" {
				missing = next
			}
			cur = next
		case info.Mode()&fs.ModeSymlink != 0:
			hops++
			if hops > maxLinkHops {
				return "", refusal("a link chain longer than 40 links",
					"passes more than %d symlinks, which may loop", maxLinkHops)
			}
			if path.IsAbs(target) || filepath.IsAbs(target) {
				return "", refusal("a link through an absolute link",
					"passes the symlink %s, which points at the absolute path %q", next, target)
			}
			// The inner target is written relative to the directory that holds
			// the link, which is cur: cur does not move.
			pending = append(strings.Split(target, "/"), pending...)
		default:
			cur = next
		}
	}
	return cur, nil
}
