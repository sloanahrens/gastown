package cmdtree

import (
	"slices"
	"strings"
)

// SameRefs reports whether two versions of the Go file hold the same gt and
// bd invocations: the multiset of Bin, Words and Comment that ScanGo returns,
// ignoring Line and File. A source ScanGo rejects returns its error.
func SameRefs(file string, oldSrc, newSrc []byte) (bool, error) {
	return SameRefsAt(file, oldSrc, file, newSrc)
}

// SameRefsAt is SameRefs for a move: oldSrc is scanned under oldFile and newSrc
// under newFile, since the path decides what ScanGo reads. A rejection errors.
func SameRefsAt(oldFile string, oldSrc []byte, newFile string, newSrc []byte) (bool, error) {
	oldRefs, err := ScanGo(oldFile, oldSrc)
	if err != nil {
		return false, err
	}
	newRefs, err := ScanGo(newFile, newSrc)
	if err != nil {
		return false, err
	}
	return slices.Equal(refKeys(oldRefs), refKeys(newRefs)), nil
}

// refKeys renders each ref as a comparison key and sorts them, so two ref
// sets are equal as multisets. Words cannot hold a space (cmdWord), so joining
// them separates one word list from another.
func refKeys(refs []Ref) []string {
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.Bin + "\x00" + strings.Join(r.Words, " ")
		if r.Comment {
			keys[i] += "\x00#"
		}
	}
	slices.Sort(keys)
	return keys
}
