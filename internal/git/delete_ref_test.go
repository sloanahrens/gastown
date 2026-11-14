package git

import "testing"

// DeleteRef deletes the full ref name it is given, so a private ref such as
// refs/local-round/<bead> can be removed by update-ref -d.
func TestDeleteRefDeletesTheFullRefName(t *testing.T) {
	t.Parallel()
	const ref = "refs/local-round/gt-abc"
	s := newScripted(nil)
	g := newTestGit(t, s)
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
	s.on("update-ref -d "+ref, ok(""))
	if err := g.DeleteRef(ref); err != nil {
		t.Fatalf("DeleteRef: %v", err)
	}
	s.noUnscripted(t)
	if !s.hasSent("update-ref -d " + ref) {
		t.Fatalf("calls %q; want update-ref -d of the ref", s.sent())
	}
}

// DeleteRef treats an absent ref as success: git update-ref -d exits 0 with no
// output when the ref is already gone, and DeleteRef must not check first and
// turn that into an error (gt-d5ojod).
func TestDeleteRefAbsentRefIsNotAnError(t *testing.T) {
	t.Parallel()
	const ref = "refs/local-round/gt-gone"
	s := newScripted(nil)
	g := newTestGit(t, s)
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
	s.on("update-ref -d "+ref, ok(""))
	if err := g.DeleteRef(ref); err != nil {
		t.Fatalf("DeleteRef on an absent ref: %v; want nil", err)
	}
	s.noUnscripted(t)
}

// DeleteRef reports git's failure; a caller that ignores it would leave the
// ref behind without a word.
func TestDeleteRefReturnsAFailure(t *testing.T) {
	t.Parallel()
	const ref = "refs/local-round/gt-abc"
	s := newScripted(nil)
	g := newTestGit(t, s)
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
	s.on("update-ref -d "+ref, fail(128, "fatal: cannot lock ref"))
	if err := g.DeleteRef(ref); err == nil {
		t.Fatal("DeleteRef returned no error for a failed update-ref")
	}
}
