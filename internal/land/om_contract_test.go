//go:build !windows

package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file pins the whole interface gastown depends on om for: the argv it
// runs, the exit codes it reads, the verdict JSON it parses and the version
// line it records. The other review tests answer a runFunc; these run the real
// process runner against a fake om, a /bin/sh script, so an om change that
// breaks gastown fails here and not in a landing (gt-q52sv).
//
// The script uses only shell builtins and absolute paths: the unit tier
// refuses a tool started by name (docs/testing.md, "The rules").

// contractOMSHA is what the fake's `om version` prints after "om version".
const contractOMSHA = "3f9c2ab"

// helpBoth is an om whose `review --help` lists both optional flags.
const helpBoth = "Usage: om review [flags]\n  -C dir\n  --base ref\n  --head ref\n  --out file\n  --bead id   work bead under review\n  --criteria-file path   acceptance criteria\n"

// fakeOMSpec is one fake om: what its help says, what the review writes and
// how it exits.
type fakeOMSpec struct {
	help string // printed to stderr by `review --help`, which exits 2 like om
	// verdict is written to the --out file unless silent.
	verdict string
	silent  bool // the review writes nothing to --out
	exit    int  // the review's exit code
	hang    bool // the review blocks instead of answering
}

// fakeOMCall is one invocation of the fake om, its argv without the program.
type fakeOMCall []string

// fakeOMRun is a fake om on disk and what it recorded.
type fakeOMRun struct {
	path     string
	log      string // argv of every call, one per line, calls separated by "--"
	copyOf   string // the --criteria-file's content as the review saw it
	outDir   string // OutDir handed to the reviewer
	reviewer OMReviewer
	warn     bytes.Buffer // the reviewer's warning lines; why() quotes them
}

// shQuote single-quotes s for /bin/sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newFakeOM writes the fake om script for spec to a temp dir and returns it
// with a reviewer that runs it through the real process runner.
func newFakeOM(t *testing.T, spec fakeOMSpec) *fakeOMRun {
	t.Helper()
	dir := t.TempDir()
	f := &fakeOMRun{
		path:   filepath.Join(dir, "om"),
		log:    filepath.Join(dir, "argv.log"),
		copyOf: filepath.Join(dir, "criteria.copy"),
		outDir: filepath.Join(t.TempDir(), "verdicts"),
	}
	var b strings.Builder
	//testpolicy:allow no-exec-files — the test is the process boundary: the real runner execs a fake om script and gastown reads its argv, exit code and verdict file
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "log=%s\n", shQuote(f.log))
	b.WriteString("{ printf '%s\\n' '--'; for a in \"$@\"; do printf '%s\\n' \"$a\"; done; } >> \"$log\"\n")
	b.WriteString("case \"$1\" in\n")
	fmt.Fprintf(&b, "version) printf 'om version %%s\\n' %s; exit 0 ;;\n", shQuote(contractOMSHA))
	b.WriteString("review) ;;\n")
	b.WriteString("*) echo \"om: unknown command $1\" >&2; exit 2 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("if [ \"$2\" = \"--help\" ]; then\n")
	fmt.Fprintf(&b, "  printf '%%s' %s >&2\n  exit 2\nfi\n", shQuote(spec.help))
	b.WriteString("out=; crit=\n")
	b.WriteString("while [ $# -gt 0 ]; do\n")
	b.WriteString("  case \"$1\" in --out) out=$2; shift ;; --criteria-file) crit=$2; shift ;; esac\n")
	b.WriteString("  shift\ndone\n")
	fmt.Fprintf(&b, "if [ -n \"$crit\" ]; then\n  while IFS= read -r l || [ -n \"$l\" ]; do printf '%%s\\n' \"$l\"; done < \"$crit\" > %s\nfi\n", shQuote(f.copyOf))
	if spec.hang {
		b.WriteString("exec /bin/sleep 1\n")
	}
	if !spec.silent {
		fmt.Fprintf(&b, "printf '%%s' %s > \"$out\"\n", shQuote(spec.verdict))
	}
	b.WriteString("echo 'om: reviewing'\n")
	fmt.Fprintf(&b, "exit %d\n", spec.exit)
	//testpolicy:allow no-exec-files — the fake om must be executable for the real runner to start it
	if err := os.WriteFile(f.path, []byte(b.String()), 0o755); err != nil { //nolint:gosec // G306: the fake om must be executable
		t.Fatal(err)
	}
	f.reviewer = OMReviewer{Path: f.path, OutDir: f.outDir}
	return f
}

// logf is the reviewer's warning sink for this fake, the Logf contractInput
// hands ReviewInput: an assertion on the recorded calls quotes it (why).
func (f *fakeOMRun) logf(format string, args ...any) {
	fmt.Fprintf(&f.warn, format+"\n", args...)
}

// why explains a call the fake om never recorded, for a failure message. A
// probe leaves no trace either because the reviewer warned about it -- a run
// error, a timeout, a busy executable -- or because omHelpProbe answered from
// its process-wide cache without running anything; the two read alike without
// this, and naming one is the whole point of the diagnostic (gt-0pt32).
func (f *fakeOMRun) why() string {
	if s := strings.TrimSpace(f.warn.String()); s != "" {
		return fmt.Sprintf("; the reviewer logged: %s", s)
	}
	if id, ok := omBinaryIdentity(f.path); ok {
		omHelpProbe.mu.Lock()
		defer omHelpProbe.mu.Unlock()
		_, help := omHelpProbe.help[id]
		_, version := omHelpProbe.version[id]
		if help || version {
			return fmt.Sprintf("; nothing was logged: omHelpProbe already held this binary (help cached %v, version cached %v)", help, version)
		}
	}
	return "; the reviewer logged nothing and omHelpProbe holds no entry for it"
}

// calls returns every recorded invocation, in order.
func (f *fakeOMRun) calls(t *testing.T) []fakeOMCall {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatalf("the fake om recorded no call: %v%s", err, f.why())
	}
	var out []fakeOMCall
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--" {
			out = append(out, fakeOMCall{})
			continue
		}
		out[len(out)-1] = append(out[len(out)-1], line)
	}
	return out
}

// review is the one call that is neither the help probe nor the version probe.
func (f *fakeOMRun) review(t *testing.T) fakeOMCall {
	t.Helper()
	var found []fakeOMCall
	for _, c := range f.calls(t) {
		if len(c) > 0 && c[0] == "review" && !(len(c) == 2 && c[1] == "--help") {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("om ran %d reviews, want exactly 1 (calls %q%s)", len(found), f.calls(t), f.why())
	}
	return found[0]
}

// contractInput is a review of base..head in a real directory (the runner
// starts om there) that names a bead and carries acceptance text. Its warnings
// land on f, which a failing recorded-call assertion quotes.
func contractInput(t *testing.T, f *fakeOMRun) ReviewInput {
	t.Helper()
	return ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa111", Head: "bbb222",
		BeadID: "gt-q52sv", CriteriaText: "## Acceptance\n- [ ] it works\n- [ ] it is pinned",
		Logf: f.logf,
	}
}

// TestOMContractArgv pins the command line gastown runs: the fixed flags and
// --bead always, --criteria-file only when the installed om's help lists it as
// a flag of its own.
func TestOMContractArgv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		help  string
		bead  string
		crit  string
		extra []string // flags after --out, in order; criteria path is checked apart
		hasC  bool
	}{
		{name: "om lists both flags", help: helpBoth, bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}, hasC: true},
		{name: "om lists only --bead", help: "usage: om review --bead id\n", bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}},
		{name: "om lists only --criteria-file", help: "usage: om review --criteria-file path\n", bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}, hasC: true},
		{name: "om lists neither flag", help: "usage: om review -C dir --base b --head h --out f\n", bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}},
		{name: "look-alike flags do not count", help: "usage: om review --bead-id id --beads list --criteria-files dir\n", bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}},
		{name: "om prints no help", help: "", bead: "gt-q52sv", crit: "acceptance text", extra: []string{"--bead", "gt-q52sv"}},
		{name: "a bead with no acceptance text", help: helpBoth, bead: "gt-q52sv", extra: []string{"--bead", "gt-q52sv"}},
		{name: "acceptance text with no bead", help: helpBoth, crit: "acceptance text", hasC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeOM(t, fakeOMSpec{help: tc.help, verdict: `{"score":0.9,"verdict":"approve","findings":[]}`})
			in := contractInput(t, f)
			in.BeadID, in.CriteriaText = tc.bead, tc.crit
			if _, err := f.reviewer.ReviewBead(context.Background(), in); err != nil {
				t.Fatalf("ReviewBead: %v\nlog: %s", err, f.warn.String())
			}
			got := f.review(t)
			fixed := []string{"review", "-C", in.Dir, "--base", in.Base, "--head", in.Head, "--out"}
			if len(got) < len(fixed)+1 || !reflect.DeepEqual([]string(got[:len(fixed)]), fixed) {
				t.Fatalf("argv = %q, want it to open with %q and a verdict path", got, fixed)
			}
			out := got[len(fixed)]
			if filepath.Dir(out) != f.outDir || !strings.HasPrefix(filepath.Base(out), "om-verdict-") || !strings.HasSuffix(out, ".json") {
				t.Fatalf("--out = %q, want om-verdict-*.json in the reviewer's OutDir %q", out, f.outDir)
			}
			rest := []string(got[len(fixed)+1:])
			want := tc.extra
			if tc.hasC {
				if len(rest) != len(want)+2 || rest[len(rest)-2] != "--criteria-file" {
					t.Fatalf("flags after --out = %q, want %q then --criteria-file <path>", rest, want)
				}
				crit := rest[len(rest)-1]
				if filepath.Dir(crit) != f.outDir || !strings.HasPrefix(filepath.Base(crit), "om-criteria-") {
					t.Fatalf("--criteria-file = %q, want om-criteria-* in OutDir %q", crit, f.outDir)
				}
				if data, err := os.ReadFile(f.copyOf); err != nil || string(data) != tc.crit+"\n" {
					t.Fatalf("om read criteria %q (%v), want %q", data, err, tc.crit)
				}
				rest = rest[:len(rest)-2]
			}
			if len(want) == 0 && len(rest) == 0 {
				return
			}
			if !reflect.DeepEqual(rest, want) {
				t.Fatalf("flags after --out = %q, want %q", rest, want)
			}
		})
	}
}

// TestOMContractWithoutBeadOrCriteria pins the argv of a review that names
// neither: five flags and nothing else, and no probe of the help.
func TestOMContractWithoutBeadOrCriteria(t *testing.T) {
	t.Parallel()
	f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: `{"score":0.9,"verdict":"approve","findings":[]}`})
	dir := t.TempDir()
	if _, err := reviewAt(context.Background(), f.reviewer, "", dir, "aaa111", "bbb222"); err != nil {
		t.Fatal(err)
	}
	got := f.review(t)
	if len(got) != 9 || !reflect.DeepEqual([]string(got[:8]), []string{"review", "-C", dir, "--base", "aaa111", "--head", "bbb222", "--out"}) {
		t.Fatalf("argv = %q, want review -C dir --base b --head h --out <file>", got)
	}
}

// TestOMContractProbes pins the two read-only calls: `review --help`, whose
// flags decide the argv, and `version`, whose sha the verdict carries.
func TestOMContractProbes(t *testing.T) {
	t.Parallel()
	f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: `{"score":0.9,"verdict":"approve","findings":[]}`})
	v, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
	if err != nil {
		t.Fatal(err)
	}
	var help, version int
	for _, c := range f.calls(t) {
		switch {
		case reflect.DeepEqual([]string(c), []string{"review", "--help"}):
			help++
		case reflect.DeepEqual([]string(c), []string{"version"}):
			version++
		}
	}
	if help != 1 || version != 1 {
		t.Fatalf("probes: %d x `review --help`, %d x `version`; want one of each (calls %q%s)", help, version, f.calls(t), f.why())
	}
	if v.Version != contractOMSHA {
		t.Fatalf("Version = %q, want %q from `om version`", v.Version, contractOMSHA)
	}
}

// TestOMContractExitCodes pins the exit code as the decision: 0 is approve, 1
// is request_changes and 2 is an execution error that never judged the diff,
// whatever the verdict file says.
func TestOMContractExitCodes(t *testing.T) {
	t.Parallel()
	const approve = `{"score":0.9,"verdict":"approve","findings":[]}`
	const request = `{"score":0.3,"verdict":"request_changes","findings":[]}`
	for _, tc := range []struct {
		name    string
		verdict string
		exit    int
		want    string // the verdict string, "" for an error
		wantErr error  // when set, the error wraps it
	}{
		{name: "exit 0 is approve", verdict: approve, exit: 0, want: VerdictApprove},
		{name: "exit 1 is request_changes", verdict: request, exit: 1, want: VerdictRequestChanges},
		{name: "exit 2 is an execution error", verdict: "", exit: 2, wantErr: ErrOMExecution},
		{name: "exit 2 beats an approving file", verdict: approve, exit: 2, wantErr: ErrOMExecution},
		{name: "exit 2 beats a requesting file", verdict: request, exit: 2, wantErr: ErrOMExecution},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: tc.verdict, silent: tc.verdict == "", exit: tc.exit})
			v, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want it to wrap %v", err, tc.wantErr)
				}
				assertNoApproval(t, v)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.Verdict != tc.want || v.Approved() != (tc.want == VerdictApprove) {
				t.Fatalf("verdict = %q (Approved %v), want %q", v.Verdict, v.Approved(), tc.want)
			}
		})
	}
}

// assertNoApproval fails when v could let work land.
func assertNoApproval(t *testing.T, v Verdict) {
	t.Helper()
	if v.Approved() || v.Verdict != "" {
		t.Fatalf("a failed review returned verdict %+v; it must carry none", v)
	}
}

// TestOMContractFailsClosed pins that a verdict gastown cannot trust is an
// error and never an approval: a file that disagrees with the exit code, an
// unknown verdict string, and a file that is missing, empty or not JSON.
func TestOMContractFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict string
		silent  bool
		exit    int
	}{
		{name: "exit 0, file says request_changes", verdict: `{"score":0.2,"verdict":"request_changes"}`, exit: 0},
		{name: "exit 1, file says approve", verdict: `{"score":0.9,"verdict":"approve"}`, exit: 1},
		{name: "exit 0, file says skipped", verdict: `{"score":0.9,"verdict":"skipped"}`, exit: 0},
		{name: "exit 0, file says an unknown verdict", verdict: `{"score":0.9,"verdict":"maybe"}`, exit: 0},
		{name: "exit 1, file says an unknown verdict", verdict: `{"score":0.9,"verdict":"maybe"}`, exit: 1},
		{name: "exit 0, file has no verdict key", verdict: `{"score":0.9,"findings":[]}`, exit: 0},
		{name: "exit 0, verdict is not a string", verdict: `{"score":0.9,"verdict":true}`, exit: 0},
		{name: "exit 0, om writes no file", silent: true, exit: 0},
		{name: "exit 1, om writes no file", silent: true, exit: 1},
		{name: "exit 0, empty file", verdict: "", exit: 0},
		{name: "exit 1, blank file", verdict: " \n\t\n", exit: 1},
		{name: "exit 0, not JSON", verdict: "approve", exit: 0},
		{name: "exit 1, truncated JSON", verdict: `{"score":0.4,"verdict":"request_cha`, exit: 1},
		{name: "exit 0, a JSON array", verdict: `["approve"]`, exit: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: tc.verdict, silent: tc.silent, exit: tc.exit})
			v, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
			if err == nil {
				t.Fatalf("ReviewBead = %+v, nil; want an error", v)
			}
			if errors.Is(err, ErrOMExecution) || errors.Is(err, ErrOMTimeout) {
				t.Fatalf("error = %v; a bad verdict file is neither an execution error nor a timeout, which Land retries", err)
			}
			assertNoApproval(t, v)
		})
	}
}

// TestOMContractMissingBinary pins that an om that cannot start yields an
// error with no verdict.
func TestOMContractMissingBinary(t *testing.T) {
	t.Parallel()
	r := OMReviewer{Path: filepath.Join(t.TempDir(), "no-such-om"), OutDir: t.TempDir()}
	v, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b"})
	if err == nil {
		t.Fatalf("ReviewBead = %+v, nil; want an error", v)
	}
	assertNoApproval(t, v)
}

// TestOMContractTimeout pins that an om that outlives the reviewer's Timeout
// is stopped and reported as ErrOMTimeout, with no verdict.
func TestOMContractTimeout(t *testing.T) {
	t.Parallel()
	f := newFakeOM(t, fakeOMSpec{help: helpBoth, hang: true, verdict: `{"score":0.9,"verdict":"approve","findings":[]}`})
	f.reviewer.Timeout = 150 * time.Millisecond
	v, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
	if !errors.Is(err, ErrOMTimeout) {
		t.Fatalf("error = %v, want it to wrap ErrOMTimeout", err)
	}
	if errors.Is(err, ErrOMExecution) {
		t.Fatalf("error = %v; a timeout is not an execution error", err)
	}
	assertNoApproval(t, v)
}

// contractVerdictJSON carries every field gastown reads, plus keys it does not
// (a field om adds later must not break the parse).
const contractVerdictJSON = `{
  "score": 0.8125,
  "verdict": "request_changes",
  "summary": "two problems in the landing path",
  "bead": "gt-q52sv",
  "model": "a field gastown does not read",
  "findings": [
    {"id": "f1", "severity": "major", "path": "internal/land/review.go", "line": 308, "title": "argv drops --out", "detail": "the verdict path is never passed", "fix": "unread"},
    {"id": "f2", "severity": "minor", "path": "README.md", "line": 0, "title": "typo"}
  ]
}`

// TestOMContractVerdictFields pins every verdict field gastown reads: score,
// verdict, summary, the optional bead and findings[id, severity, path, line,
// title, detail], and that keys it does not read are ignored.
func TestOMContractVerdictFields(t *testing.T) {
	t.Parallel()
	f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: contractVerdictJSON, exit: 1})
	got, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
	if err != nil {
		t.Fatal(err)
	}
	want := Verdict{
		Verdict: VerdictRequestChanges, Score: 0.8125, Summary: "two problems in the landing path", Bead: "gt-q52sv",
		Findings: []Finding{
			{ID: "f1", Severity: "major", Path: "internal/land/review.go", Line: 308, Title: "argv drops --out", Detail: "the verdict path is never passed"},
			{ID: "f2", Severity: "minor", Path: "README.md", Title: "typo"},
		},
		Version: contractOMSHA,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdict = %+v\nwant      %+v", got, want)
	}
	if strings.Contains(f.warn.String(), "bead") {
		t.Fatalf("a verdict echoing the bead logged a warning: %s", f.warn.String())
	}
}

// TestOMContractApproveWithoutOptionalFields pins that an approve needs only
// verdict and score: no summary, no bead, no findings.
func TestOMContractApproveWithoutOptionalFields(t *testing.T) {
	t.Parallel()
	f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: `{"score":0.95,"verdict":"approve"}`})
	got, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
	if err != nil {
		t.Fatal(err)
	}
	want := Verdict{Verdict: VerdictApprove, Score: 0.95, Version: contractOMSHA}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("verdict = %+v, want %+v", got, want)
	}
	if !got.Approved() {
		t.Fatal("an approve verdict is not Approved()")
	}
}

// TestOMContractBeadEcho pins the optional bead: om echoes it, omits it or
// names another, and the verdict stands each time, with a warning for the last
// two.
func TestOMContractBeadEcho(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		bead     string // the "bead" value om writes, "" omits the key
		wantBead string
		warn     string // fragment of the warning, "" for none
	}{
		{name: "echoes the bead", bead: "gt-q52sv", wantBead: "gt-q52sv"},
		{name: "omits the bead", warn: "did not echo the work bead"},
		{name: "names another bead", bead: "gt-other", wantBead: "gt-other", warn: "names bead gt-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict := `{"score":0.9,"verdict":"approve","findings":[]}`
			if tc.bead != "" {
				verdict = fmt.Sprintf(`{"score":0.9,"verdict":"approve","bead":%q,"findings":[]}`, tc.bead)
			}
			f := newFakeOM(t, fakeOMSpec{help: helpBoth, verdict: verdict})
			got, err := f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
			if err != nil {
				t.Fatal(err)
			}
			if got.Bead != tc.wantBead || !got.Approved() {
				t.Fatalf("verdict = %+v, want an approve with bead %q", got, tc.wantBead)
			}
			if tc.warn == "" && f.warn.Len() > 0 {
				t.Fatalf("unexpected warning: %s", f.warn.String())
			}
			if tc.warn != "" && !strings.Contains(f.warn.String(), tc.warn) {
				t.Fatalf("log = %q, want it to contain %q", f.warn.String(), tc.warn)
			}
		})
	}
}

// TestOMContractLeavesNoFiles pins that a review removes the verdict and
// criteria files it created in OutDir, whatever the outcome.
func TestOMContractLeavesNoFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		spec fakeOMSpec
	}{
		{name: "approve", spec: fakeOMSpec{help: helpBoth, verdict: `{"score":0.9,"verdict":"approve"}`}},
		{name: "request_changes", spec: fakeOMSpec{help: helpBoth, verdict: `{"score":0.2,"verdict":"request_changes"}`, exit: 1}},
		{name: "execution error", spec: fakeOMSpec{help: helpBoth, silent: true, exit: 2}},
		{name: "bad verdict", spec: fakeOMSpec{help: helpBoth, verdict: "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeOM(t, tc.spec)
			_, _ = f.reviewer.ReviewBead(context.Background(), contractInput(t, f))
			entries, err := os.ReadDir(f.outDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("OutDir still holds %d file(s) after the review, first %s", len(entries), entries[0].Name())
			}
		})
	}
}
