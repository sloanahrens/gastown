package cmd

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/townstate"
)

// rigsJSON is a town registry naming alpha and beta.
const rigsJSON = `{"version":1,"rigs":{"alpha":{"git_url":"x"},"beta":{"git_url":"y"}}}`

// TestTownStateMigrateLandingQueue: the queue is the ready-to-land beads the
// landing worker still acts on, in every actionable status, summed over the rigs.
// A rig whose list fails, or a registry that does not parse, fails the count.
func TestTownStateMigrateLandingQueue(t *testing.T) {
	t.Parallel()
	label := land.LabelReadyToLand
	bead := func(status string, labels ...string) *beads.Issue {
		return &beads.Issue{ID: "gt-x", Status: status, Labels: labels}
	}
	t.Run("every actionable status counts, the rest do not", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		lists := map[string][]*beads.Issue{
			"alpha": {bead("open", label), bead("hooked", label), bead("in_progress", label), bead("blocked", label)},
			"beta":  {bead("deferred", label), bead("closed", label), bead("open"), nil},
		}
		list := func(rigRoot string) ([]*beads.Issue, error) { return lists[filepath.Base(rigRoot)], nil }
		got, err := landingQueueCount(list, town)
		if err != nil || got != 4 {
			t.Fatalf("landingQueueCount = %d, %v; want 4 (open, hooked, in_progress, blocked on alpha)", got, err)
		}
	})
	t.Run("a town with no registry has an empty queue", func(t *testing.T) {
		t.Parallel()
		none := func(string) ([]*beads.Issue, error) { return nil, errors.New("must not list") }
		got, err := landingQueueCount(none, t.TempDir())
		if err != nil || got != 0 {
			t.Fatalf("landingQueueCount with no registry = %d, %v; want 0", got, err)
		}
	})
	t.Run("a registry that does not parse is an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", "{")
		if _, err := landingQueueCount(func(string) ([]*beads.Issue, error) { return nil, nil }, town); err == nil {
			t.Fatal("landingQueueCount with a broken registry = nil, want an error")
		}
	})
	t.Run("a rig whose list fails fails the whole queue", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		failing := func(rigRoot string) ([]*beads.Issue, error) {
			if filepath.Base(rigRoot) == "beta" {
				return nil, errors.New("bd exited 1")
			}
			return nil, nil
		}
		got, err := landingQueueCount(failing, town)
		if err == nil {
			t.Fatalf("landingQueueCount = %d, nil; want the failed rig refused", got)
		}
		if !strings.Contains(err.Error(), "rig beta") || strings.Contains(err.Error(), "bd exited 1") {
			t.Errorf("the refusal names %q; want the rig named and the list's output withheld", err)
		}
	})
}

// TestTownStateMigrateListWithinIsBounded: a list that does not answer by the
// deadline is an error, never an empty answer.
func TestTownStateMigrateListWithinIsBounded(t *testing.T) {
	t.Parallel()
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	_, err := listWithin(time.Millisecond, func() ([]*beads.Issue, error) {
		<-hang
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("listWithin of a hung list = %v, want a deadline error", err)
	}
	got, err := listWithin(time.Second, func() ([]*beads.Issue, error) { return []*beads.Issue{{ID: "gt-1"}}, nil })
	if err != nil || len(got) != 1 {
		t.Fatalf("listWithin of a prompt list = %v, %v; want its answer", got, err)
	}
}

// TestTownStateMigrateLandingProbes: the candidate probe reads the landing worker's
// own work root from the town's patrol settings, and the post-land probe reads each
// rig's red-main state. An unreadable state is an error the caller counts unmet.
func TestTownStateMigrateLandingProbes(t *testing.T) {
	t.Parallel()
	t.Run("a landing directory in the work root is a run in flight", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		base := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/daemon.json", `{"patrols":{"landing_worker":{"work_root":"`+base+`"}}}`)
		if err := os.MkdirAll(filepath.Join(base, "beta", "land-123"), 0o700); err != nil {
			t.Fatal(err)
		}
		busy, err := landingCandidates(town)
		if err != nil || len(busy) != 1 || busy[0] != "beta" {
			t.Fatalf("landingCandidates = %v, %v; want beta in flight", busy, err)
		}
	})
	t.Run("an empty work root has no run in flight", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		base := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/daemon.json", `{"patrols":{"landing_worker":{"work_root":"`+base+`"}}}`)
		busy, err := landingCandidates(town)
		if err != nil || len(busy) != 0 {
			t.Fatalf("landingCandidates = %v, %v; want none in flight", busy, err)
		}
	})
	t.Run("post-land verdicts still pending name the rig and its newest commit", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		writeTownStateFileContent(t, town, ".runtime/red-main/alpha.json",
			`{"post_land_pending":[{"land":{"Commit":"aaaaaaaaaaaaaaaa"}},{"land":{"Commit":"0123456789abcdef"}}]}`)
		pending, err := postLandPending(town)
		if err != nil || len(pending) != 1 || pending[0] != "alpha (newest verdict at 0123456789ab)" {
			t.Fatalf("postLandPending = %v, %v; want alpha at the newest commit", pending, err)
		}
	})
	t.Run("a red-main state that is not JSON is an error, not an empty state", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeTownStateFileContent(t, town, "mayor/rigs.json", rigsJSON)
		writeTownStateFileContent(t, town, ".runtime/red-main/alpha.json", "{")
		if pending, err := postLandPending(town); err == nil {
			t.Fatalf("postLandPending = %v, nil; want the unreadable state refused", pending)
		}
	})
}

// TestTownStateMigratePlanReportsAnUnreadableLandingQueue: the plan shows a queue
// that cannot be read as unmet, with the error, and does not show it as met.
func TestTownStateMigratePlanReportsAnUnreadableLandingQueue(t *testing.T) {
	t.Parallel()
	env, _, out := newMigrateEnv(t, migrateTown(t))
	env.probes.landingQueue = func(string) (int, error) { return 0, errors.New("bd list exited 1") }
	if err := runTownStateMigratePlanIn(env); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !strings.Contains(out.String(), "unmet: landing queue unreadable, so a landing run in flight is not ruled out: bd list exited 1") {
		t.Errorf("plan does not show the unreadable queue as unmet:\n%s", out.String())
	}
	if strings.Contains(out.String(), "\n  met: landing queue") {
		t.Errorf("plan shows an unreadable queue as met:\n%s", out.String())
	}
}

// TestTownStateMigrateCutoverNamesEachLandingProbe: the cut-over's landing probes
// each report met or unmet by name, and an unreadable one is unmet with its error.
func TestTownStateMigrateCutoverNamesEachLandingProbe(t *testing.T) {
	t.Parallel()
	env, _, _ := newMigrateEnv(t, migrateTown(t))
	env.probes = readyProbes()
	env.probes.landingCandidates = func(string) ([]string, error) { return nil, errors.New("work root is a file") }
	checks := cutoverChecks(env)
	var unmet []string
	for _, c := range checks {
		if !c.Met {
			unmet = append(unmet, c.Text)
		}
	}
	if len(unmet) != 1 || !strings.HasPrefix(unmet[0], "landing candidate unreadable") || !strings.Contains(unmet[0], "work root is a file") {
		t.Fatalf("unmet checks = %q, want the candidate probe unmet with its error", unmet)
	}
	if n := len(checks); n != 5 {
		t.Errorf("cut-over checks = %d, want five (hold, container, queue, candidate, post-land)", n)
	}
}

// TestTownStateMigrateHelperRunIsBounded: every helper run carries a deadline, so
// a hung helper is stopped rather than holding the copy or the verify.
func TestTownStateMigrateHelperRunIsBounded(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	eng := dockerEngine{image: "gt-daemon:test", run: fd.run}
	if err := eng.Run("gt-state", townstate.ListArgv, nil, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fd.deadline {
		t.Fatal("a helper run has no deadline; a hung helper would hold the command")
	}
	if migrateHelperTimeout != 10*time.Minute {
		t.Errorf("migrateHelperTimeout = %s, want ten minutes", migrateHelperTimeout)
	}
}

// TestTownStateMigrateHelperForwardsBeadsHostByName: the Dolt host reaches the
// helper through the docker client's environment, and never on the command line.
func TestTownStateMigrateHelperForwardsBeadsHostByName(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	eng := dockerEngine{image: "gt-daemon:test", run: fd.run, beadsHost: "gt-dolt"}
	if err := eng.Run("gt-state", townstate.BeadsReadArgv, nil, io.Discard); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := strings.Join(fd.calls[0], " ")
	if !strings.Contains(args, "-e BEADS_DOLT_SERVER_HOST") || strings.Contains(args, "gt-dolt") {
		t.Errorf("helper argv = %s, want the variable forwarded by name only", args)
	}
	if len(fd.envs[0]) != 1 || fd.envs[0][0] != "BEADS_DOLT_SERVER_HOST=gt-dolt" {
		t.Errorf("docker client env = %v, want the host set there", fd.envs[0])
	}
}

// TestTownStateMigrateResolveBeadsHost: the operator's shell wins, else the town's
// dolt.container_host; a town naming neither has no host to forward, and an
// unreadable town file is an error.
func TestTownStateMigrateResolveBeadsHost(t *testing.T) {
	t.Parallel()
	withContainerHost := t.TempDir()
	writeTownStateFileContent(t, withContainerHost, "mayor/town.json", `{"name":"test","dolt":{"host":"127.0.0.1","port":3307,"container_host":"host.docker.internal"}}`)
	withoutHost := t.TempDir()
	writeTownStateFileContent(t, withoutHost, "mayor/town.json", `{"name":"test","dolt":{"host":"127.0.0.1","port":3307}}`)
	broken := t.TempDir()
	writeTownStateFileContent(t, broken, "mayor/town.json", "{")

	cases := []struct {
		name, town, shell, wantHost, wantSource string
		wantErr                                 bool
	}{
		{name: "the shell wins", town: withContainerHost, shell: " gt-dolt ", wantHost: "gt-dolt", wantSource: "the operator's shell"},
		{name: "the town's container host", town: withContainerHost, wantHost: "host.docker.internal", wantSource: "the town's dolt.container_host"},
		{name: "a town with no container host", town: withoutHost},
		{name: "no town file", town: t.TempDir()},
		{name: "an unreadable town file", town: broken, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, source, err := resolveBeadsHost(tc.town, tc.shell)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveBeadsHost error = %v, want error %v", err, tc.wantErr)
			}
			if host != tc.wantHost || source != tc.wantSource {
				t.Errorf("resolveBeadsHost = %q, %q; want %q, %q", host, source, tc.wantHost, tc.wantSource)
			}
		})
	}
}

// TestTownStateMigrateBeadsReadIsNotForwardedWithoutAHost: a town and a shell that
// name no Dolt host cannot prove the copy serves bd, so the copy says not forwarded
// and exits non-zero; it never prints an ok read.
func TestTownStateMigrateBeadsReadIsNotForwardedWithoutAHost(t *testing.T) {
	t.Parallel()
	env, fv, out := newMigrateEnv(t, migrateTown(t))
	env.beadsHost = ""
	err := runTownStateMigrateCopyIn(env)
	if err == nil {
		t.Fatal("copy with no Dolt host = nil, want a non-zero exit")
	}
	if !strings.Contains(out.String(), "bd read through the volume copy: not forwarded") || strings.Contains(out.String(), "bd read through the volume copy: ok") {
		t.Errorf("the copy did not say the bd read was not forwarded:\n%s", out.String())
	}
	for _, call := range fv.calls {
		if call == strings.Join(townstate.BeadsReadArgv, " ") {
			t.Errorf("the bd read ran with no host to reach: %v", fv.calls)
		}
	}
}

// TestTownStateMigrateShellWord: a word of safe characters prints bare; any other
// word is one single-quoted word, with each embedded quote closed, escaped and
// reopened.
func TestTownStateMigrateShellWord(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"gt-state:/state":       "gt-state:/state",
		"/town/with space/x":    "'/town/with space/x'",
		"it's":                  `'it'\''s'`,
		"":                      "''",
		"gt-daemon@sha256:abcd": "gt-daemon@sha256:abcd",
	}
	for in, want := range cases {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTownStateMigrateRollbackPlanQuotesEveryPath: a town, compose file or volume
// path with a space prints as one shell word, so each printed command runs.
func TestTownStateMigrateRollbackPlanQuotesEveryPath(t *testing.T) {
	t.Parallel()
	env, _, out := newMigrateEnv(t, migrateTown(t))
	env.town = "/tmp/my town"
	env.composePath = "/tmp/my town/compose.yml"
	env.savedCompose = "/tmp/my town/compose.yml.pre-cutover"
	if err := runTownStateMigrateRollbackPlanIn(env); err != nil {
		t.Fatalf("rollback plan: %v", err)
	}
	for _, want := range []string{
		"gt town state import --town '/tmp/my town' --merge-newer",
		"cp '/tmp/my town/compose.yml.pre-cutover' '/tmp/my town/compose.yml'",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rollback plan lacks %q:\n%s", want, out.String())
		}
	}
}
