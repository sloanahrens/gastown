package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestOMSkewRefreshClone: the om clone is made from the bare repository's url
// on first use, fetched on every call; a url that fails or is empty is an error.
func TestOMSkewRefreshClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want, bare := filepath.Join(town, ".runtime", "om-skew-src"), filepath.Join(town, "om", ".repo.git")
	var calls []string
	d := &Daemon{execCmd: hmSkewRunner(t, hmSkewAnswers{calls: &calls})}
	for i := range 2 {
		if src, err := d.omSkewRefreshClone(town); err != nil || src != want {
			t.Fatalf("call %d: src = %q, err = %v; want %q", i, src, err, want)
		}
	}
	fetch := " fetch --quiet --no-tags -- " + hmSkewTestURL + " +refs/heads/main:refs/remotes/origin/main"
	if len(calls) != 5 || !strings.Contains(calls[0], " --git-dir="+bare+" remote get-url origin") ||
		!strings.Contains(calls[1], " clone ") || !strings.Contains(calls[1], " "+want) ||
		!strings.Contains(calls[2], fetch) || !strings.Contains(calls[4], fetch) {
		t.Fatalf("calls = %q, want the url read, a clone into %s, and a fetch on each call", calls, want)
	}
	for _, a := range []hmSkewAnswers{{originErr: true}, {noOrigin: true}, {cloneErr: true}} {
		var got []string
		a.calls = &got
		if _, err := (&Daemon{execCmd: hmSkewRunner(t, a)}).omSkewRefreshClone(t.TempDir()); err == nil ||
			strings.Contains(err.Error(), hmSkewTestURL) ||
			strings.Contains(strings.Join(got, "\n"), " fetch ") || ((a.originErr || a.noOrigin) && len(got) > 1) {
			t.Errorf("%+v: calls = %q, err = %v; want an error carrying no url and nothing cloned or fetched", a, got, err)
		}
	}
}
