# Testing

Before you write or convert a test in this repository, read this page. It covers the rules that `internal/testpolicy` enforces, the seams that let unit tests avoid real processes and real time, and the procedure for converting a package. The worked examples come from the two packages converted first: `internal/tmux` and `internal/slot`. The design behind the rules is in [the test-rewrite design](plans/2026-09-27-test-rewrite-design.md).

## Two tiers

| tier | what runs | build tag | test names | command |
|---|---|---|---|---|
| unit | in-process only: fakes, fake clock, `t.TempDir`, `git` | none | `TestX` | `make test` |
| integration | real tmux, Docker, bd, Dolt, the gt binary, real processes | `//go:build integration` | `TestIntegrationX` | `make test-integration` |

- `make test` runs the unit tier through the budget runner (`internal/testpolicy/cmd/budget`).
- `make test-integration` runs `go test -tags integration -run '^TestIntegration' ./...`. Tests that need Docker still need a `gt slot run` around the call.
- `make test-timing PKGS=./internal/<pkg>/...` measures the unit tier in a tmux pane that launchd starts. macOS scans every new executable there, which is the town's condition after a reboot. The script first prints a probe (ms per new executable; a taxed pane shows 50 or more), then the seconds taken. A converted package's target is 5 s or less in that pane. The integration tier's target is 60 s or less.

The unit tier does no real I/O that carries a wall-clock timeout. That includes sockets and dials: a 200 ms dial in `internal/tmux` timed out under load (a 1-in-600 failure) and blocked other runs for 150 s in socket syscalls. The `no-network` rule enforces this for sockets, and the same rule applies by review to anything else with a deadline. Put such I/O behind a seam and script it; the filesystem through `t.TempDir` is fine.

The integration tier is only for tests that need the real tool. A test that moves there to escape a rule, such as a global swap, belongs in the unit tier behind a seam. Integration tests also follow the zero-flakes rule:
- They poll for the condition they need with a generous deadline (`eventually` and `integrationWait` in `internal/tmux`), never sleep for a guessed interval.
- A missing precondition fails the test instead of skipping it.

`go test -tags integration` compiles both tiers together. `TestMain` therefore lives in the integration tier only (see `internal/tmux/testmain_integration_test.go`). A unit test must pass with no tmux, ps or docker on `PATH`.

## The rules

`go test ./internal/testpolicy/` scans every package not listed in `internal/testpolicy/unconverted.txt`. It prints one line per violation: `file:line:col: [rule] message`.

| rule | a unit test must not |
|---|---|
| `no-sleep` | call `time.Sleep`, `time.After`, `time.AfterFunc`, `time.NewTimer`, `time.NewTicker` or `time.Tick` |
| `no-env` | call `os.Setenv`, `os.Unsetenv` or `t.Setenv` |
| `no-chdir` | call `os.Chdir` or `t.Chdir` |
| `no-skip` | call `t.Skip`, `t.Skipf` or `t.SkipNow` |
| `no-subprocess` | run `exec.Command` on anything but `git` |
| `no-network` | dial or listen: `net.Dial*`, `net.Listen*`, `net.File*Conn`/`FileListener`, `httptest.New*Server`, or a `net.Dialer`/`net.ListenConfig` (literal, `var` or `new`). The check is syntax-only, so a Dialer reached through a type alias or a struct field is not caught. |
| `fake-clock-epoch` | call `clockwork.NewFakeClock()`, which starts at `time.Now()`; use `NewFakeClockAt` with a fixed epoch |
| `no-build` | run the `go` tool |
| `no-exec-files` | write a `#!` script or create or chmod a file with an execute bit |
| `no-global-swap` | assign a package-level variable |
| `parallel` | leave out `t.Parallel()` in a top-level `TestX` |

The rules for production code, and for the tree as a whole, are:

| rule | meaning |
|---|---|
| `prod-no-sleep` | a package that imports clockwork must not call `time.Sleep`; sleep through the clock |
| `prod-no-setenv` | non-`main` code must not mutate the process environment |
| `integration-name` | a test in an integration-tagged file must be named `TestIntegration…` |
| `fake-contract` | a `…fake` package must export a `Run…Contract`, and an integration test must call it |
| `allow-reason` | an exemption must carry a reason |

An exemption is a comment on the offending line or the line above it:

```go
//testpolicy:allow no-subprocess — reason the rule cannot hold here
```

`TestPolicy` logs every exemption it honors. Exemptions are for a real exception; a converted package normally has none, and `internal/tmux` has none. When a test needs real processes, move it to the integration tier instead of exempting it.

## Seams

A type that runs programs or reads the clock gets two unexported fields, with nil meaning "use the real one". A struct literal such as `&Tmux{}` then keeps working, and tests set both fields.

### The exec runner

From `internal/tmux/exec.go`:

```go
// execFunc runs a program and returns its stdout and stderr. Tmux sends every
// tmux, ps and kill invocation through one, so tests can record and answer
// them without starting processes.
type execFunc func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func (t *Tmux) runner() execFunc {
	if t.exec == nil {
		return realExec
	}
	return t.exec
}
```

Every call goes through `t.runner()`: tmux, `ps` and `kill`. So does a helper built inside a method. `killSplitBrainSession` used to call `NewTmuxWithSocket("default")` and so escaped the seam; it now calls `t.withSocket("default")`, which copies both fields. Package-level helpers take the runner as their first parameter, for example `getAllDescendants(ex execFunc, pid string)`. If an error has to carry an exit code, match it through `interface{ ExitCode() int }` rather than `*exec.ExitError`, so that a fake can produce one too.

The tests build the Tmux in the test file, never in production code (`internal/tmux/helpers_test.go`):

```go
func newTmuxForTest(socket string, ex execFunc, clk clockwork.Clock) *Tmux {
	return &Tmux{socketName: socket, exec: ex, clock: clk}
}
```

A test then asserts what was sent (bucket T below). `scripted` in `helpers_test.go` records every call, with the `-u -L <socket>` prefix split off:

```go
func TestHasSessionSendsHasSession(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := newTmuxForTest("gt-test-x", s.exec, nil)
	ok, err := tm.HasSession("alpha")
	if err != nil || !ok {
		t.Fatalf("HasSession = %v, %v; want true, nil", ok, err)
	}
	want := tmuxCall{name: "tmux", socket: "gt-test-x", args: []string{"has-session", "-t", "=alpha"}}
	if calls := s.all(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
```

For bigger tests the package has more helpers:
- `bySub` answers `scripted` from a map keyed by subcommand.
- `fakeServer` (`fakeserver_test.go`) is a small stateful model of a tmux server and the process table that `ps` and `kill` see.
- `fakeSockets` (`fakesockets_test.go`) scripts the socket directory behind the `socketOps` seam.

Every argv-level emulator, and every canned stdout or stderr, is checked against the real tool by a differential integration test. The test replays the argv the package sends against the real tool and against the emulator, and compares exit status, error family and output. Values that differ by nature (pids, ids, paths, timestamps) are compared only as empty or non-empty. When a unit test asserts an error, the canned stderr must be the real tool's words. In tmux 3.7c, `send-keys` to a missing session says "can't find pane", not "can't find session", and a test that invented the latter hid a real bug: `WaitForIdle` never saw that the session had gone.

In `internal/tmux`, `TestIntegrationFakeServerMatchesTmux` pins these:
- every subcommand fakeServer answers, for a live target, a missing target and no server
- live pane content from `capture-pane`, with and without `-S`
- every `#{...}` format fakeServer expands
- `list-panes -s` and `-a`
- a live `respawn-pane`
- the canned `list-keys` table the binding tests use

What it does not pin, and why that is acceptable:
- fakeServer's agent-composer rendering and its `ps` process table: they model an agent TUI and the kernel, which no tmux replay can check.
- The dialog, spinner and transcript fixtures: they are verbatim captures from real sessions, each saying where it came from.
Keep new canned answers inside the pinned set, or extend the test.

Other environment reads go through a seam too. Methods read `$TMUX` and `GT_ROOT` through `t.env`, and `newTmuxForTest` gives each test an empty environment.

### Interface seams

When a collaborator is a whole service rather than one program, inject an interface. `internal/slot` does this with `ContainerRuntime` (`internal/slot/gate.go`):

```go
type ContainerRuntime interface {
	List() ([]string, error)
	Remove(id string) error
	Info() (VMInfo, error)
}

// WithRuntime sets the Docker runtime the gate checks.
func WithRuntime(r ContainerRuntime) Option { return func(g *Gate) { g.runtime = r } }

// WithClock sets the clock the gate's waits and timestamps run on.
func WithClock(c clockwork.Clock) Option { return func(g *Gate) { g.clock = c } }
```

Its tests hand `NewGate(WithRuntime(&fakeRuntime{...}), WithClock(clk))` a scripted `docker ps` listing (`internal/slot/helpers_test.go`).

### The clock

Every sleep, deadline and time read goes through `clockwork.Clock`:

- `t.clk().Sleep(d)`
- `t.clk().Now()`
- `t.clk().Since(x)`
- `t.clk().NewTimer(d)` (read it with `.Chan()`)
- `clockwork.WithTimeout(ctx, clk, d)` for a context deadline

Fake clocks start from a fixed epoch: `newFixedClock()` returns `clockwork.NewFakeClockAt(testEpoch)`. Never seed one from `time.Now()`, and date fixtures (stamps, activity times) from the same epoch. `clockwork.NewFakeClock()` is `NewFakeClockAt(time.Now())`, so the `fake-clock-epoch` rule bans it in unit tests.

When a context may be clock-driven, check `ctx.Done()` rather than calling `ctx.Err()`, because a clock-driven context's `Err` blocks until the context is done. A package-level variable that exists only so tests can shrink an interval goes back to being a `const`; the fake clock moves past the real value instead.

Tests move time with `driveClock`. Both converted packages define their own copy of it in `helpers_test.go`:

```go
// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}
```

This replaced an 8 s real poll (bucket L):

```go
func TestAcceptWorkspaceTrustDialog_InvalidSession(t *testing.T) {
	t.Parallel()
	pane := &fakePane{dead: true}
	s, err := runDialog(t, pane, func(tm *Tmux) error { return tm.AcceptWorkspaceTrustDialog("gt-x") })
	if err != nil {
		t.Fatalf("expected nil error for nonexistent session, got: %v", err)
	}
	if n := len(s.find("capture-pane")); n < 2 {
		t.Errorf("capture-pane calls = %d, want retries across the poll window", n)
	}
}
```

`driveClock` advances whenever anything is waiting on the clock, so it suits code that has one sleeper at a time. With two sleepers, such as a context deadline plus a sleep, it can move time past the second before the goroutine the first one woke has run. In that case wait for both and advance by hand:

```go
if err := clk.BlockUntilContext(t.Context(), 2); err != nil {
	t.Fatal(err)
}
clk.Advance(2 * time.Second)
```

## Fakes and contracts

A fake that stands in for a collaborator in other packages' tests lives in its own `<pkg>fake` package, for example `internal/tmux/tmuxfake`. It exports a contract suite in a non-test file so that both tiers can import it:

```go
// Sessions is the session-lifecycle surface that both the fake and *tmux.Tmux provide.
type Sessions interface { /* ... */ }

// RunSessionsContract checks the behavior every Sessions implementation must share.
func RunSessionsContract(t *testing.T, newImpl func(t *testing.T) Sessions)
```

Run the contract in both tiers. In the unit tier, run it against the fake:

```go
func TestFakeSessionsContract(t *testing.T) {
	t.Parallel()
	RunSessionsContract(t, func(t *testing.T) Sessions { return New(clockwork.NewFakeClock()) })
}
```

In the integration tier, run it against the real implementation (`internal/tmux/contract_integration_test.go`):

```go
//go:build integration

func TestIntegrationSessionsContract(t *testing.T) {
	tmuxfake.RunSessionsContract(t, func(t *testing.T) tmuxfake.Sessions {
		tm := tmux.NewTmuxWithSocket(constants.TestSocketName("gt-test-contract"))
		t.Cleanup(func() { _ = tm.KillServer() })
		return tm
	})
}
```

Write the integration runner first. If a contract case fails against the real implementation, the contract is wrong: correct it to what the real system does, then make the fake copy it. Every behavior the fake claims should be pinned by a contract case. That includes the error and missing-object paths, not only the happy path: `RunSessionsContract` pins no-server, missing-session (kill, pane command, capture, send, environment), unset-variable and create-validation behavior.

## Converting a package

### 1. Triage every test into one bucket

- **T (translation):** the test checks what a method sends. Rewrite it as a unit test that asserts the recorded calls.
- **L (logic):** the test checks parsing, detection, decisions or timeouts over output. Rewrite it with canned output and the fake clock. Pure functions only need `t.Parallel()`.
- **R (real):** the behavior exists only in the real system, such as tmux's `-S` history window, a hook firing, a process tree, or socket ownership. Move the test to an `_integration_test.go` file and rename it `TestIntegration<OldName>`. If it is about the fake's surface, add a case to the contract instead.
- **D (delete):** the test re-enacts something a T, L or R test already covers, exercises no production code, or asserts nothing. Record why. Every deleted test is listed by name, with its reason, in the commit that deletes it and in the MR body.

Keep the triage as a TSV (`file`, `test`, `bucket`, `reason`). The MR body is built from it.

A test that only `t.Log`s a mismatch, or that discards the error it is supposedly checking, asserts nothing. Its replacement must assert.

### 2. Convert one file or group per commit

After each commit, run:

```bash
go test -count=1 ./internal/<pkg>/...
go test ./internal/testpolicy/ -run TestPolicy -count=1   # with the package temporarily unlisted
```

The file is finished when the policy prints nothing for it. Name the commit `<pkg>: convert <file> tests (T:n L:n R:n D:n)`.

### 3. Fix production bugs you find, test-first

A conversion often exposes a real bug: a seam that leaks, a timeout too tight for a loaded host, or a race. Fix it in the same branch:

1. Write the failing test and confirm it fails with the error seen in production.
2. Fix the bug in its own commit, whose message says what broke, why, and the evidence.
3. List the fix in the MR body.

If the bug is in another package, do not fix it there. Report it with `file:line` and the evidence.

Four bugs were found this way in `internal/tmux`:

- The gt-h9z socket probe allowed 1 s, which refused healthy servers under load. It now allows 5 s, and a test runs the slow server on the fake clock.
- `killSplitBrainSession` bypassed the injected runner.
- `WaitForIdle` never noticed a vanished session on real tmux, because tmux says "can't find pane". It polled out its whole timeout. It now stops at once (`ErrPaneNotFound`).
- A socket dial that timed out under load was read as "can't tell". The dead-socket cleanup then left the file, and the new-session guard refused the create. A timed-out dial is now retried on the clock.

### 4. Zero flakes

A flaky test is a bug in the test or in the code. It is never retried, skipped or quarantined. There are no reruns in CI, no `t.Skip` for "environment could not supply the precondition", and no quarantine list.

Prove a fix under load before claiming it:

```bash
go test -count=20 ./internal/<pkg>/...                  # while another suite runs beside it
go test -race -shuffle=on -count=20 ./internal/<pkg>/...
```

Common sources of flakes, and their fixes:

| source | fix |
|---|---|
| process-wide state keyed by name (a lock map, a default socket) | give each parallel test its own key, for example `"gt-" + t.Name()` |
| a fixture that depends on the host's environment | make the fixture independent of it; the unit tier must also pass under `env -i` |
| a fixed deadline on real time | the fake clock; for a production timeout, the real margin a loaded host needs |

### 5. Take the package off the list

1. Delete the package's line from `internal/testpolicy/unconverted.txt`.
2. Lower `maxUnconverted` in `internal/testpolicy/policy_test.go` in the same commit, so the list can only shrink.
3. Run `go test ./internal/testpolicy/`. It must pass with no violation lines for the package.
4. Run `grep -c 'testpolicy:allow' internal/<pkg>/*_test.go`. It should total 0.
5. Measure the tiers:

```bash
go test -count=1 -cover ./internal/<pkg>/                    # unit coverage
make test-timing PKGS=./internal/<pkg>/...                   # unit tier, taxed pane: target 5 s or less
go test -tags integration -run '^TestIntegration' -count=1 ./internal/<pkg>/...   # target 60 s or less
```

Coverage may drop by no more than 2 points; justify any larger drop in the MR. `internal/tmux` went from 65.6% (the old suite, with real tmux) to 71.1% for the unit tier alone.

## One bead, one MR

Each package conversion is one child bead of the rewrite epic and one MR. Do not bundle two packages, and do not bundle unrelated fixes; a bug found during conversion is part of that package's MR (step 3).

## MR commit-body format

The last commit of the MR carries the summary in its body. Commit messages never carry a Co-Authored-By trailer or tool attribution.

```text
docs|<pkg>: <subject>

Buckets: T:<n> L:<n> R:<n> D:<n> (of <n> original tests)

Deleted:
- TestName: reason
- TestName: reason

Production bugs fixed:
- <hash> <subject>: <one line on cause and evidence>

Coverage: <before>% (old suite) -> <after>% (unit tier), <after>% (both tiers)
Timing: unit tier <n> s in make test-timing (probe <n> ms), integration <n> s
Flakes resolved: <test>: <cause> -> <fix>
```
