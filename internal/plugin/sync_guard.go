package plugin

import (
	"bufio"
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: git object ids are SHA-1; used for identity, not security
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// blobHistory maps a repo-relative path to every git blob id that path has
// held in any commit. A nil blobHistory means the source has no readable
// history, and the guard fails closed.
type blobHistory map[string]map[string]bool

// sourceHistory reads the blob history of sourceDir's files from the git
// repository containing it. It returns nil history (not an error) when the
// source is not in a git repository or git is unavailable.
func sourceHistory(sourceDir string) (hist blobHistory, prefix string) {
	top, err := exec.Command("git", "-C", sourceDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil, ""
	}
	root := strings.TrimSpace(string(top))
	// Resolve symlinks on both sides (macOS /var -> /private/var) so the
	// prefix is computed between comparable paths.
	realRoot, err1 := filepath.EvalSymlinks(root)
	realSrc, err2 := filepath.EvalSymlinks(sourceDir)
	if err1 != nil || err2 != nil {
		return nil, ""
	}
	rel, err := filepath.Rel(realRoot, realSrc)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, ""
	}
	prefix = filepath.ToSlash(rel)
	if prefix == "." {
		prefix = ""
	}

	args := []string{"-C", root, "log", "--all", "--format=", "--raw", "--no-abbrev", "--no-renames", "--"}
	if prefix != "" {
		args = append(args, prefix)
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, ""
	}
	hist = blobHistory{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// :100644 100755 <old> <new> M\t<path>
		line := sc.Text()
		tab := strings.IndexByte(line, '\t')
		if !strings.HasPrefix(line, ":") || tab < 0 {
			continue
		}
		fields := strings.Fields(line[:tab])
		if len(fields) < 4 {
			continue
		}
		p := line[tab+1:]
		for _, id := range fields[2:4] {
			if strings.Trim(id, "0") == "" {
				continue // all-zero id: the path did not exist on that side
			}
			if hist[p] == nil {
				hist[p] = map[string]bool{}
			}
			hist[p][id] = true
		}
	}
	return hist, prefix
}

// gitBlobID returns the git object id git would assign to data as a blob.
func gitBlobID(data []byte) string {
	h := sha1.New() //nolint:gosec // G401: see import
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// runtimeEdits lists the files in the runtime copy dstPluginDir that a sync
// would destroy: content the source repo never held at that path (a hand
// edit, or a file that exists only in the runtime copy). A file identical to
// the current source, or to any version the repo held, is an older copy and
// safe to replace. With nil history every differing file counts (fail closed).
func runtimeEdits(name, srcPluginDir, dstPluginDir string, hist blobHistory, prefix string) ([]string, error) {
	var edits []string
	err := filepath.WalkDir(dstPluginDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dstPluginDir, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: walking the town's plugin directory
		if err != nil {
			return err
		}
		if src, err := os.ReadFile(filepath.Join(srcPluginDir, rel)); err == nil && bytes.Equal(src, data) { //nolint:gosec // G304: trusted source tree
			return nil
		}
		repoPath := path.Join(prefix, name, filepath.ToSlash(rel))
		if hist != nil && hist[repoPath][gitBlobID(data)] {
			return nil
		}
		edits = append(edits, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(edits)
	return edits, err
}
