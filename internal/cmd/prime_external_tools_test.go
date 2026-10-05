package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// These tests run prime's external tools through primeTools with a fake
// runner, a fake clock and a buffer for output. They used to put shell-script
// stubs for bd, gt and git on PATH: every test then paid a fork+exec of a
// freshly written executable, which on a loaded host could outlast the
// deadlines around it (gt-5clb, gt-v2a5), and needed t.Setenv, a stdout swap
// and a package-level deadline hook, so none could run in parallel. The
// subprocess half — killing a tool at its deadline and not waiting on an
// escaped child — is execPrimeExternalCommand's, covered in the integration
// tier.

// primeToolCall is one tool invocation a fakePrimeRunner saw.
type primeToolCall struct {
	workDir string
	line    string // "<name>:<args joined by spaces>", as the old stubs logged
	ctx     context.Context
}

// fakePrimeRunner answers prime's tool calls from a table keyed by call line
// and records every call. A call with no answer fails like a tool that exits
// non-zero.
type fakePrimeRunner struct {
	mu      sync.Mutex
	answers map[string]string
	calls   []primeToolCall
	// block, when set for a call line, makes that call wait for its context
	// to end and return ctx.Err(), as a wedged tool does. started receives
	// the call once it is waiting.
	block   map[string]bool
	started chan primeToolCall
}

func (f *fakePrimeRunner) run(ctx context.Context, workDir, name string, args ...string) (bytes.Buffer, bytes.Buffer, error) {
	var stdout, stderr bytes.Buffer
	call := primeToolCall{workDir: workDir, line: name + ":" + strings.Join(args, " "), ctx: ctx}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	answer, ok := f.answers[call.line]
	blocks := f.block[call.line]
	f.mu.Unlock()

	if blocks {
		f.started <- call
		<-ctx.Done()
		return stdout, stderr, ctx.Err()
	}
	if !ok {
		stderr.WriteString("unexpected args: " + call.line)
		return stdout, stderr, errors.New("exit status 99")
	}
	stdout.WriteString(answer)
	return stdout, stderr, nil
}

func (f *fakePrimeRunner) called(line string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.line == line {
			return true
		}
	}
	return false
}

func (f *fakePrimeRunner) callLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []string
	for _, c := range f.calls {
		lines = append(lines, c.line)
	}
	return lines
}

// newFakePrimeTools returns primeTools wired to f, a fake clock, and a buffer
// that collects everything prime prints.
func newFakePrimeTools(f *fakePrimeRunner) (primeTools, *clockwork.FakeClock, *bytes.Buffer) {
	clk := clockwork.NewFakeClockAt(primeTestEpoch)
	var out bytes.Buffer
	return primeTools{run: f.run, clock: clk, out: &out}, clk, &out
}

// primeTestEpoch is where every fake clock in the prime tests starts, so a
// test never depends on the wall-clock time it runs at.
var primeTestEpoch = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)

const (
	primeKVListCall    = "bd:kv list --json"
	primeMailCheckCall = "gt:mail check --inject"
)

// TestRunPrimeExternalTools_RunsMemoryAndMail pins that the two sections are
// independent: a role that renders memories still gets its mail, both in one
// call. Crew is the role that gets both, so it is the subject here; the
// roles that get only one are covered by the two tests below.
func TestRunPrimeExternalTools_RunsMemoryAndMail(t *testing.T) {
	t.Parallel()
	f := &fakePrimeRunner{answers: map[string]string{
		primeKVListCall:    `{"gt.feedback.test":"remembered"}`,
		primeMailCheckCall: "MAIL OUTPUT\n",
	}}
	p, _, out := newFakePrimeTools(f)
	workDir := t.TempDir()

	p.externalTools(RoleContext{Role: RoleCrew}, workDir)

	for _, want := range []string{primeKVListCall, primeMailCheckCall} {
		if !f.called(want) {
			t.Fatalf("calls %q missing %q", f.callLines(), want)
		}
	}
	for _, c := range f.calls {
		if c.workDir != workDir {
			t.Errorf("%s ran in %q, want %q", c.line, c.workDir, workDir)
		}
	}
	if !strings.Contains(out.String(), "remembered") {
		t.Fatalf("memory injection missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "MAIL OUTPUT") {
		t.Fatalf("mail injection missing: %q", out.String())
	}
}

// TestRunPrimeExternalTools_MemoryIsCrewOnly pins the memory role gate
// (gt-o51s, plan Task 8 C3). It is a separate test from
// SkipsMailCheckForPatrolRoles because the two gates are independent: mail is
// withheld from patrol roles, memories from everyone but crew, and a
// polecat is the role that gets exactly one of them.
func TestRunPrimeExternalTools_MemoryIsCrewOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role       Role
		wantMemory bool
	}{
		{RoleCrew, true},
		{RolePolecat, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			t.Parallel()
			f := &fakePrimeRunner{answers: map[string]string{
				primeKVListCall:    `{"gt.feedback.test":"remembered"}`,
				primeMailCheckCall: "MAIL OUTPUT\n",
			}}
			p, _, out := newFakePrimeTools(f)

			p.externalTools(RoleContext{Role: tc.role}, t.TempDir())

			if tc.wantMemory {
				if !f.called(primeKVListCall) {
					t.Fatalf("role %s never listed memories: calls %q", tc.role, f.callLines())
				}
				if !strings.Contains(out.String(), "remembered") {
					t.Fatalf("role %s rendered no memories: %q", tc.role, out.String())
				}
				return
			}
			if f.called(primeKVListCall) {
				t.Fatalf("role %s listed memories it does not render: calls %q", tc.role, f.callLines())
			}
			if strings.Contains(out.String(), "remembered") {
				t.Fatalf("role %s rendered a memory index it should not have: %q", tc.role, out.String())
			}
			// The gate is about memories only: withholding them must not take
			// the role's mail with it.
			if tc.role == RolePolecat {
				if !f.called(primeMailCheckCall) {
					t.Fatalf("polecat lost its mail check along with its memories: calls %q", f.callLines())
				}
				if !strings.Contains(out.String(), "MAIL OUTPUT") {
					t.Fatalf("polecat lost its mail along with its memories: %q", out.String())
				}
			}
		})
	}
}

// TestRunPrimeMemoryInject_BoundsTheIndex is the acceptance guard for the size
// half of gt-o51s: at the live corpus's scale the section a crew prime
// carries must fit inside memoryInjectMaxChars, which is what makes it fit
// inside primeHookBudget alongside the hooked work at all.
func TestRunPrimeMemoryInject_BoundsTheIndex(t *testing.T) {
	t.Parallel()
	// The live corpus, in shape: 38 entries of ~1200 chars each.
	kvJSON, err := json.Marshal(syntheticMemories(38, 1200))
	if err != nil {
		t.Fatalf("marshal kv: %v", err)
	}
	f := &fakePrimeRunner{answers: map[string]string{primeKVListCall: string(kvJSON)}}
	p, _, buf := newFakePrimeTools(f)

	p.memoryInject(RoleContext{Role: RoleCrew}, t.TempDir())
	out := buf.String()

	if !strings.Contains(out, "# Agent Memories (38)") {
		t.Fatalf("crew rendered no index over a 38-entry corpus:\n%s", out[:min(len(out), 400)])
	}
	if len(out) > memoryInjectMaxChars+memoryIndexFooterSlack {
		t.Errorf("memory section = %d chars, want <= %d (cap %d + footer)",
			len(out), memoryInjectMaxChars+memoryIndexFooterSlack, memoryInjectMaxChars)
	}
	// The section is only useful if the whole of it is affordable: a bound that
	// still exceeded the hook budget would be no bound at all.
	if len(out) > primeHookBudget {
		t.Errorf("memory section = %d chars, over the %d-char hook budget by itself", len(out), primeHookBudget)
	}
}

// waitOutWedgedTool runs fn, which must reach exactly one wedged tool call,
// and drives the fake clock to that call's deadline. It checks the call was
// given primeExternalToolTimeout from the moment it started, and that fn
// returns once that much fake time has passed, which is what abandoning the
// tool means. No wall-clock time is measured.
func waitOutWedgedTool(t *testing.T, f *fakePrimeRunner, clk *clockwork.FakeClock, fn func()) primeToolCall {
	t.Helper()
	f.started = make(chan primeToolCall)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	var call primeToolCall
	select {
	case call = <-f.started:
	case <-done:
		t.Fatalf("returned without reaching the wedged tool: calls %q", f.callLines())
	}
	deadline, ok := call.ctx.Deadline()
	if !ok {
		t.Fatalf("%s ran with no deadline", call.line)
	}
	if want := clk.Now().Add(primeExternalToolTimeout); !deadline.Equal(want) {
		t.Fatalf("%s deadline = %v, want now+%v = %v", call.line, deadline, primeExternalToolTimeout, want)
	}
	clk.Advance(primeExternalToolTimeout)
	<-done
	if err := call.ctx.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s context err = %v, want deadline exceeded", call.line, err)
	}
	return call
}

// TestRunPrimeExternalTools_BoundsSlowMailCheck proves prime abandons a mail
// check at primeExternalToolTimeout instead of blocking startup on it, and
// that the memory section it rendered first still stands.
//
// The bound is asserted on the fake clock: the wedged mail check is handed a
// deadline exactly primeExternalToolTimeout away, and prime returns when that
// much fake time passes. The earlier elapsed<N assertions measured the host,
// not the code: a correctly bounded prime measured 2.687s under gate load
// (gt-v2a5), and the 10s safety net that replaced it still timed a freshly
// written shell stub's exec.
func TestRunPrimeExternalTools_BoundsSlowMailCheck(t *testing.T) {
	t.Parallel()
	f := &fakePrimeRunner{
		answers: map[string]string{primeKVListCall: `{"gt.feedback.test":"remembered"}`},
		block:   map[string]bool{primeMailCheckCall: true},
	}
	p, clk, out := newFakePrimeTools(f)

	// Crew is the subject because this test asserts that a stalled mail
	// check leaves the memory section standing, which needs a role that
	// renders one (see shouldRenderMemories).
	call := waitOutWedgedTool(t, f, clk, func() {
		p.externalTools(RoleContext{Role: RoleCrew}, t.TempDir())
	})

	if call.line != primeMailCheckCall {
		t.Fatalf("wedged call = %q, want %q", call.line, primeMailCheckCall)
	}
	if !strings.Contains(out.String(), "remembered") {
		t.Fatalf("a slow mail check must not suppress the memory section: %q", out.String())
	}
}
