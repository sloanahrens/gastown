package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/version"
)

// nonBinaryStale is a staleness reading a diff can actually be taken over: both
// commits are set, so rebuildGTNonBinaryRange has a range to read.
func nonBinaryStale(behind int, repoCommit string) *version.StaleBinaryInfo {
	info := staleInfo(behind)
	info.RepoCommit = repoCommit
	return info
}

// logSinkLogger points a daemon's log at a buffer so a test can read the lines
// a cycle wrote.
func logSinkLogger(d *Daemon, buf *bytes.Buffer) {
	d.logger = log.New(buf, "", 0)
}

// TestRebuildGTCycle_SkipsADocsOnlyLanding is the case the slice exists for: a
// landing that changed only documentation, markdown, test sources or testdata
// installs nothing and costs no gate slot and no daemon restart (gt-3qmv4.3).
func TestRebuildGTCycle_SkipsADocsOnlyLanding(t *testing.T) {
	t.Parallel()
	d, rec := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return nonBinaryStale(3, "cafecafe") }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	var logs bytes.Buffer
	logSinkLogger(d, &logs)
	// A starvation block is running when the docs-only landing lands: nothing
	// in the range needs installing, so the clock must close rather than age.
	d.rebuildGTBlock.open(d.clk().Now().Add(-31 * time.Minute))
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "diff"):
			return cliReply{stdout: strings.Join([]string{
				"README.md",
				"docs/design/architecture.md",
				"internal/daemon/rebuild_gt_test.go",
				"internal/daemon/testdata/fixture.txt",
			}, "\n") + "\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a docs-only skip reached a verdict and must hold the interval")
	}
	if installs := installCalls(cli); len(installs) != 0 {
		t.Errorf("a docs-only landing was installed: %v", installs)
	}
	if _, open := d.rebuildGTBlock.openFor(d.clk().Now()); open {
		t.Error("a docs-only skip left the starvation clock running")
	}
	want := "rebuild_gt: skip: only non-binary paths changed (3 commits)"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log does not carry %q:\n%s", want, logs.String())
	}
	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a docs-only skip escalated: %+v", esc)
	}
}

// TestRebuildGTCycle_InstallsWhenAMixedLandingChangesTheBinary pins the other
// half: one path outside the non-binary shapes is a build, docs beside it or
// not (gt-3qmv4.3).
func TestRebuildGTCycle_InstallsWhenAMixedLandingChangesTheBinary(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return nonBinaryStale(3, "cafecafe") }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "diff"):
			return cliReply{stdout: "docs/design/architecture.md\ninternal/daemon/rebuild_gt.go\n"}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		case gitSub(c, "log"):
			return cliReply{stdout: "abc1234 the commit that was inert\n"}
		}
		if c.name == "bash" {
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 cafecafe -\n"}
		}
		return cliReply{}
	})

	if settled := d.runRebuildGT(); !settled {
		t.Fatal("a completed install holds the interval")
	}
	if installs := installCalls(cli); len(installs) != 1 {
		t.Errorf("a landing that changed the binary was not installed: %v", installs)
	}
}

// TestRebuildGTCycle_InstallsWhenAnEmbedSourceChanged pins the carve-out: the
// go:embed directories carry files into the binary, so a change under one is
// binary-affecting even when the file is markdown — internal/templates embeds
// polecat-CLAUDE.md and commands/bodies/*.md (gt-3qmv4.3).
func TestRebuildGTCycle_InstallsWhenAnEmbedSourceChanged(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"internal/templates/polecat-CLAUDE.md",
		"internal/templates/commands/bodies/foo.md",
		"internal/cmdtree/bd-command-tree.json",
		"internal/config/roles/worker.toml",
		"internal/formula/formulas/polecat.formula.toml",
		"scripts/install-gt.sh",
		"plugins/tool-updater/run.sh",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			d, _ := rebuildGTTown(t)
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return nonBinaryStale(3, "cafecafe") }
			d.rebuildGTGateFn = func() (string, bool) { return "", false }
			cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
				switch {
				case gitSub(c, "branch"):
					return cliReply{stdout: "main\n"}
				case gitSub(c, "diff"):
					return cliReply{stdout: path + "\n"}
				case gitSub(c, "rev-parse"):
					return cliReply{stdout: "abc1234567890\n"}
				}
				if c.name == "bash" {
					return cliReply{stdout: "install-gt: RESULT installed abc1234567890 cafecafe -\n"}
				}
				return cliReply{}
			})

			d.runRebuildGT()
			if installs := installCalls(cli); len(installs) != 1 {
				t.Errorf("a change to %s did not install: %v", path, installs)
			}
		})
	}
}

// TestRebuildGTCycle_InstallsWhenTheDiffIsUnreadable pins the fail-safe: an
// unreadable diff is not evidence that nothing needs building, so the cycle
// installs as it did before this check existed (gt-3qmv4.3).
func TestRebuildGTCycle_InstallsWhenTheDiffIsUnreadable(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return nonBinaryStale(3, "cafecafe") }
	d.rebuildGTGateFn = func() (string, bool) { return "", false }
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		switch {
		case gitSub(c, "branch"):
			return cliReply{stdout: "main\n"}
		case gitSub(c, "diff"):
			return cliReply{stdout: "fatal: bad object\n", stderr: "fatal: bad object", code: 128}
		case gitSub(c, "rev-parse"):
			return cliReply{stdout: "abc1234567890\n"}
		}
		if c.name == "bash" {
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 cafecafe -\n"}
		}
		return cliReply{}
	})

	d.runRebuildGT()
	if installs := installCalls(cli); len(installs) != 1 {
		t.Errorf("an unreadable diff did not install: %v", installs)
	}
}

// TestRebuildGTNonBinaryPath pins the shape table directly, including the
// embed carve-out and the shapes that look exempt but are not.
func TestRebuildGTNonBinaryPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path      string
		nonBinary bool
	}{
		{"docs/specs/foo.md", true},
		{"docs/design/architecture.md", true},
		{"README.md", true},
		{"internal/daemon/rebuild_gt_test.go", true},
		{"internal/daemon/testdata/fixture.txt", true},
		{"testdata/fixture.txt", true},
		// Inside a go:embed source directory, even documentation is carried
		// into the binary.
		{"internal/templates/polecat-CLAUDE.md", false},
		{"internal/templates/commands/bodies/foo.md", false},
		// A test source is never compiled into the binary, so it stays exempt.
		{"internal/templates/townroot_test.go", true},
		// The embed's data is binary-affecting regardless of shape.
		{"internal/cmdtree/bd-command-tree.json", false},
		{"internal/config/roles/worker.toml", false},
		{"internal/formula/formulas/polecat.formula.toml", false},
		// Ordinary source, scripts and plugins. A markdown file in one of those
		// trees is still markdown: it is not compiled into the binary.
		{"internal/daemon/rebuild_gt.go", false},
		{"scripts/install-gt.sh", false},
		{"plugins/tool-updater/run.sh", false},
		{"plugins/README.md", true},
		{"main.go", false},
		{"AGENTS.md", true},
	}
	for _, tc := range tests {
		if got := rebuildGTNonBinaryPath(tc.path); got != tc.nonBinary {
			t.Errorf("rebuildGTNonBinaryPath(%q) = %v, want %v", tc.path, got, tc.nonBinary)
		}
	}
}

// TestRebuildGTDrift_IgnoresANonBinaryBacklog pins the alarm's count: a backlog
// that changed nothing the binary is built from is not drift, however long it
// is (gt-3qmv4.3).
func TestRebuildGTDrift_IgnoresANonBinaryBacklog(t *testing.T) {
	t.Parallel()
	d, rec := daemonWithRecorder(t)
	withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "diff") {
			return cliReply{stdout: "README.md\ndocs/design/architecture.md\n"}
		}
		// A count git could not subdivide: the path reading decides.
		return cliReply{}
	})

	d.rebuildGTDrift(d.config.TownRoot, nonBinaryStale(rebuildGTMaxCommitsBehind+5, "cafecafe"))

	if esc := rec.Escalations(); len(esc) != 0 {
		t.Errorf("a docs-only backlog escalated: %+v", esc)
	}
}

// TestRebuildGTDrift_CountsOnlyBinaryAffectingCommits pins the count: the
// ceiling applies to the binary-affecting commits, not the raw ones.
func TestRebuildGTDrift_CountsOnlyBinaryAffectingCommits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		count     string
		wantDrift bool
	}{
		{"under the ceiling", "3\n", false},
		{"over the ceiling", "21\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, rec := daemonWithRecorder(t)
			withRebuildGTCli(t, d, func(c cliCall) cliReply {
				if gitSub(c, "rev-list") {
					return cliReply{stdout: tc.count}
				}
				return cliReply{}
			})

			d.rebuildGTDrift(d.config.TownRoot, nonBinaryStale(40, "cafecafe"))

			esc := rec.Escalations()
			if tc.wantDrift {
				if len(esc) != 1 || esc[0].Escalation.Fingerprint != alertKeyRebuildGTDrift {
					t.Fatalf("escalations = %+v, want one under %s", esc, alertKeyRebuildGTDrift)
				}
				if !strings.Contains(esc[0].Escalation.Reason, "21 commits behind") {
					t.Errorf("reason = %q, want the binary-affecting count", esc[0].Escalation.Reason)
				}
				return
			}
			if len(esc) != 0 {
				t.Errorf("3 binary-affecting commits out of 40 escalated: %+v", esc)
			}
		})
	}
}
