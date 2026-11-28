package git

import (
	"fmt"
	"slices"
	"strings"
)

// AddedFile is one file a branch adds: its path and the blob the path holds.
type AddedFile struct {
	Path string
	Blob string
}

// AddedFiles lists the files base..head adds and the blob each holds: every
// non-merge commit's addition (a later commit's deletion still counts, since
// the landing merges the branch's commits) plus what only a merge adds.
// Regular files only, one entry per path and blob; a bad record is an error.
func (g *Git) AddedFiles(base, head string) ([]AddedFile, error) {
	logFiles, err := g.addedFiles("log", base+".."+head, "--no-merges", "--format=")
	if err != nil {
		return nil, err
	}
	treeFiles, err := g.addedFiles("diff", base+"..."+head)
	if err != nil {
		return nil, err
	}
	for _, f := range treeFiles {
		if !slices.Contains(logFiles, f) {
			logFiles = append(logFiles, f)
		}
	}
	return logFiles, nil
}

// addedFiles runs one --raw -z --diff-filter=A listing over rev; --end-of-options
// stops a rev beginning with "-" reading as an option.
func (g *Git) addedFiles(verb, rev string, prefix ...string) ([]AddedFile, error) {
	args := append([]string{verb}, prefix...)
	args = append(args, "--raw", "-z", "--no-renames", "--no-abbrev", "--diff-filter=A", "--end-of-options", rev)
	out, err := g.runOutput(args...)
	if err != nil {
		return nil, fmt.Errorf("git %s --raw %s: %w", verb, rev, err)
	}
	return parseAddedRaw(out)
}

// parseAddedRaw reads a --raw -z --diff-filter=A listing: "<meta>\0<path>\0" per file, one leading newline trimmed.
func parseAddedRaw(out string) ([]AddedFile, error) {
	tokens := strings.Split(out, "\x00")
	if len(tokens)%2 != 1 || tokens[len(tokens)-1] != "" {
		return nil, fmt.Errorf("parsing git --raw -z output: %d tokens are not <meta>/<path> pairs ending in NUL", len(tokens))
	}
	tokens = tokens[:len(tokens)-1]
	files := make([]AddedFile, 0, len(tokens)/2)
	for i := 0; i < len(tokens); i += 2 {
		meta, path := strings.TrimLeft(tokens[i], "\n"), tokens[i+1]
		fields := strings.Fields(meta)
		if len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || fields[4] != "A" || path == "" || !isHexObjectID(fields[3]) {
			return nil, fmt.Errorf("parsing git --raw record %d: want ':oldmode newmode oldblob newblob A' and a path, with a full hex new blob", i/2+1)
		}
		if fields[1] == "100644" || fields[1] == "100755" {
			files = append(files, AddedFile{Path: path, Blob: fields[3]})
		}
	}
	return files, nil
}

// BlobBytes returns a blob's exact bytes, refusing a name that is not a full hex object id.
func (g *Git) BlobBytes(blob string) ([]byte, error) {
	if !isHexObjectID(blob) {
		return nil, fmt.Errorf("blob %q is not a full hex object id", blob)
	}
	out, err := g.runOutput("cat-file", "blob", blob)
	if err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %w", blob, err)
	}
	return []byte(out), nil
}
