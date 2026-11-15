package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// seatGitForLogTest gives d the git handle a seat of the named kind gets: a host
// seat's stub is opened for its host worktree and labeled by overrunSeatGit, and
// a sandbox seat's stub is labeled by labelOverrunSandboxGit, the same wrapper
// overrunSandboxGit puts on the in-sandbox handle (the routing test
// TestOverrunGuardLogSeatGitLabelNamesTheWorktreeItPicked proves the real
// sandbox seat gets that label).
func seatGitForLogTest(t *testing.T, d *Daemon, sandbox bool, stub *pushStubGit) {
	t.Helper()
	if !sandbox {
		seedHostWorktreeStub(t, d)
		d.openGitFn = func(string) daemonGit { return stub }
		return
	}
	d.overrunSeatGitFn = func(overrunSeat) (daemonGit, error) {
		return labelOverrunSandboxGit(stub), nil
	}
}

// TestOverrunGuardLog_NonFastForwardNamesTheWorktreeExamined is the reworded
// line from the 2026-11-14 incidents: the tip it reads may be the host stub's
// rather than the seat's work, so the line says which worktree it examined
// (gt-1mrhlg).
func TestOverrunGuardLog_NonFastForwardNamesTheWorktreeExamined(t *testing.T) {
	t.Parallel()
	const tip = "abc123"
	tests := []struct {
		name    string
		sandbox bool
		want    string
	}{
		{name: "host seat", want: "treating the push as done (in the host worktree)"},
		{name: "sandbox seat", sandbox: true, want: "treating the push as done (in the sandbox clone)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, buf := newOverrunDaemon(t, false)
			seatGitForLogTest(t, d, tc.sandbox, &pushStubGit{
				branch:     stopSeat.Branch,
				sha:        tip,
				pushErr:    overrunPushRejected(),
				originRefs: []git.RemoteRef{{Hash: "9f8e7d6", Name: "refs/heads/" + stopSeat.Branch}},
				ancestors:  map[string]bool{tip + " 9f8e7d6": true},
			})

			if err := d.overrunPushBranch(stopSeat); err != nil {
				t.Fatalf("push = %v, want the refused push treated as done", err)
			}
			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("log = %q, want a line containing %q", got, tc.want)
			}
		})
	}
}

// TestOverrunGuardLog_OriginDoesNotHoldNamesTheWorktreeExamined: the second
// incident line names the worktree whose tip it read, so a sandbox seat's stub
// is not reported as if it were the seat's work (gt-1mrhlg).
func TestOverrunGuardLog_OriginDoesNotHoldNamesTheWorktreeExamined(t *testing.T) {
	t.Parallel()
	const tip = "abc123"
	tests := []struct {
		name    string
		sandbox bool
		want    string
	}{
		{name: "host seat", want: "worktree left as it is (in the host worktree)"},
		{name: "sandbox seat", sandbox: true, want: "worktree left as it is (in the sandbox clone)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, buf := newOverrunDaemon(t, false)
			seatGitForLogTest(t, d, tc.sandbox, &pushStubGit{
				branch: stopSeat.Branch,
				sha:    tip,
				revs:   map[string]string{"HEAD": tip},
			})

			if err := d.overrunResetWorktree(stopSeat); err != nil {
				t.Fatalf("reset = %v, want the worktree left as it is", err)
			}
			if got := buf.String(); !strings.Contains(got, tc.want) {
				t.Errorf("log = %q, want a line containing %q", got, tc.want)
			}
		})
	}
}

// TestOverrunGuardLogUnlabeledHandleIsNotTheHostWorktree: a handle that carries
// no label is named as unlabeled, never as the host worktree it may not be
// (gt-0bt513).
func TestOverrunGuardLogUnlabeledHandleIsNotTheHostWorktree(t *testing.T) {
	t.Parallel()
	const want = "unlabeled worktree"
	if got := overrunWorktreeLabel(&pushStubGit{}); got != want {
		t.Errorf("label of an unlabeled handle = %q, want %q", got, want)
	}
}

// TestOverrunGuardLogLabeledHandleRefusesAGitDirItCannotRead: a labeled handle
// over a handle that cannot read its git directory refuses with an error, never
// an empty directory a caller would act on (gt-0bt513).
func TestOverrunGuardLogLabeledHandleRefusesAGitDirItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		label func(daemonGit) daemonGit
	}{
		{name: "host handle", label: labelOverrunHostGit},
		{name: "sandbox handle", label: labelOverrunSandboxGit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := hostGitDir(tc.label(&pushStubGit{}))
			if err == nil {
				t.Errorf("git directory = %q, want an error for a handle that cannot read its git directory", dir)
			}
		})
	}
}

// TestOverrunGuardLogSeatGitLabelNamesTheWorktreeItPicked pins where the label
// comes from: the same seam that picks the handle. A sandbox seat's handle is
// the in-sandbox one and is labeled the sandbox clone; a host seat's handle is
// the host worktree (gt-1mrhlg). The name carries OverrunGuardLog so the stated
// gate (-run OverrunGuardLog) selects it (gt-0bt513).
func TestOverrunGuardLogSeatGitLabelNamesTheWorktreeItPicked(t *testing.T) {
	t.Parallel()
	t.Run("host seat", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		seedHostWorktreeStub(t, d)
		d.openGitFn = func(string) daemonGit { return &pushStubGit{} }

		g, err := d.overrunSeatGit(stopSeat)
		if err != nil {
			t.Fatalf("overrunSeatGit = %v, want the host worktree's handle", err)
		}
		if got := overrunWorktreeLabel(g); got != overrunHostWorktreeLabel {
			t.Errorf("label = %q, want %q", got, overrunHostWorktreeLabel)
		}
	})
	t.Run("sandbox seat", func(t *testing.T) {
		t.Parallel()
		const clone = "/sandbox/gt/gastown/polecats/capable/gastown"
		f := newOverrunSeatGitFixture(t, overrunStopSandboxRecord(clone), nil, "")

		g, err := f.d.overrunSeatGit(stopSeat)
		if err != nil {
			t.Fatalf("overrunSeatGit = %v, want the sandbox clone's handle", err)
		}
		if got := overrunWorktreeLabel(g); got != overrunSandboxCloneLabel {
			t.Errorf("label = %q, want %q", got, overrunSandboxCloneLabel)
		}
	})
}

// TestOverrunGuardLog_LinesNamingATipOrWorktreeNameTheWorktree is the grep-style
// guard: every log line in overrun_guard_stop.go that mentions a tip or a
// worktree prints the label of the worktree it examined, or is one of the lines
// that name a worktree as absent or kept, which examined none (gt-1mrhlg).
func TestOverrunGuardLog_LinesNamingATipOrWorktreeNameTheWorktree(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src := filepath.Join(filepath.Dir(thisFile), "overrun_guard_stop.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	violations, scanned := overrunLogLinesWithoutAWorktreeLabel(fset, file)

	// A walk that found no log call would report a clean file forever.
	if scanned == 0 {
		t.Fatalf("no d.logger.Printf call found in %s — the guard would pass vacuously", src)
	}
	if len(violations) > 0 {
		t.Errorf("a log line naming a tip or a worktree does not say which worktree it examined (gt-1mrhlg).\n"+
			"Print overrunWorktreeLabel(g) in it, or list it in overrunLogLinesNamingNoWorktree with its reason:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestOverrunGuardLogScannerFlagsAnUnlabeledLine pins the scanner on a planted
// file: a line naming a tip or a worktree without the label is a violation, and
// a labeled one, a line naming neither, and an exempt one are not. The name
// carries OverrunGuardLog so the stated gate selects it (gt-0bt513).
func TestOverrunGuardLogScannerFlagsAnUnlabeledLine(t *testing.T) {
	t.Parallel()
	const src = `package daemon

func f(d *Daemon, g daemonGit, seat overrunSeat) {
	d.logger.Printf("overrun_guard: tip %s adds nothing origin lacks", seat.Bead)
	d.logger.Printf("overrun_guard: tip %s adds nothing origin lacks (in the %s)", seat.Bead, overrunWorktreeLabel(g))
	d.logger.Printf("overrun_guard: %s/%s: no worktree to push", seat.Rig, seat.Polecat)
	d.logger.Printf("overrun_guard: %s: bead handed back", seat.Rig)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "planted.go", src, 0)
	if err != nil {
		t.Fatalf("parse planted source: %v", err)
	}

	violations, scanned := overrunLogLinesWithoutAWorktreeLabel(fset, file)

	if scanned != 4 {
		t.Errorf("scanned %d log calls, want 4", scanned)
	}
	if len(violations) != 1 || !strings.Contains(violations[0], "planted.go:4") {
		t.Errorf("violations = %q, want only planted.go:4, the unlabeled tip line", violations)
	}
}

// overrunLogLinesNamingNoWorktree holds the log lines that mention a tip or a
// worktree without having examined one, keyed by the line's format, with the
// reason it names none. A line here needs no label; every other line that
// mentions a tip or a worktree must print overrunWorktreeLabel(g).
var overrunLogLinesNamingNoWorktree = map[string]string{
	"overrun_guard: stop %s on %s waits: late check of the worktree failed, retrying next tick (%d of %d), sandbox kept: %v":          "the late check failed before a handle reached a worktree, so no worktree was examined",
	"overrun_guard: stop %s on %s: late check of the worktree failed after %d attempts, sandbox kept: %v":                             "the late check gave up before a handle reached a worktree, so no worktree was examined",
	"overrun_guard: stopped %s on %s after %s; bead handed back with a note (branch %s and worktree kept, seat left paused)":          "says the worktree was kept, not that one was examined",
	"overrun_guard: %s/%s: no worktree to checkpoint":                                                                                 "no worktree was resolved, so none was examined",
	"overrun_guard: %s/%s: no worktree to push":                                                                                       "no worktree was resolved, so none was examined",
	"overrun_guard: %s/%s: no worktree to read a cleanup status from":                                                                 "no worktree was resolved, so none was examined",
	"overrun_guard: stop %s on %s: sandbox %s diverged from origin, sandbox and worktree kept; handing the bead back with a note: %v": "says the worktree was kept, not that one was examined",
	"overrun_guard: WARNING: %s: worktree not reset after the push of %s: %v":                                                         "reports the reset that failed, not a worktree it examined; the reset's own lines name the worktree",
}

// overrunLogLinesWithoutAWorktreeLabel walks file for every d.logger.Printf
// call and names each whose message mentions a tip or a worktree and does not
// print overrunWorktreeLabel(...) in its arguments, unless the message is in
// overrunLogLinesNamingNoWorktree. It returns the violations as
// "file:line: message" and the number of log calls it scanned, so a walk that
// matched nothing cannot pass as clean.
func overrunLogLinesWithoutAWorktreeLabel(fset *token.FileSet, file *ast.File) (violations []string, scanned int) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isLoggerPrintf(call.Fun) || len(call.Args) == 0 {
			return true
		}
		scanned++
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			pos := fset.Position(call.Pos())
			violations = append(violations, pos.String()+": the message is not a literal, so its text cannot be checked")
			return true
		}
		format := strings.Trim(lit.Value, `"`)
		lower := strings.ToLower(format)
		if !strings.Contains(lower, "tip") && !strings.Contains(lower, "worktree") {
			return true
		}
		if _, exempt := overrunLogLinesNamingNoWorktree[format]; exempt {
			return true
		}
		for _, arg := range call.Args[1:] {
			if isCallTo(arg, "overrunWorktreeLabel") {
				return true
			}
		}
		pos := fset.Position(call.Pos())
		violations = append(violations, pos.String()+": "+format)
		return true
	})
	return violations, scanned
}

// isLoggerPrintf reports whether fun is a Printf method call on a logger
// field.
func isLoggerPrintf(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Printf" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "logger"
}

// isCallTo reports whether arg is a call to the plain function name.
func isCallTo(arg ast.Expr, name string) bool {
	call, ok := arg.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == name
}
