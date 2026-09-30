package git

import (
	"reflect"
	"testing"
)

// allowMutation answers the town-root guard's top-level probe for g, as git
// answers it in an ordinary repository.
func allowMutation(s *scripted, g *Git) {
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
}

func TestPathQueries(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"rev-parse --is-bare-repository":         ok("true\n"),
		"ls-files -- tracked.json":               ok("tracked.json\n"),
		"ls-files -- new.json":                   ok(""),
		"check-ignore -q -- ignored.log":         ok(""),
		"check-ignore -q -- plain.txt":           fail(1, ""),
		"check-ignore -q -- broken":              fail(128, "fatal: bad"),
		"diff --quiet -- a":                      ok(""),
		"diff --cached --quiet -- a":             fail(1, ""),
		"diff --quiet -- b":                      ok(""),
		"diff --cached --quiet -- b":             ok(""),
		"status --porcelain --ignored -- .beads": ok("?? .beads/\n!! .beads/keep\n?? .beads/x\n"),
	})
	g := newTestGit(t, s)
	if bare, err := g.IsBareRepository(); err != nil || !bare {
		t.Errorf("IsBareRepository = %v, %v", bare, err)
	}
	for path, want := range map[string]bool{"tracked.json": true, "new.json": false} {
		if got, err := g.IsTracked(path); err != nil || got != want {
			t.Errorf("IsTracked(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
	for path, want := range map[string]bool{"ignored.log": true, "plain.txt": false} {
		if got, err := g.IsIgnored(path); err != nil || got != want {
			t.Errorf("IsIgnored(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
	if _, err := g.IsIgnored("broken"); err == nil {
		t.Error("IsIgnored treated a git failure as an answer")
	}
	for path, want := range map[string]bool{"a": true, "b": false} {
		if got, err := g.PathChanged(path); err != nil || got != want {
			t.Errorf("PathChanged(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
	if got, err := g.UntrackedPaths(".beads"); err != nil || !reflect.DeepEqual(got, []string{".beads/", ".beads/x"}) {
		t.Errorf("UntrackedPaths = %q, %v", got, err)
	}
	s.noUnscripted(t)
}

func TestPathRepairs(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"remote remove fork":      ok(""),
		"pull --rebase":           ok(""),
		"sparse-checkout disable": ok(""),
	})
	g := newTestGit(t, s)
	allowMutation(s, g)
	for name, err := range map[string]error{"RemoveRemote": g.RemoveRemote("fork"), "PullRebase": g.PullRebase(), "DisableSparseCheckout": g.DisableSparseCheckout()} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, want := range []string{"remote remove fork", "pull --rebase", "sparse-checkout disable"} {
		if !s.hasSent(want) {
			t.Errorf("%q not sent: %q", want, s.sent())
		}
	}
}
