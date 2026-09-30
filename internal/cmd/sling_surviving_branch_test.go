package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
)

// writeGastownRoutes maps the gt- prefix to the gastown rig, which is what
// survivingBranchForBead uses to find the rig's git repo.
func writeGastownRoutes(t *testing.T, townRoot string) {
	t.Helper()
	writeTestRoutes(t, townRoot, []beads.Route{{Prefix: "gt-", Path: "gastown/mayor/rig"}})
}

// Convoy and epic feeders count a resling refusal as a deferral: not a
// success, not a failed attempt, and a run of only deferrals is not an error.
func TestFeederDispatchTallyTreatsReslingRefusalAsDeferral(t *testing.T) {
	refusal := &reslingRefusal{msg: "refusing to re-sling gt-a: ..."}
	cases := []struct {
		name    string
		errs    []error
		wantErr string
	}{
		{name: "all deferred", errs: []error{refusal, fmt.Errorf("wrapped: %w", refusal)}},
		{name: "deferred and dispatched", errs: []error{refusal, nil}},
		{name: "deferred and failed", errs: []error{refusal, errors.New("spawn failed")}, wantErr: "all 1 dispatch attempts failed for convoy hq-cv-1"},
		{name: "all failed", errs: []error{errors.New("x"), errors.New("y")}, wantErr: "all 2 dispatch attempts failed for convoy hq-cv-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tally feederDispatchTally
			var err error
			out := captureStdout(t, func() {
				for i, e := range tc.errs {
					ok := tally.record(fmt.Sprintf("gt-%d", i), e)
					if ok != (e == nil) {
						t.Errorf("record(%v) = %v", e, ok)
					}
				}
				err = tally.result("convoy", "hq-cv-1")
			})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if errors.Is(tc.errs[0], errReslingRefused) && strings.Contains(out, "✗ gt-0") {
				t.Fatalf("a refusal was printed as a failure:\n%s", out)
			}
		})
	}
}

// The scheduler leaves a refused dispatch queued without recording a failure,
// so a preserved-work bead never trips the circuit breaker.
func TestCapacityDispatchDeferralRecognizesReslingRefusal(t *testing.T) {
	t.Parallel()
	refusal := &reslingRefusal{msg: "refusing to re-sling gt-a: ..."}
	if _, ok := capacityDispatchDeferral(fmt.Errorf("sling failed: %w", refusal)); !ok {
		t.Fatal("a wrapped resling refusal must be a deferral")
	}
	if _, ok := capacityDispatchDeferral(fmt.Errorf("sling failed: %w", &polecatCapacityAdmissionError{})); !ok {
		t.Fatal("a capacity admission refusal must stay a deferral")
	}
	if _, ok := capacityDispatchDeferral(errors.New("sling failed: spawn failed")); ok {
		t.Fatal("an ordinary failure must not be a deferral")
	}
}

// clearOrphanEpisodeLabels removes only the episode labels the bead carries,
// writes nothing when it carries none, and never fails the caller.
func TestClearOrphanEpisodeLabels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bd stub")
	}
	cases := []struct {
		name       string
		labels     string
		showFails  bool
		wantUpdate string // "" = no update call
	}{
		{name: "all three present", labels: `"gt:preserved-orphan","keep-me","gt:survival-unknown","gt:survival-escalated"`,
			wantUpdate: "update gt-lbl1 --remove-label=gt:preserved-orphan --remove-label=gt:survival-unknown --remove-label=gt:survival-escalated"},
		{name: "one present", labels: `"gt:survival-unknown"`, wantUpdate: "update gt-lbl1 --remove-label=gt:survival-unknown"},
		{name: "none present", labels: `"keep-me"`},
		{name: "read fails", showFails: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			binDir := filepath.Join(townRoot, "bin")
			for _, d := range []string{filepath.Join(townRoot, ".beads"), binDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(townRoot, "bd.log")
			fail := ""
			if tc.showFails {
				fail = "1"
			}
			script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
for a in "$@"; do
  case "$a" in
  show)
    [ -n "` + fail + `" ] && { echo "boom" >&2; exit 1; }
    echo '[{"id":"gt-lbl1","title":"t","status":"hooked","labels":[` + tc.labels + `]}]'
    exit 0 ;;
  update) exit 0 ;;
  esac
done
exit 0
`
			_ = writeBDStub(t, binDir, script, "")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			_ = captureStdout(t, func() { clearOrphanEpisodeLabels(townRoot, "gt-lbl1", "") })

			data, _ := os.ReadFile(logPath)
			var updates []string
			for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if i := strings.Index(l, "update "); i >= 0 {
					updates = append(updates, l[i:])
				}
			}
			if tc.wantUpdate == "" {
				if len(updates) != 0 {
					t.Fatalf("want no update, got %v", updates)
				}
				return
			}
			if len(updates) != 1 || !strings.HasPrefix(updates[0], tc.wantUpdate) {
				t.Fatalf("updates = %v, want one starting %q\nlog:\n%s", updates, tc.wantUpdate, data)
			}
		})
	}
}

// An unrouted bead is cleared in the hook write's work dir, not the town
// root: the labels live in the database the hook just wrote.
func TestClearOrphanEpisodeLabelsUsesHookWorkDir(t *testing.T) {
	townRoot := t.TempDir()
	workDir := t.TempDir()
	binDir := filepath.Join(townRoot, "bin")
	for _, d := range []string{filepath.Join(townRoot, ".beads"), filepath.Join(workDir, ".beads"), binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(townRoot, "bd-cwd.log")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
  show)
    pwd -P >> "` + logPath + `"
    echo '[{"id":"zz-lbl2","title":"t","status":"hooked","labels":["gt:preserved-orphan"]}]'
    exit 0 ;;
  update) exit 0 ;;
  esac
done
exit 0
`
	_ = writeBDStub(t, binDir, script, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_ = captureStdout(t, func() { clearOrphanEpisodeLabels(townRoot, "zz-lbl2", workDir) })

	data, _ := os.ReadFile(logPath)
	want, _ := filepath.EvalSymlinks(workDir)
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("bd show ran in %q, want hook work dir %q", got, want)
	}
}

// TestReslingSurvivingWorkGuard is the gt-3qfp guard: a dead holder's work
// that survives on a branch, or whose survival cannot be verified, refuses
// the re-sling with the shared marker the daemon's feeder matches, and names
// both ways forward. Only a verified "nothing to protect" lets it through.
func TestReslingSurvivingWorkGuard(t *testing.T) {
	t.Parallel()
	const branch = "polecat/pearl/gt-ibt8+mu72g5cz"
	cases := []struct {
		name   string
		branch string
		err    error
		want   []string // refusal fragments; nil = no refusal
	}{
		{name: "work survives", branch: branch, want: []string{"refusing to re-sling gt-ibt8", branch, "--branch " + branch, "--force"}},
		{name: "survival unknown", err: errors.New("origin unreachable"),
			want: []string{"cannot verify surviving work (origin unreachable)", "resume with --branch or override with --force"}},
		{name: "no rig repo", err: polecat.ErrNoRigRepo},
		{name: "routes to no rig", err: fmt.Errorf("gt-ibt8: %w", errBeadRoutesToNoRig)},
		{name: "nothing survives"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reslingSurvivingWorkGuardWith(func(string, string) (string, error) { return tc.branch, tc.err },
				"/town", "gt-ibt8", "gastown/polecats/pearl")
			if tc.want == nil {
				if err != nil {
					t.Fatalf("guard refused with nothing to protect: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("guard let the re-sling through")
			}
			if !strings.HasPrefix(err.Error(), dispatch.ReslingRefusalMarker+" ") {
				t.Errorf("refusal %q does not start with %q", err, dispatch.ReslingRefusalMarker)
			}
			if !errors.Is(fmt.Errorf("sling failed: %w", err), errReslingRefused) {
				t.Error("a wrapped refusal must match errReslingRefused")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q lacks %q", err, w)
				}
			}
		})
	}
}
