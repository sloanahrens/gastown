package bird

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// ttyEnv is an environment a shell would give gt bird start: no identity in it,
// so Plan is left to decide the launch.
var ttyEnv = []string{"PATH=/usr/bin:/bin"}

// fakeFile is an fs.FileInfo of the one thing checkClaude reads: whether the
// path is a directory and whether it can run.
type fakeFile struct {
	dir bool
	// mode is what the file's permissions read as; 0o755 runs, 0o644 does not.
	mode fs.FileMode
}

func (f fakeFile) Name() string       { return "claude" }
func (f fakeFile) Size() int64        { return 0 }
func (f fakeFile) Mode() fs.FileMode  { return f.mode }
func (f fakeFile) ModTime() time.Time { return time.Time{} }
func (f fakeFile) IsDir() bool        { return f.dir }
func (f fakeFile) Sys() any           { return nil }

// runnableClaude is the stat a start reads for a claude that is there and can
// run.
func runnableClaude(string) (fs.FileInfo, error) { return fakeFile{mode: 0o755}, nil }

// execCall is one replacement of the process: what the runner asked for.
type execCall struct {
	path string
	argv []string
	env  []string
}

// testRunner returns a Runner over a fresh registry whose exec records the call
// and returns instead of replacing the process. Nothing it touches is real:
// the clock, the process table, the file stat and the environment are all
// scripted, no bead is labeled, and every file it writes is under a temp dir.
func testRunner(t *testing.T, names ...string) (*Runner, *Registry, *[]execCall) {
	t.Helper()
	reg := testRegistry(t, names...)
	calls := &[]execCall{}
	r := &Runner{
		Registry: reg,
		Claude:   "/fake/bin/claude",
		Home:     townDir(reg),
		Terminal: Terminal{Stdin: true, Stdout: true},
		Environ:  func() []string { return slices.Clone(ttyEnv) },
		Stat:     runnableClaude,
		PID:      func() int { return 99 },
		Getwd:    func() (string, error) { return "/work", nil },
		Exec: func(path string, argv, env []string) error {
			*calls = append(*calls, execCall{path: path, argv: slices.Clone(argv), env: slices.Clone(env)})
			return nil
		},
	}
	return r, reg, calls
}

// lastCall fails the test unless the process was replaced exactly once and
// returns that call.
func lastCall(t *testing.T, calls []execCall) execCall {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("the process was replaced %d times, want once", len(calls))
	}
	return calls[0]
}

// agentNamed returns the recorded agent of name, if the state holds one.
func agentNamed(t *testing.T, reg *Registry, name string) (Agent, bool) {
	t.Helper()
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, a := range st.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

func TestStartClaimsTheNameRecordsTheBeadAndExecsClaude(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	reg.ProcStart = starts(map[int]string{99: "tok99"})

	// The exec is the last thing Start does, so everything the session's name
	// carries has to be on disk by the time claude runs in its place.
	r.Exec = func(path string, argv, env []string) error {
		*calls = append(*calls, execCall{path: path, argv: slices.Clone(argv), env: slices.Clone(env)})
		if _, ok := agentNamed(t, reg, "wren"); !ok {
			t.Error("claude ran before the session was recorded")
		}
		if name, ok, _ := reg.Bound("gt-a"); !ok || name != "wren" {
			t.Errorf("claude ran before the bead was bound: %q, %v", name, ok)
		}
		return nil
	}

	if err := r.Start([]string{"--bead", "gt-a", "fix the flaky test"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	call := lastCall(t, *calls)
	if call.path != "/fake/bin/claude" {
		t.Errorf("exec path = %q, want the real claude", call.path)
	}
	wantArgv := []string{"/fake/bin/claude", "--name", "wren", "fix the flaky test"}
	if !slices.Equal(call.argv, wantArgv) {
		t.Errorf("exec argv = %q, want %q", call.argv, wantArgv)
	}
	for _, want := range []string{agentNameVar + "=wren", "BEADS_ACTOR=town/crew/wren", "GIT_AUTHOR_NAME=wren"} {
		if !slices.Contains(call.env, want) {
			t.Errorf("exec env has no %s: %q", want, call.env)
		}
	}

	agent, ok := agentNamed(t, reg, "wren")
	if !ok {
		t.Fatal("no agent record was written for wren")
	}
	want := Agent{Name: "wren", PID: 99, Start: "tok99", Bead: "gt-a", Cwd: "/work", Started: at(0)}
	if agent != want {
		t.Errorf("agent record = %+v, want %+v", agent, want)
	}
	bound, ok, err := reg.Bound("gt-a")
	if err != nil || !ok || bound != "wren" {
		t.Errorf("Bound(gt-a) = %q, %v, %v; want wren", bound, ok, err)
	}
}

func TestStartLabelsTheBeadInItsOwnStore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		bead string
		dir  string
	}{
		{name: "a town bead is labeled in ~/gt", bead: "gt-a", dir: "gt"},
		{name: "a claude bead is labeled in ~/.claude", bead: "claude-a", dir: ".claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, reg, _ := testRunner(t, "wren")
			reg.ProcStart = starts(map[int]string{99: "tok99"})
			var dir, bead, label string
			r.LabelBead = func(d, b, l string) error {
				dir, bead, label = d, b, l
				return nil
			}
			if err := r.Start([]string{"--bead", tc.bead}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if want := filepath.Join(townDir(reg), tc.dir); dir != want {
				t.Errorf("labeled in %q, want %q", dir, want)
			}
			if bead != tc.bead || label != "agent:wren" {
				t.Errorf("labeled %q %q, want %q agent:wren", bead, label, tc.bead)
			}
		})
	}
}

func TestStartRunsClaudeWhenTheLabelCannotBeWritten(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	reg.ProcStart = starts(map[int]string{99: "tok99"})
	r.LabelBead = func(string, string, string) error { return errors.New("bd is not installed") }
	var errOut bytes.Buffer
	r.Stderr = &errOut

	if err := r.Start([]string{"--bead", "gt-a"}); err != nil {
		t.Fatalf("Start: %v, want the label failure to be a warning", err)
	}
	if _, ok := agentNamed(t, reg, "wren"); !ok {
		t.Error("the session was not recorded, so the label decided the start")
	}
	if !strings.Contains(errOut.String(), "not labeled agent:wren") {
		t.Errorf("stderr = %q, want a line saying the bead is not labeled", errOut.String())
	}
	lastCall(t, *calls)
}

func TestStartPassesClaudeThroughWithoutClaimingAName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "a print run belongs to its caller", args: []string{"-p", "hello"}, env: ttyEnv},
		{name: "a session already named keeps its identity", args: nil, env: append(slices.Clone(ttyEnv), "GT_ROLE=gastown/polecats/corpus")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, reg, calls := testRunner(t, "wren")
			r.Environ = func() []string { return slices.Clone(tc.env) }
			if err := r.Start(tc.args); err != nil {
				t.Fatalf("Start: %v", err)
			}
			call := lastCall(t, *calls)
			wantArgv := append([]string{"/fake/bin/claude"}, tc.args...)
			if !slices.Equal(call.argv, wantArgv) {
				t.Errorf("exec argv = %q, want the caller's own %q", call.argv, wantArgv)
			}
			if !slices.Equal(call.env, tc.env) {
				t.Errorf("exec env = %q, want the caller's own %q", call.env, tc.env)
			}
			if st, err := reg.Load(); err != nil || len(st.Agents) != 0 {
				t.Errorf("state = %+v, %v; want no agent recorded for a passthrough", st, err)
			}
		})
	}
}

// TestStartKeepsItsOwnFlagsFromClaude pins the wrapper's line: --bead is gt's,
// so a run that is not a session still hands claude the caller's other
// arguments without it.
func TestStartKeepsItsOwnFlagsFromClaude(t *testing.T) {
	t.Parallel()
	r, _, calls := testRunner(t, "wren")
	if err := r.Start([]string{"--bead", "gt-a", "-p", "fix it"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"/fake/bin/claude", "-p", "fix it"}
	if got := lastCall(t, *calls).argv; !slices.Equal(got, want) {
		t.Errorf("claude ran with %q, want %q: --bead is gt's flag", got, want)
	}
}

func TestStartRefusesABeadIdItCannotBind(t *testing.T) {
	t.Parallel()
	r, _, calls := testRunner(t, "wren")
	err := r.Start([]string{"--bead", "gt 1"})
	if err == nil || !strings.Contains(err.Error(), "bead id") {
		t.Fatalf("Start with an unreadable bead = %v, want the argument refused", err)
	}
	if len(*calls) != 0 {
		t.Error("claude ran with a bead id gt bird start could not read")
	}
}

func TestStartFailsWhenTheNameCannotBeRecorded(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	reg.Write = func(string, any) error { return errors.New("the state file is not writable") }
	err := r.Start(nil)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("Start = %v, want the registry's write failure", err)
	}
	if len(*calls) != 0 {
		t.Error("claude ran under a name the registry never recorded")
	}
	if st, loadErr := reg.Load(); loadErr != nil || len(st.Agents) != 0 {
		t.Errorf("state = %+v, %v; want no record for a claim that could not be written", st, loadErr)
	}
}

func TestStartFailsWhenTheBindingCannotBeRecorded(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	// The claim's write is the first; the binding's is the second, and only it
	// fails, so the failure lands between the two writes.
	writes := 0
	reg.Write = func(path string, v any) error {
		writes++
		if writes > 1 {
			return errors.New("the state file is not writable")
		}
		return atomicfile.EnsureDirAndWriteJSON(path, v)
	}
	err := r.Start([]string{"--bead", "gt-a"})
	if err == nil || !strings.Contains(err.Error(), "recording that") {
		t.Fatalf("Start = %v, want the binding's write failure", err)
	}
	if writes != 2 {
		t.Errorf("the state file was written %d times, want the claim's write and then the binding's", writes)
	}
	if len(*calls) != 0 {
		t.Error("claude ran without the bead it was started for")
	}
}

func TestStartFailsWhenTheProcessTableWillNotSayWhoRunsIt(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, errors.New("the process table is unreadable") }
	err := r.Start(nil)
	if err == nil || !strings.Contains(err.Error(), "start token") {
		t.Fatalf("Start = %v, want the start token read to stop it", err)
	}
	if len(*calls) != 0 {
		t.Error("claude ran without a recorded process")
	}
}

func TestStartRefusesAMissingClaudeWithStatus127(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	r.Stat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }

	err := r.Start(nil)
	var missing *MissingClaudeError
	if !errors.As(err, &missing) {
		t.Fatalf("Start = %v, want a MissingClaudeError", err)
	}
	if missing.ExitStatus() != 127 {
		t.Errorf("ExitStatus = %d, want 127", missing.ExitStatus())
	}
	if !strings.Contains(err.Error(), r.Claude) {
		t.Errorf("error %q does not name the binary %q", err, r.Claude)
	}
	if len(*calls) != 0 {
		t.Error("the process was replaced with a claude that is not there")
	}
	if st, loadErr := reg.Load(); loadErr != nil || len(st.Agents) != 0 {
		t.Errorf("state = %+v, %v; want no name claimed for a claude that cannot run", st, loadErr)
	}
}

func TestStartRefusesAClaudeThatIsNotExecutable(t *testing.T) {
	t.Parallel()
	for _, file := range []fakeFile{{mode: 0o644}, {dir: true, mode: 0o755}} {
		r, _, calls := testRunner(t, "wren")
		r.Stat = func(string) (fs.FileInfo, error) { return file, nil }
		err := r.Start(nil)
		var missing *MissingClaudeError
		if !errors.As(err, &missing) {
			t.Fatalf("Start with %+v = %v, want a MissingClaudeError", file, err)
		}
		if len(*calls) != 0 {
			t.Errorf("the process was replaced with %+v", file)
		}
	}
}

func TestStartGivesTwoOverlappingSessionsDifferentNames(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren", "robin")
	reg.ProcStart = starts(map[int]string{99: "tok99", 100: "tok100"})

	// The first start blocks inside its exec, which is where its name is already
	// recorded and the lock is already free: the second start runs while the
	// first session is up, exactly as two launches would overlap.
	firstExec := make(chan string, 1)
	release := make(chan struct{})
	r.Exec = func(_ string, argv, _ []string) error {
		// Reading the registry here fails if the lock were still held across
		// the exec, which is what would keep a second launch out.
		if _, err := reg.Load(); err != nil {
			t.Errorf("the registry is locked during the exec: %v", err)
		}
		firstExec <- argv[2]
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- r.Start(nil) }()
	firstName := <-firstExec

	second, _, secondCalls := testRunner(t, "wren", "robin")
	second.Registry = reg
	second.PID = func() int { return 100 }
	// The second start plans from the same registry, so it sees the name the
	// first session holds.
	if err := second.Start(nil); err != nil {
		t.Fatalf("the second start: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first start: %v", err)
	}

	secondName := lastCall(t, *secondCalls).argv[2]
	if firstName == secondName {
		t.Fatalf("both sessions claimed %q, want two distinct names", firstName)
	}
	for _, claimed := range []struct {
		name string
		pid  int
	}{{firstName, 99}, {secondName, 100}} {
		agent, ok := agentNamed(t, reg, claimed.name)
		if !ok {
			t.Fatalf("%s was launched but not recorded", claimed.name)
		}
		if agent.PID != claimed.pid {
			t.Errorf("%s is recorded under pid %d, want the session that claimed it (%d)", claimed.name, agent.PID, claimed.pid)
		}
	}
}

// TestConcurrentStartsTakeDifferentNames is the race the lock and the claim
// exist for: launches that all read the registry before any of them writes it.
// Each one that loses a name it chose asks Plan again for the next free one, so
// eight starts leave eight distinct sessions.
func TestConcurrentStartsTakeDifferentNames(t *testing.T) {
	t.Parallel()
	names := []string{"wren", "robin", "lark", "finch", "swift", "heron", "crane", "plover"}
	reg := testRegistry(t, names...)
	// Every launch here waits on the one registry lock, and the fake clock is
	// shared by all of them: one launch's retry would spend another's lock
	// budget. The real clock keeps each wait its own, and the lock is held for
	// microseconds at a time, so nothing here waits on wall time.
	reg.Now = nil
	reg.Sleep = nil
	// Every recorded session is live, as one is between its record and its own
	// exit; the map is read by every launch's pruning and liveness checks.
	var procsMu sync.Mutex
	procs := map[int]string{}
	reg.ProcStart = func(pid int) (string, bool, error) {
		procsMu.Lock()
		defer procsMu.Unlock()
		token, ok := procs[pid]
		return token, ok, nil
	}
	var started sync.WaitGroup
	var mu sync.Mutex
	var got []string
	for i := range names {
		pid := 100 + i
		procsMu.Lock()
		procs[pid] = fmt.Sprintf("tok%d", pid)
		procsMu.Unlock()
		started.Add(1)
		go func() {
			defer started.Done()
			r := &Runner{
				Registry: reg,
				Claude:   "/fake/bin/claude",
				Home:     townDir(reg),
				Terminal: Terminal{Stdin: true, Stdout: true},
				Environ:  func() []string { return nil },
				Stat:     runnableClaude,
				PID:      func() int { return pid },
				Exec: func(_ string, argv, _ []string) error {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, argv[2])
					return nil
				},
			}
			if err := r.Start(nil); err != nil {
				t.Errorf("start on pid %d: %v", pid, err)
			}
		}()
	}
	started.Wait()

	slices.Sort(got)
	if want := slices.Sorted(slices.Values(names)); !slices.Equal(got, want) {
		t.Fatalf("the starts took %q, want each of %q exactly once", got, want)
	}
}

func TestStartRefusesANameAPinnedSessionCannotTake(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren", "robin")
	reg.ProcStart = starts(map[int]string{10: "tok10"})
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok10"})

	err := r.Start([]string{"--name", "robin"})
	if err == nil || !strings.Contains(err.Error(), "robin is already running") {
		t.Fatalf("Start under the pinned name robin = %v, want a refusal naming robin", err)
	}
	if len(*calls) != 0 {
		t.Error("claude ran under a name another session holds")
	}
}

func TestBindBindsTheSessionNameAndSaysWhatTheBeadLastRanAs(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren", "lark")
	reg.ProcStart = starts(map[int]string{99: "tok99"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 99, Start: "tok99", Cwd: "/work", Started: at(0)})
	if err := reg.Update(func(st *State) error {
		st.Bind("lark", "gt-a", at(-5))
		return nil
	}); err != nil {
		t.Fatalf("seeding the binding: %v", err)
	}
	r.Environ = func() []string { return append(slices.Clone(ttyEnv), agentNameVar+"=wren") }
	var out bytes.Buffer
	r.Stdout = &out

	if err := r.Bind("gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	name, ok, err := reg.Bound("gt-a")
	if err != nil || !ok || name != "wren" {
		t.Errorf("Bound(gt-a) = %q, %v, %v; want the newest binding wren", name, ok, err)
	}
	agent, _ := agentNamed(t, reg, "wren")
	if agent.Bead != "gt-a" {
		t.Errorf("agent record bead = %q, want gt-a", agent.Bead)
	}
	if agent.PID != 99 || agent.Start != "tok99" || agent.Cwd != "/work" {
		t.Errorf("agent record = %+v, want the session's own pid, start and cwd kept", agent)
	}
	if !strings.Contains(out.String(), "lark") || !strings.Contains(out.String(), "gt-a") {
		t.Errorf("output = %q, want a line naming the bead's earlier name lark", out.String())
	}
}

func TestBindIsQuietWhenTheBeadLastRanAsThisSession(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren")
	if err := reg.Update(func(st *State) error {
		st.Bind("wren", "gt-a", at(0))
		return nil
	}); err != nil {
		t.Fatalf("seeding the binding: %v", err)
	}
	r.Environ = func() []string { return append(slices.Clone(ttyEnv), agentNameVar+"=wren") }
	var out bytes.Buffer
	r.Stdout = &out

	if err := r.Bind("gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing said about a bead this session already ran", out.String())
	}
}

func TestBindFailsWhenTheBindingCannotBeWritten(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren")
	reg.Write = func(string, any) error { return errors.New("the state file is not writable") }
	r.Environ = func() []string { return append(slices.Clone(ttyEnv), agentNameVar+"=wren") }
	if err := r.Bind("gt-a"); err == nil || !strings.Contains(err.Error(), "binding wren to gt-a") {
		t.Fatalf("Bind = %v, want the registry's write failure", err)
	}
}

func TestBindRefusesWithoutABirdName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		env   []string
		bead  string
		wants string
	}{
		{name: "no name in the environment", env: ttyEnv, bead: "gt-a", wants: agentNameVar},
		{name: "a name no session could use", env: append(slices.Clone(ttyEnv), agentNameVar+"=not a name"), bead: "gt-a", wants: "must match"},
		{name: "a bead id gt bird bind cannot read", env: append(slices.Clone(ttyEnv), agentNameVar+"=wren"), bead: "gt a", wants: "bead id matches"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, reg, _ := testRunner(t, "wren")
			r.Environ = func() []string { return slices.Clone(tc.env) }
			err := r.Bind(tc.bead)
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("Bind(%q) = %v, want an error naming %q", tc.bead, err, tc.wants)
			}
			if st, loadErr := reg.Load(); loadErr != nil || len(st.Bindings) != 0 {
				t.Errorf("state = %+v, %v; want nothing bound", st, loadErr)
			}
		})
	}
}

func TestListPrintsTheLiveBirdsAndDropsTheDead(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren", "robin")
	reg.ProcStart = starts(map[int]string{10: "tok10"})
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok10", Bead: "gt-a", Cwd: "/work"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 11, Start: "tok11", Cwd: "/elsewhere"})
	var out bytes.Buffer

	if err := r.List(&out); err != nil {
		t.Fatalf("List: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("List printed %q, want a header and the one live bird", out.String())
	}
	if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "CWD") {
		t.Errorf("first line = %q, want the column names", lines[0])
	}
	for _, want := range []string{"robin", "gt-a", "/work"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row = %q, want %q in it", lines[1], want)
		}
	}
	if _, ok := agentNamed(t, reg, "wren"); ok {
		t.Error("the dead bird wren is still recorded; list must prune it")
	}
}

func TestListFailsOnAStateFileItCannotRead(t *testing.T) {
	t.Parallel()
	r, reg, _ := testRunner(t, "wren")
	if err := os.MkdirAll(filepath.Dir(reg.Path), 0o755); err != nil {
		t.Fatalf("making room for the state file: %v", err)
	}
	if err := os.WriteFile(reg.Path, []byte("{ this is not JSON"), 0o644); err != nil {
		t.Fatalf("writing a corrupt state file: %v", err)
	}
	if err := r.List(&bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("List = %v, want the state file's read failure", err)
	}
}

func TestListSaysWhenNoBirdIsRunning(t *testing.T) {
	t.Parallel()
	r, _, _ := testRunner(t, "wren")
	var out bytes.Buffer
	if err := r.List(&out); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "no live birds" {
		t.Errorf("List printed %q, want %q", got, "no live birds")
	}
}
