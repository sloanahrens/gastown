package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRoutingBdStub(t *testing.T, scriptBody string) {
	t.Helper()

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/bin/sh\n" + scriptBody
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func chdirConvoyTest(t *testing.T, dir string) {
	t.Helper()

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
}

func makeRoutingTownWorkspace(t *testing.T) (string, string) {
	t.Helper()

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	expectedWD := townRoot
	if resolved, err := filepath.EvalSymlinks(townRoot); err == nil && resolved != "" {
		expectedWD = resolved
	}
	return townRoot, expectedWD
}

// convoyCLIFixture is a convoyCLI over a temp town with an in-process bd.
// Nothing it does starts a process or reads the cwd, the environment or a
// package global.
type convoyCLIFixture struct {
	c       convoyCLI
	root    string
	bd      *inprocBD
	rec     *callsBD
	out     *bytes.Buffer
	ensured []string // beads dirs the convoy types were registered in
}

func newConvoyCLIFixture(t *testing.T, answer func(f *inprocBD, cmd string, args []string) bdAnswer) *convoyCLIFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		return answer(f, cmd, args)
	}}
	fx := &convoyCLIFixture{root: root, bd: bd, rec: &callsBD{bd: bd}, out: &bytes.Buffer{}}
	fx.c = convoyCLI{
		townRoot: func() (string, error) { return root, nil },
		bd:       fx.rec.run,
		out:      fx.out,
		warn:     io.Discard,
		entropy:  strings.NewReader("abcde"),
		sender:   func() string { return "mayor/" },
		ensureTypes: func(beadsDir string) error {
			fx.ensured = append(fx.ensured, beadsDir)
			return nil
		},
	}
	return fx
}

// assertAllPinnedToTown fails unless every bd call ran from the town root
// against the town's .beads database, whatever BEADS_DIR the caller had.
func (fx *convoyCLIFixture) assertAllPinnedToTown(t *testing.T) {
	t.Helper()
	calls := fx.rec.recorded()
	if len(calls) == 0 {
		t.Fatal("no bd calls")
	}
	townBeads := filepath.Join(fx.root, ".beads")
	for _, c := range calls {
		if c.Dir != fx.root {
			t.Errorf("bd %q ran in %q, want town root %q", c.Args, c.Dir, fx.root)
		}
		if got := callEnv(c, "BEADS_DIR"); got != townBeads {
			t.Errorf("bd %q BEADS_DIR = %q, want %q", c.Args, got, townBeads)
		}
	}
}

// TestRunConvoyList_UsesTownRootAndStripsBeadsDir: gt convoy list --json
// --all reads the convoys and their tracked issues from the town root,
// pinned to the town database.
func TestRunConvoyList_UsesTownRootAndStripsBeadsDir(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, func(f *inprocBD, cmd string, args []string) bdAnswer {
		switch cmd {
		case "list":
			if argsMention(args, "--label=gt:convoy") {
				return bdOut(`[{"id":"hq-cv-town","title":"Town convoy","status":"open","created_at":"2026-03-09T00:00:00Z","labels":["gt:convoy"]}]`)
			}
			return bdOut("[]")
		case "show":
			return bdOut(`[{"id":"hq-cv-town","title":"Town convoy","status":"open","issue_type":"convoy","dependencies":[]}]`)
		}
		return bdOut("[]")
	})

	if err := fx.c.list(convoyListOptions{json: true, all: true}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(fx.out.String(), `"id": "hq-cv-town"`) {
		t.Fatalf("expected convoy JSON output, got:\n%s", fx.out.String())
	}
	if !fx.bd.logged("list --label=gt:convoy --json --limit=0 --all --flat") {
		t.Errorf("convoys not listed with --all; bd log:\n%s", fx.bd.log())
	}
	fx.assertAllPinnedToTown(t)
}

// TestRunConvoyStatus_UsesTownRootAndStripsBeadsDir: gt convoy status <id>
// reads the convoy from the town root, pinned to the town database, and
// reports its progress.
func TestRunConvoyStatus_UsesTownRootAndStripsBeadsDir(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, func(f *inprocBD, cmd string, args []string) bdAnswer {
		if cmd == "show" {
			return bdOut(`[{"id":"hq-cv-status","title":"Status convoy","status":"open","issue_type":"convoy","created_at":"2026-03-09T00:00:00Z","labels":[],"dependencies":[]}]`)
		}
		return bdOut("[]")
	})

	if err := fx.c.status(false, []string{"hq-cv-status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := fx.out.String()
	if !strings.Contains(out, "hq-cv-status") || !strings.Contains(out, "Progress:  0/0 completed") {
		t.Fatalf("unexpected status output:\n%s", out)
	}
	fx.assertAllPinnedToTown(t)
}

// convoyWriteBD answers `bd show` of the convoy hq-cv-test and fails
// `bd dep add` for the issue failDep; every other call succeeds silently.
func convoyWriteBD(failDep string) func(f *inprocBD, cmd string, args []string) bdAnswer {
	return func(f *inprocBD, cmd string, args []string) bdAnswer {
		switch cmd {
		case "show":
			return bdOut(`[{"id":"hq-cv-test","title":"Test Convoy","status":"open","issue_type":"convoy"}]`)
		case "dep":
			if failDep != "" && argsMention(args, failDep) {
				return bdAnswer{stderr: "simulated tracking failure", code: 1}
			}
		case "create":
			return bdOut(`[{"id":"hq-cv-test"}]`)
		}
		return bdOut("")
	}
}

// TestConvoyCreate_UsesTrackingHelper: convoy create writes the convoy in the
// town database under an hq-cv-* ID drawn from its entropy, then records one
// tracks edge per issue.
func TestConvoyCreate_UsesTrackingHelper(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD(""))

	if err := fx.c.create(convoyCreateOptions{}, []string{"test-convoy", "mo-2sh.1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(fx.bd.log(), "create --type=task --id=hq-cv-pqrst --title=test-convoy") {
		t.Errorf("convoy hq-cv-pqrst not created; bd log:\n%s", fx.bd.log())
	}
	if !fx.bd.logged("dep add hq-cv-pqrst mo-2sh.1 --type=tracks") {
		t.Errorf("tracks edge not recorded; bd log:\n%s", fx.bd.log())
	}
	for _, c := range fx.rec.recorded() {
		if len(c.Args) > 0 && c.Args[0] == "create" && callEnv(c, "BEADS_DIR") != filepath.Join(fx.root, ".beads") {
			t.Errorf("create BEADS_DIR = %q, want the town database", callEnv(c, "BEADS_DIR"))
		}
	}
}

// TestConvoyCreate_RegistersTypesInTownBeadsDir is the hq-dt4 regression:
// the convoy types and statuses are registered in the town's .beads
// directory, not the workspace root, or every convoy reads as empty.
func TestConvoyCreate_RegistersTypesInTownBeadsDir(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD(""))

	if err := fx.c.create(convoyCreateOptions{}, []string{"test-convoy", "gt-abc"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := filepath.Join(fx.root, ".beads"); len(fx.ensured) != 1 || fx.ensured[0] != want {
		t.Fatalf("types registered in %v, want [%s]", fx.ensured, want)
	}
}

// TestConvoyAdd_UsesTrackingHelper: convoy add checks the convoy in the town
// database and records a tracks edge for each issue, in order.
func TestConvoyAdd_UsesTrackingHelper(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD(""))

	if err := fx.c.add([]string{"hq-cv-test", "ag-95s.1", "ag-95s.2"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	for _, want := range []string{"show hq-cv-test --json", "dep add hq-cv-test ag-95s.1 --type=tracks", "dep add hq-cv-test ag-95s.2 --type=tracks"} {
		if !fx.bd.logged(want) {
			t.Errorf("missing bd call %q; bd log:\n%s", want, fx.bd.log())
		}
	}
}

// TestConvoyAdd_ReportsOnlyIssuesActuallyAdded: an issue whose tracks edge
// fails is left out of the added count and list.
func TestConvoyAdd_ReportsOnlyIssuesActuallyAdded(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD("ag-95s.2"))

	if err := fx.c.add([]string{"hq-cv-test", "ag-95s.1", "ag-95s.2", "ag-95s.3"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := fx.out.String()
	if !strings.Contains(out, "Added 2 issue(s)") {
		t.Errorf("output should report 2 added issues, got:\n%s", out)
	}
	if !strings.Contains(out, "Issues: ag-95s.1, ag-95s.3") {
		t.Errorf("output should list the issues actually added (1 and 3), got:\n%s", out)
	}
	if strings.Contains(out, "ag-95s.2") {
		t.Errorf("output must not list the failed issue ag-95s.2, got:\n%s", out)
	}
}
