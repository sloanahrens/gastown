package land

import "path/filepath"

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
func (r OMReviewer) Isolated(tmp string) Reviewer {
	c := r
	c.Out = nil
	c.Verdicts = nil
	c.OutDir = ""
	if tmp != "" {
		c.OutDir = filepath.Join(tmp, "out")
	}
	return c
}
