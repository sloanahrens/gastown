package gitfake

import (
	"fmt"
	"maps"
	"slices"

	"github.com/steveyegge/gastown/internal/git"
)

// AddedFiles lists the files base..head adds, as *git.Git.AddedFiles does: one
// entry per path and blob, from the range's commits and head's tree over the
// merge base's. A revision that does not resolve is an error.
func (h *handle) AddedFiles(base, head string) ([]git.AddedFile, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	b, hd, err := h.resolveRange(base, head, "log", "--raw", base+".."+head)
	if err != nil {
		return nil, err
	}
	seen := map[git.AddedFile]bool{}
	files := []git.AddedFile{}
	add := func(path, content string) {
		file := git.AddedFile{Path: path, Blob: blobID(content)}
		if !seen[file] {
			seen[file] = true
			files = append(files, file)
		}
	}
	for _, c := range h.f.rangeCommits(b, hd) {
		if len(c.parents) > 1 {
			continue
		}
		var parent map[string]string
		if len(c.parents) == 1 {
			parent = h.f.treeOf(c.parents[0])
		}
		for _, path := range slices.Sorted(maps.Keys(c.tree)) {
			if _, ok := parent[path]; !ok {
				add(path, c.tree[path])
			}
		}
	}
	headTree, baseTree := h.f.treeOf(hd), map[string]string{}
	if mb := h.f.mergeBase(b, hd); mb != "" {
		baseTree = h.f.treeOf(mb)
	}
	for _, path := range slices.Sorted(maps.Keys(headTree)) {
		if _, ok := baseTree[path]; !ok {
			add(path, headTree[path])
		}
	}
	return files, nil
}

// BlobBytes returns a blob's exact bytes, as *git.Git.BlobBytes does, or an
// error for a name that is not a full hex object id or a blob the world lacks.
func (h *handle) BlobBytes(blob string) ([]byte, error) {
	if !isFullID(blob) {
		return nil, fmt.Errorf("blob %q is not a full hex object id", blob)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"cat-file", "blob", blob}
	if _, _, err := h.locate(args...); err != nil {
		return nil, err
	}
	content, ok := h.blobContent(blob)
	if !ok {
		return nil, gitErr(128, "fatal: Not a valid object name "+blob, args...)
	}
	return []byte(content), nil
}
