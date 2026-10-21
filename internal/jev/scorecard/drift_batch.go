package scorecard

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// inputGit is a cloneGit that can also feed git's standard input, which lets a
// doc's history be read in two calls: a log with the patches, and one cat-file
// --batch for the text at every edit's parent.
type inputGit interface {
	RunInput(ctx context.Context, input string, args ...string) (string, error)
}

// RunInput runs git in the town's bare clone with input on its standard input.
func (c *townClone) RunInput(ctx context.Context, input string, args ...string) (string, error) {
	if err := c.open(); err != nil {
		return "", err
	}
	return c.clone.RunInput(ctx, input, args...)
}

// editsBatched is Edits in two git calls per doc, however many edits it has.
// It returns what the one-call-per-edit path does: the same edits, with the
// same Before and Patch.
func (g docGit) editsBatched(ctx context.Context, git inputGit, doc string) ([]docEdit, error) {
	// A diff line starts with a space, +, -, \ or @, so \x01 at the start of a
	// line can only be a commit header.
	log, err := g.git.Run(ctx, "log", "--reverse", "--no-merges", "--format=%x01%H%x00%P%x00%ct",
		"--unified=0", "--no-renames", "-p", docTarget, "--", doc)
	if err != nil {
		return nil, err
	}
	var (
		out     []docEdit
		parents []string // each edit's first parent, empty for a root commit
	)
	for _, block := range splitLogBlocks(log) {
		header, body, _ := strings.Cut(block, "\n")
		fields := strings.Split(header, "\x00")
		if len(fields) != 3 {
			return nil, fmt.Errorf("git log for %s: unexpected line %q", doc, header)
		}
		secs, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("git log for %s: bad commit time %q: %w", doc, fields[2], err)
		}
		parent := ""
		if p := strings.Fields(fields[1]); len(p) > 0 {
			parent = p[0]
		}
		parents = append(parents, parent)
		// log prints a blank line between the header and the patch, which show
		// --format= does not.
		out = append(out, docEdit{Time: time.Unix(secs, 0), Patch: strings.TrimPrefix(body, "\n")})
	}

	var in strings.Builder
	for _, p := range parents {
		if p != "" {
			in.WriteString(p + ":" + doc + "\n")
		}
	}
	if in.Len() == 0 {
		return out, nil
	}
	blobs, err := git.RunInput(ctx, in.String(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for i, p := range parents {
		if p == "" {
			continue
		}
		text, rest, ok := nextBlob(blobs)
		if !ok {
			// An answer this reader cannot place: read this edit's parent the
			// slow way, and the rest of the output with it.
			return g.editsOneByOne(ctx, doc)
		}
		out[i].Before, blobs = text, rest
	}
	return out, nil
}

// splitLogBlocks cuts `git log -p` output at its \x01 commit headers: each
// block is a header line followed by its patch.
func splitLogBlocks(log string) []string {
	var blocks []string
	for log != "" {
		rest, ok := strings.CutPrefix(log, "\x01")
		if !ok {
			return blocks
		}
		next := strings.Index(rest, "\n\x01")
		if next < 0 {
			return append(blocks, rest)
		}
		blocks = append(blocks, rest[:next+1])
		log = rest[next+1:]
	}
	return blocks
}

// nextBlob reads one object from `git cat-file --batch` output: its text, and
// what follows it. An object the repo lacks reads as empty text, as a doc the
// parent does not hold does. ok is false for anything else, such as a tree.
func nextBlob(out string) (text, rest string, ok bool) {
	line, after, found := strings.Cut(out, "\n")
	if !found {
		return "", "", false
	}
	fields := strings.Fields(line)
	switch {
	case len(fields) == 2 && fields[1] == "missing":
		return "", after, true
	case len(fields) == 3 && fields[1] == "blob":
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || len(after) < size+1 {
			return "", "", false
		}
		return after[:size], after[size+1:], true
	}
	return "", "", false
}
