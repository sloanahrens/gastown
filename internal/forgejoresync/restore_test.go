package forgejoresync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// restoreStages are the run's stages in order, the names a failing row picks from.
var restoreStages = []string{"list", "fetch", "lift", "push", "verify"}

// restoreRuleLine is the by-hand message a failed restore logs; the test asserts it verbatim.
const restoreRuleLine = "ERROR: could not restore main protection on acme/rig; run the provisioner by hand"

// restoreCase is one run of Run: which stage fails, whether the provisioner fails, and
// whether the token resolves or the run is a dry run. writes is what the provisioner
// writes to its log; nil writes the token in one line.
type restoreCase struct {
	name        string
	failStep    string
	restoreFail bool
	noToken     bool
	dryRun      bool
	badBranch   bool
	writes      func(io.Writer)
}

// restoreCall is one in-process provisioner call: the options it was given and the
// token the provisioner would read from its environment.
type restoreCall struct {
	opts  forgejoprov.Options
	token string
}

// restoreFixture is one Resync wired to fake stages and a fake provisioner, recording
// the order the stages and the restore ran in.
type restoreFixture struct {
	resync *Resync
	events []string
	calls  []restoreCall
	logs   []string
}

// newRestoreFixture builds a Resync for c. The admin token is in the environment unless
// c.noToken, and the token file is absent, so a missing token is a real absence.
func newRestoreFixture(t *testing.T, c restoreCase) *restoreFixture {
	t.Helper()
	fx := &restoreFixture{}
	getenv := func(name string) string {
		if name == restoreTokenEnv && !c.noToken {
			return forgejotest.AdminToken
		}
		return ""
	}
	branch := "main"
	if c.badBranch {
		// A quote needs escaping in the provisioner's JSON, which it refuses to write.
		branch = `bad"branch`
	}
	fx.resync = &Resync{
		Opts: &Options{
			Repo:       liftRepo,
			APIURL:     "http://127.0.0.1:3000/api/v1",
			WebURL:     "http://127.0.0.1:3000",
			MainBranch: branch,
			BackupDir:  liftBackupDir,
			DryRun:     c.dryRun,
		},
		Getenv:   getenv,
		ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		Log:      func(line string) { fx.logs = append(fx.logs, line) },
	}
	for _, name := range restoreStages {
		fx.resync.Steps = append(fx.resync.Steps, Step{Name: name, Run: func() error {
			fx.events = append(fx.events, name)
			if name == c.failStep {
				return errors.New("step refused")
			}
			return nil
		}})
	}
	fx.resync.Provision = func(_ context.Context, opts *forgejoprov.Options, env forgejoprov.Env) int {
		fx.events = append(fx.events, "restore")
		fx.calls = append(fx.calls, restoreCall{opts: *opts, token: env.Getenv(restoreTokenEnv)})
		if c.writes != nil {
			c.writes(env.Log)
		} else {
			// The provisioner echoes the token into its log: the run's log must not carry it.
			fmt.Fprintf(env.Log, "forgejo-provision: token %s\n", forgejotest.AdminToken)
		}
		if c.restoreFail {
			return forgejoprov.ExitFailure
		}
		return forgejoprov.ExitOK
	}
	return fx
}

// writeTokenInPieces writes "forgejo-provision: token <token>" and its newline in n
// writes, each cut inside the token, so no single write holds the whole token.
func writeTokenInPieces(w io.Writer, n int) {
	msg := "forgejo-provision: token " + forgejotest.AdminToken + "\n"
	start := strings.Index(msg, forgejotest.AdminToken)
	prev := 0
	for i := 1; i <= n; i++ {
		cut := len(msg)
		if i < n {
			cut = start + i*len(forgejotest.AdminToken)/n
		}
		fmt.Fprint(w, msg[prev:cut])
		prev = cut
	}
}

// assertNoTokenInLogs fails when the admin token shows in the run's log. The failure
// names the output, never its text, so the message cannot carry the token either.
func assertNoTokenInLogs(t *testing.T, fx *restoreFixture) {
	t.Helper()
	for _, line := range fx.logs {
		if strings.Contains(line, forgejotest.AdminToken) {
			t.Fatal("admin token leaked into the run's log")
		}
	}
}

// TestRestoreRunsAfterEveryFailurePoint checks the provisioner runs exactly once after
// each stage fails and after success, and that no stage after a failure runs. Mutation:
// skip the restore when the push fails and the push row goes red.
func TestRestoreRunsAfterEveryFailurePoint(t *testing.T) {
	t.Parallel()
	cases := []restoreCase{{name: "success"}}
	for _, stage := range restoreStages {
		cases = append(cases, restoreCase{name: "fails at " + stage, failStep: stage})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRestoreFixture(t, c)
			rc := fx.resync.Run()

			wantRC := ExitOK
			if c.failStep != "" {
				wantRC = ExitFailure
			}
			if rc != wantRC {
				t.Fatalf("Run() = %d, want %d", rc, wantRC)
			}
			if len(fx.calls) != 1 {
				t.Fatalf("provisioner called %d times, want 1", len(fx.calls))
			}
			var want []string
			for _, stage := range restoreStages {
				want = append(want, stage)
				if stage == c.failStep {
					break
				}
			}
			want = append(want, "restore")
			if !slices.Equal(fx.events, want) {
				t.Fatalf("events = %v, want %v", fx.events, want)
			}
			assertNoTokenInLogs(t, fx)
		})
	}
}

// TestRestoreSkippedBeforeTheTokenResolves checks a missing token stops the run before
// any stage and before the provisioner: nothing is lifted, so there is nothing to restore.
func TestRestoreSkippedBeforeTheTokenResolves(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "no token", noToken: true})
	rc := fx.resync.Run()
	if rc != ExitFailure {
		t.Fatalf("Run() = %d, want %d", rc, ExitFailure)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("provisioner called %d times with no token, want 0", len(fx.calls))
	}
	if len(fx.events) != 0 {
		t.Fatalf("stages ran with no token: %v", fx.events)
	}
	if !slices.ContainsFunc(fx.logs, func(line string) bool { return strings.Contains(line, "no admin token") }) {
		t.Fatalf("no refusal logged; logs = %v", fx.logs)
	}
}

// TestRestoreFailureForcesExitOne checks a failed restore turns the run into ExitFailure
// and logs the by-hand message, whether or not a stage failed first. Mutation: drop the
// forced ExitFailure in the deferred restore and the success row goes red.
func TestRestoreFailureForcesExitOne(t *testing.T) {
	t.Parallel()
	cases := []restoreCase{
		{name: "steps succeed", restoreFail: true},
		{name: "a step fails too", failStep: "push", restoreFail: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRestoreFixture(t, c)
			if rc := fx.resync.Run(); rc != ExitFailure {
				t.Fatalf("Run() = %d, want %d", rc, ExitFailure)
			}
			if !slices.Contains(fx.logs, restoreRuleLine) {
				t.Fatalf("by-hand message not logged; logs = %v", fx.logs)
			}
			assertNoTokenInLogs(t, fx)
		})
	}
}

// TestRestoreMasksTheTokenAcrossWrites checks the token never reaches the run's log, even
// when the provisioner splits it across writes or leaves a partial token at the end of its
// output. Each row names the one line the masked output must end up as. Mutation: mask each
// write alone, instead of the whole stream, and the split rows leak the token.
func TestRestoreMasksTheTokenAcrossWrites(t *testing.T) {
	t.Parallel()
	tok := forgejotest.AdminToken
	cases := []struct {
		name   string
		writes func(io.Writer)
		want   string
	}{
		{
			name:   "token split across two writes",
			writes: func(w io.Writer) { writeTokenInPieces(w, 2) },
			want:   "forgejo-provision: token ***",
		},
		{
			name:   "token split across three writes",
			writes: func(w io.Writer) { writeTokenInPieces(w, 3) },
			want:   "forgejo-provision: token ***",
		},
		{
			name:   "token is the last bytes of the stream",
			writes: func(w io.Writer) { fmt.Fprint(w, "forgejo-provision: end "+tok) },
			want:   "forgejo-provision: end ***",
		},
		{
			name:   "partial token is the last bytes of the stream",
			writes: func(w io.Writer) { fmt.Fprint(w, "forgejo-provision: end "+tok[:4]) },
			want:   "forgejo-provision: end ***",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fx := newRestoreFixture(t, restoreCase{name: c.name, writes: c.writes})
			if rc := fx.resync.Run(); rc != ExitOK {
				t.Fatalf("Run() = %d, want %d", rc, ExitOK)
			}
			assertNoTokenInLogs(t, fx)
			if !slices.Contains(fx.logs, c.want) {
				t.Fatalf("masked line %q not logged; logs = %v", c.want, fx.logs)
			}
		})
	}
}

// restoreTransport is the Forgejo transport a restore run reaches. It records each request
// it is sent. With hang set it waits for the request's context to end instead of answering,
// and records whether that end was the deadline.
type restoreTransport struct {
	hang      bool
	requests  []*http.Request
	deadlined bool
}

// RoundTrip records req and then either refuses it or, when hang is set, waits for its
// context to end. A hung request with no deadline is refused at once, so a restore that
// drops its deadline fails the test instead of blocking it.
func (tr *restoreTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.requests = append(tr.requests, req)
	if !tr.hang {
		return nil, errors.New("transport refused")
	}
	if _, ok := req.Context().Deadline(); !ok {
		return nil, errors.New("request has no deadline")
	}
	<-req.Context().Done()
	tr.deadlined = errors.Is(req.Context().Err(), context.DeadlineExceeded)
	return nil, req.Context().Err()
}

// TestRestoreDeadlineBoundsAHungForgejo checks a hung Forgejo cannot hold the restore past
// its deadline: the request ends on the deadline, and the run still fails with the by-hand
// message. Mutation: drop the context deadline and the transport waits out its fallback bound,
// so deadlined stays false and the row goes red.
func TestRestoreDeadlineBoundsAHungForgejo(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "hung forgejo"})
	fx.resync.Provision = nil
	tr := &restoreTransport{hang: true}
	fx.resync.Client = &forgejoprov.Client{HTTP: &http.Client{Transport: tr}}
	fx.resync.RestoreTimeout = 10 * time.Millisecond
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run() = %d, want %d", rc, ExitFailure)
	}
	if !tr.deadlined {
		t.Fatal("restore request was not ended by the restore deadline")
	}
	if !slices.Contains(fx.logs, restoreRuleLine) {
		t.Fatalf("by-hand message not logged; logs = %v", fx.logs)
	}
	assertNoTokenInLogs(t, fx)
}

// TestRestoreReachesForgejoThroughTheClientTransport checks the restore runs the production
// provisioner when none is injected, and that its requests go through the Resync's client
// transport to the API root the run was given, with the admin token only in the
// Authorization header. Mutation: drop the HTTP forwarding and no request reaches the transport.
func TestRestoreReachesForgejoThroughTheClientTransport(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "reaches forgejo"})
	fx.resync.Provision = nil
	tr := &restoreTransport{}
	fx.resync.Client = &forgejoprov.Client{HTTP: &http.Client{Transport: tr}}
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run() = %d, want %d", rc, ExitFailure)
	}
	if len(tr.requests) == 0 {
		t.Fatal("no request reached the client transport")
	}
	req := tr.requests[0]
	if !strings.HasPrefix(req.URL.String(), "http://127.0.0.1:3000/api/v1/") {
		t.Fatalf("request went to %s, not the API root", req.URL)
	}
	if !strings.Contains(req.Header.Get("Authorization"), forgejotest.AdminToken) {
		t.Fatal("request did not carry the admin token in its Authorization header")
	}
	if !slices.Contains(fx.logs, restoreRuleLine) {
		t.Fatalf("by-hand message not logged; logs = %v", fx.logs)
	}
	assertNoTokenInLogs(t, fx)
}

// TestRestoreDryRunSendsNoRestore checks a dry run runs the stages and calls no
// provisioner, so a dry run restores nothing.
func TestRestoreDryRunSendsNoRestore(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "dry run", dryRun: true})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("Run() = %d, want %d", rc, ExitOK)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("provisioner called %d times in a dry run, want 0", len(fx.calls))
	}
}

// TestRestoreRefusedOptionsSendNoRestore checks options the provisioner refuses stop the
// restore before any call, and the by-hand message is still logged, so main is not left
// unprotected without a pointer to the fix.
func TestRestoreRefusedOptionsSendNoRestore(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "bad branch", badBranch: true})
	if rc := fx.resync.Run(); rc != ExitFailure {
		t.Fatalf("Run() = %d, want %d", rc, ExitFailure)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("provisioner called %d times with refused options, want 0", len(fx.calls))
	}
	if !slices.ContainsFunc(fx.logs, func(line string) bool {
		return strings.HasPrefix(line, "ERROR: could not restore ") && strings.HasSuffix(line, "run the provisioner by hand")
	}) {
		t.Fatalf("by-hand message not logged; logs = %v", fx.logs)
	}
	assertNoTokenInLogs(t, fx)
}

// TestRestoreNeverPutsTheTokenInArgv checks the in-process call hands the token over only
// in the environment the provisioner reads, and that no string option holds it.
func TestRestoreNeverPutsTheTokenInArgv(t *testing.T) {
	t.Parallel()
	fx := newRestoreFixture(t, restoreCase{name: "success"})
	if rc := fx.resync.Run(); rc != ExitOK {
		t.Fatalf("Run() = %d, want %d", rc, ExitOK)
	}
	if len(fx.calls) != 1 {
		t.Fatalf("provisioner called %d times, want 1", len(fx.calls))
	}
	call := fx.calls[0]
	assertNoTokenInOptions(t, call.opts)
	if call.token != forgejotest.AdminToken {
		t.Fatal("provisioner did not receive the admin token in memory")
	}
	if call.opts.Repos[0] != liftRepo || call.opts.MainBranch != "main" || call.opts.APIURL != "http://127.0.0.1:3000/api/v1" {
		t.Fatalf("restore options = repos %v branch %q api %q", call.opts.Repos, call.opts.MainBranch, call.opts.APIURL)
	}
	assertNoTokenInLogs(t, fx)
}

// assertNoTokenInOptions fails when a string or string-slice field of opts holds the
// admin token. The failure names the field, never its value.
func assertNoTokenInOptions(t *testing.T, opts forgejoprov.Options) {
	t.Helper()
	v := reflect.ValueOf(opts)
	for i := range v.NumField() {
		field := v.Field(i)
		name := v.Type().Field(i).Name
		switch field.Kind() {
		case reflect.String:
			if strings.Contains(field.String(), forgejotest.AdminToken) {
				t.Fatalf("option %s carries the admin token", name)
			}
		case reflect.Slice:
			for j := range field.Len() {
				if field.Index(j).Kind() == reflect.String && strings.Contains(field.Index(j).String(), forgejotest.AdminToken) {
					t.Fatalf("option %s carries the admin token", name)
				}
			}
		}
	}
}
