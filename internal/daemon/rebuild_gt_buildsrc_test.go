package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/version"
)

// gitCallVerb returns the git verb of c: its first argument that is neither a
// flag nor the directory a -C flag names. The verb is found by what it is, not
// by where it sits, so a reordered argument does not change the answer.
func gitCallVerb(c cliCall) string {
	if c.name != "git" {
		return ""
	}
	for i := 0; i < len(c.args); i++ {
		if c.args[i] == "-C" {
			i++ // skip the directory the flag names
			continue
		}
		if !strings.HasPrefix(c.args[i], "-") {
			return c.args[i]
		}
	}
	return ""
}

// gitCallHas reports whether c carries arg anywhere in its argv. The preflight's
// revisions are matched by content, not by index, for the same reason as the verb.
func gitCallHas(c cliCall, arg string) bool {
	return c.name == "git" && slices.Contains(c.args, arg)
}

// TestRebuildGTOnMain_GitFailureIsAnErrorNotARefusal pins the preflight's
// failure path: a git call that cannot be read is an error the caller defers
// on, never a refusal that settles the cycle as "not on main".
func TestRebuildGTOnMain_GitFailureIsAnErrorNotARefusal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		answer func(c cliCall) cliReply
	}{
		{
			name: "branch read fails",
			answer: func(c cliCall) cliReply {
				if gitCallVerb(c) == "branch" {
					return cliReply{code: 128, stderr: "fatal: not a git repository"}
				}
				return cliReply{}
			},
		},
		{
			name: "detached HEAD does not resolve",
			answer: func(c cliCall) cliReply {
				if gitCallVerb(c) == "rev-parse" && gitCallHas(c, "HEAD^{commit}") {
					return cliReply{code: 128, stderr: "fatal: bad revision"}
				}
				return cliReply{}
			},
		},
		{
			name: "origin/main does not resolve",
			answer: func(c cliCall) cliReply {
				if gitCallVerb(c) == "rev-parse" && gitCallHas(c, "refs/remotes/origin/main^{commit}") {
					return cliReply{code: 128, stderr: "fatal: bad revision"}
				}
				return cliReply{}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := rebuildGTTown(t)
			withRebuildGTCli(t, d, tc.answer)

			onMain, why, err := d.rebuildGTOnMain(filepath.Join(d.config.TownRoot, "gastown"))
			if err == nil {
				t.Fatalf("rebuildGTOnMain: want an error to defer on, got onMain=%v why=%q", onMain, why)
			}
			if onMain || why != "" {
				t.Fatalf("rebuildGTOnMain: a failed read reported onMain=%v why=%q, want neither", onMain, why)
			}
		})
	}
}

// TestRebuildGTOnMain_AcceptsOnlyMainsTip pins the preflight's branch decision
// against the answers a build checkout gives git: detached at origin/main's tip
// and on main are accepted; detached at another commit and on another branch
// are refused, each with its reason.
func TestRebuildGTOnMain_AcceptsOnlyMainsTip(t *testing.T) {
	t.Parallel()
	const (
		tip   = "1111111111111111111111111111111111111111"
		other = "2222222222222222222222222222222222222222"
	)
	cases := []struct {
		name    string
		branch  string // what git branch --show-current prints
		head    string // what HEAD resolves to
		wantOK  bool
		wantWhy string
	}{
		{name: "detached at origin/main is accepted", head: tip, wantOK: true},
		{name: "on main is accepted", branch: "main", head: tip, wantOK: true},
		{
			name:    "detached at another commit is refused",
			head:    other,
			wantWhy: "not on main branch (detached at " + shortSHA(other) + ", not origin/main)",
		},
		{name: "on another branch is refused", branch: "feature", head: tip, wantWhy: "not on main branch (on feature)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _ := rebuildGTTown(t)
			withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitCallVerb(c) == "branch":
					return cliReply{stdout: tc.branch + "\n"}
				case gitCallVerb(c) == "rev-parse" && gitCallHas(c, "HEAD^{commit}"):
					return cliReply{stdout: tc.head + "\n"}
				case gitCallVerb(c) == "rev-parse" && gitCallHas(c, "refs/remotes/origin/main^{commit}"):
					return cliReply{stdout: tip + "\n"}
				}
				return cliReply{}
			})

			onMain, why, err := d.rebuildGTOnMain(filepath.Join(d.config.TownRoot, "gastown"))
			if err != nil {
				t.Fatalf("rebuildGTOnMain: unexpected error %v", err)
			}
			if onMain != tc.wantOK || why != tc.wantWhy {
				t.Fatalf("rebuildGTOnMain = (%v, %q), want (%v, %q)", onMain, why, tc.wantOK, tc.wantWhy)
			}
		})
	}
}

// TestRunRebuildGT_TakesItsRepoRootFromEnsure pins the wiring: the cycle's
// checks read the checkout buildsrc.Ensure returned, not a path it derived
// itself.
func TestRunRebuildGT_TakesItsRepoRootFromEnsure(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	want := filepath.Join(t.TempDir(), "build-src", "gastown")
	var asked []string
	d.rebuildGTEnsureFn = func(_ context.Context, townRoot, rig, rev string) (string, error) {
		asked = append(asked, townRoot+"|"+rig+"|"+rev)
		return want, nil
	}
	var staleOf []string
	d.rebuildGTStaleFn = func(repoRoot string) *version.StaleBinaryInfo {
		staleOf = append(staleOf, repoRoot)
		return nil
	}
	d.runRebuildGT()

	if len(asked) != 1 || asked[0] != d.config.TownRoot+"|gastown|" {
		t.Fatalf("Ensure called as %v, want one call for the gastown rig at the bare main", asked)
	}
	if len(staleOf) != 1 || staleOf[0] != want {
		t.Fatalf("staleness read at %v, want the Ensure checkout %s", staleOf, want)
	}
}

// TestRunRebuildGT_EnsureErrorSkipsWithItsText pins the skip path: an Ensure
// error ends the cycle as the no-checkout skip, naming the error in the log.
func TestRunRebuildGT_EnsureErrorSkipsWithItsText(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	d.rebuildGTEnsureFn = func(context.Context, string, string, string) (string, error) {
		return "", errors.New("rig gastown has no bare repo")
	}
	staleCalled := false
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo {
		staleCalled = true
		return nil
	}

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("an Ensure error must settle the cycle as a skip, not defer it")
	}
	if staleCalled {
		t.Fatal("no checkout means no staleness read")
	}
	if !strings.Contains(logs.String(), "no gt source checkout") || !strings.Contains(logs.String(), "rig gastown has no bare repo") {
		t.Fatalf("log = %q, want the skip line with the Ensure error", logs.String())
	}
}
