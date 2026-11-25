package land

import (
	"context"
	"io"
	"path/filepath"
	"slices"
)

// Isolator is a Reviewer that can return a copy of itself whose verdict files
// live under a directory of the caller's, so a review run beside a landing
// shares none of om's files with the landing's review (gt-ozk4zj).
type Isolator interface {
	Isolated(tmp string) Reviewer
}

// Isolated returns a copy of r whose verdict files go under tmp/out; r is not
// changed. The copy drops Out and Verdicts: a pool review must not stream om's
// output into the landing's writer, and VerdictStore.Write prunes the bead's
// older files, which the landing owns. An empty tmp leaves the copy with no
// verdict directory, so its review refuses instead of writing into the working
// directory. Isolated creates and removes nothing; reviewOnce creates tmp/out.
//
// The copy's om children also get TMPDIR=tmp: om keeps its raw replies in
// os.TempDir() when its caller names no directory, so two reviews at once would
// overwrite each other's files there. Only the copy is wrapped; r's own om runs
// with the environment the landing gives it.
//
// The copy reads a failed review's raw reply from tmp too, the directory its own
// om saves one in: the receiver's RawDir is another review's, so a pool review
// that exits without a verdict would otherwise quote that one (gt-hnbpyp).
func (r OMReviewer) Isolated(tmp string) Reviewer {
	c := r
	c.Out = nil
	c.Verdicts = nil
	c.OutDir = ""
	if tmp != "" {
		c.OutDir = filepath.Join(tmp, "out")
		c.run = withEnv(r.run, "TMPDIR="+tmp)
		c.RawDir = func() string { return tmp }
	}
	return c
}

// OMVersion returns the version of the om this reviewer runs, the string
// ReviewBead stamps on Verdict.Version: a stored review is only reusable under
// the om that made it, so a caller compares it before reusing one (gt-jtdmp4).
//
// It is the same cached probe ReviewBead reads (omVersion), so the answer a
// caller gets is the one that review would record, and a probe that fails
// reports omVersionUnknown exactly as the verdict does. It takes the caller's
// ReviewInput because the probe runs `om version` in in.Dir and warns as
// in.Logf directs; constructing one of its own is what TestReviewInputGuard
// refuses.
func (r OMReviewer) OMVersion(ctx context.Context, in ReviewInput) string {
	return r.omVersion(ctx, in)
}

// withEnv returns run with extra appended to the env it gives each child, an
// entry with "=" setting that variable. It keeps extra out of the caller's
// slice's backing array, which may be a package-level list every reviewer
// shares, so one copy's variable never reaches another's children.
func withEnv(run runFunc, extra string) runFunc {
	if run == nil {
		run = realRun
	}
	return func(ctx context.Context, dir string, env, argv []string, out io.Writer) (int, error) {
		return run(ctx, dir, append(slices.Clip(env), extra), argv, out)
	}
}
