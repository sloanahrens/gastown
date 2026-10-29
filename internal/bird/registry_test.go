// The tests in this file touch the disk: they read and write real state files,
// take a real flock on the lock file beside them, and read real directories.
// Each builds on testRegistry(t), which takes its own t.TempDir (or calls
// t.TempDir itself), so no test shares a directory, a state file or a lock with
// another. The seams a test drives in place of the kernel are the clock (Now and
// Sleep), the process table (ProcStart), the dice (Rand), the writer (Write) and
// the stat expandGlobStat reads a literal component with; nothing else is faked.
package bird

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// testRegistry returns a Registry over a fresh directory, with a scripted
// clock, a Rand that picks the first free name and no live processes. Sleep
// advances the clock instead of waiting, so a lock the test holds is waited
// out just as fast with no real time passing.
func testRegistry(t *testing.T, names ...string) *Registry {
	t.Helper()
	dir := t.TempDir()
	var mu sync.Mutex
	now := at(0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	reg := &Registry{
		Path:        filepath.Join(dir, ".runtime", "agents", "state.json"),
		Pool:        names,
		PolecatRoot: dir,
		PolecatGlob: filepath.Join("*", "polecats", "*"),
		Now:         clock,
		Sleep: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d)
		},
		Rand:      func(int) int { return 0 },
		ProcStart: starts(nil),
	}
	return reg
}

// townDir is the town root testRegistry built its paths under.
func townDir(reg *Registry) string {
	return reg.PolecatRoot
}

// starts returns a process-info function answering from procs; a pid absent
// from procs has no live process.
func starts(procs map[int]string) func(int) (string, bool, error) {
	return func(pid int) (string, bool, error) {
		token, ok := procs[pid]
		return token, ok, nil
	}
}

// seedAgent records an agent in the state file without going through the
// registry's own writers.
func seedAgent(t *testing.T, reg *Registry, a Agent) {
	t.Helper()
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, a)
		return nil
	}); err != nil {
		t.Fatalf("seeding an agent: %v", err)
	}
}

func TestPickSkipsBoundNames(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	if err := reg.Update(func(st *State) error {
		st.Bind("wren", "gt-a", at(0))
		return nil
	}); err != nil {
		t.Fatalf("seeding a binding: %v", err)
	}
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "robin" {
		t.Errorf("Pick = %q, want the unbound robin", got.Name)
	}
}

func TestPickSkipsLiveAgents(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "robin", "wren")
	reg.ProcStart = starts(map[int]string{10: "tok"})
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "wren" {
		t.Errorf("Pick = %q, want the name of no live process", got.Name)
	}
}

func TestPickSkipsPolecatDirectories(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "lark", "wren")
	seated := filepath.Join(townDir(reg), "gastown", "polecats", "lark")
	if err := os.MkdirAll(seated, 0o755); err != nil {
		t.Fatalf("creating a polecat directory: %v", err)
	}
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "wren" {
		t.Errorf("Pick = %q, want a name no polecat directory holds", got.Name)
	}
}

func TestPickDrawsFromTheEmbeddedPoolByDefault(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t)
	got, err := reg.Pick()
	if err != nil || got.Name == "" {
		t.Fatalf("Pick = %+v, %v, want a name from the embedded pool", got, err)
	}
	if want := Pool()[0]; got.Name != want {
		t.Errorf("Pick = %q, want the first embedded name %q", got.Name, want)
	}
}

func TestPickChoosesThroughTheInjectedSource(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin", "lark")
	reg.Rand = func(n int) int {
		if n != 3 {
			t.Errorf("Rand called with %d free names, want 3", n)
		}
		return 2
	}
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "lark" {
		t.Errorf("Pick = %q, want the name the source selected", got.Name)
	}
}

func TestPickSkipsNamesNoSessionCouldUse(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "not a name", "wren")
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "wren" {
		t.Errorf("Pick = %q, want the one usable name", got.Name)
	}
}

// TestPickReportsWhyItPassedANameOver is the explanation Pick owes a launch
// that got its second choice: each case takes one name out of a pool of two, and
// the result must name the free one it took and the taken one with the reason.
func TestPickReportsWhyItPassedANameOver(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		taken   string
		arrange func(t *testing.T, reg *Registry)
		want    SkipReason
	}{
		{
			name:  "a name no session could use",
			taken: "not a name",
			want:  SkipUnusable,
		},
		{
			name:  "a name bound to a bead",
			taken: "robin",
			arrange: func(t *testing.T, reg *Registry) {
				t.Helper()
				if _, err := reg.Bind("robin", "gt-a"); err != nil {
					t.Fatalf("binding robin: %v", err)
				}
			},
			want: SkipBound,
		},
		{
			name:  "a name an earlier pick reserved",
			taken: "robin",
			arrange: func(t *testing.T, reg *Registry) {
				t.Helper()
				if got, err := reg.Pick(); err != nil || got.Name != "robin" {
					t.Fatalf("the first Pick = %+v, %v, want robin reserved", got, err)
				}
			},
			want: SkipReserved,
		},
		{
			name:  "a name a live process holds",
			taken: "robin",
			arrange: func(t *testing.T, reg *Registry) {
				t.Helper()
				reg.ProcStart = starts(map[int]string{10: "tok"})
				seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
			},
			want: SkipLiveProcess,
		},
		{
			name:  "a name a process holds that no start token is matched to",
			taken: "robin",
			arrange: func(t *testing.T, reg *Registry) {
				t.Helper()
				reg.ProcStart = func(int) (string, bool, error) { return "", true, nil }
				seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
			},
			want: SkipLiveProcess,
		},
		{
			name:  "a name a polecat directory holds",
			taken: "robin",
			arrange: func(t *testing.T, reg *Registry) {
				t.Helper()
				seated := filepath.Join(townDir(reg), "gastown", "polecats", "robin")
				if err := os.MkdirAll(seated, 0o755); err != nil {
					t.Fatalf("creating a polecat directory: %v", err)
				}
			},
			want: SkipPolecatDirectory,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, tc.taken, "wren")
			if tc.arrange != nil {
				tc.arrange(t, reg)
			}
			got, err := reg.Pick()
			if err != nil {
				t.Fatalf("Pick: %v", err)
			}
			if got.Name != "wren" {
				t.Errorf("Pick took %q, want the free wren", got.Name)
			}
			want := []Skip{{Name: tc.taken, Reason: tc.want}}
			if !slices.Equal(got.Skips, want) {
				t.Errorf("Pick skips = %+v, want %+v", got.Skips, want)
			}
		})
	}
}

// TestPickKeepsAProcessWhoseStartTokenItCannotRead pins the decision Live
// documents: a live pid whose start token this process cannot read (a process
// under another uid) reads as live, so Pick passes its name over and the record
// survives until the pid exits. A registry that pruned it would free the name
// for a second launch while the first bird may still be running.
func TestPickKeepsAProcessWhoseStartTokenItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "robin", "wren")
	reg.ProcStart = func(int) (string, bool, error) { return "", true, nil }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "wren" {
		t.Errorf("Pick took %q, want wren: robin is held by a live process whose start token cannot be read", got.Name)
	}
	if want := []Skip{{Name: "robin", Reason: SkipLiveProcess}}; !slices.Equal(got.Skips, want) {
		t.Errorf("Pick skips = %+v, want %+v", got.Skips, want)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != "robin" {
		t.Errorf("agents on disk = %+v, want robin kept until its process exits", st.Agents)
	}
}

func TestPickReservesTheNameItReturns(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	first, err := reg.Pick()
	if err != nil || first.Name == "" {
		t.Fatalf("first Pick = %+v, %v, want a name", first, err)
	}
	second, err := reg.Pick()
	if err != nil || second.Name == "" {
		t.Fatalf("second Pick = %+v, %v, want a name", second, err)
	}
	if second.Name == first.Name {
		t.Errorf("two Picks returned %q, want the first one reserved so a second launch gets another name", first.Name)
	}
}

func TestPickHandsOutANameAgainOnceItsReservationExpires(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ReservationTTL = 5 * time.Minute
	if got, err := reg.Pick(); err != nil || got.Name != "wren" {
		t.Fatalf("Pick = %+v, %v, want wren from a pool of one", got, err)
	}
	reg.Now = func() time.Time { return at(10) }
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick after the ttl: %v", err)
	}
	if got.Name != "wren" {
		t.Errorf("Pick = %q, want a launcher that died to leave no name held", got.Name)
	}
}

func TestTakenRefusesABadPolecatGlob(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.PolecatGlob = "["
	if _, err := reg.Taken(); err == nil {
		t.Fatal("Taken accepted a polecat glob that cannot match")
	}
}

func TestTakenHoldsAPolecatDirectoryName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "lark")
	seated := filepath.Join(townDir(reg), "gastown", "polecats", "lark")
	if err := os.MkdirAll(seated, 0o755); err != nil {
		t.Fatalf("creating a polecat directory: %v", err)
	}
	taken, err := reg.Taken()
	if err != nil {
		t.Fatalf("Taken: %v", err)
	}
	if !taken["lark"] {
		t.Errorf("taken = %v, want the polecat directory's name held", taken)
	}
}

func TestPolecatNamesCountsNothingWithNoGlob(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.PolecatGlob = ""
	names, err := reg.PolecatNames()
	if err != nil {
		t.Fatalf("PolecatNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("PolecatNames = %v, want none for a registry with no glob", names)
	}
}

func TestRegistryTakenReportsALivenessItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	unreadable := errors.New("no process table")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, unreadable }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	if _, err := reg.Taken(); !errors.Is(err, unreadable) {
		t.Fatalf("Taken = %v, want the read error rather than a name handed out on a guess", err)
	}
}

func TestPickRefusesABadPolecatGlob(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.PolecatGlob = "["
	if _, err := reg.Pick(); err == nil {
		t.Fatal("Pick accepted a polecat glob that cannot match")
	}
}

func TestPickReportsALivenessItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	unreadable := errors.New("no process table")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, unreadable }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	if got, err := reg.Pick(); !errors.Is(err, unreadable) {
		t.Fatalf("Pick = %+v, %v, want the read error rather than a guess", got, err)
	}
}

func TestNewReadsTheTownRootLiterally(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "gt [x] *?")
	seated := filepath.Join(root, "gastown", "polecats", "wren")
	if err := os.MkdirAll(seated, 0o755); err != nil {
		t.Fatalf("creating a polecat directory: %v", err)
	}
	reg := New(root)
	reg.Pool = []string{"wren", "robin"}
	reg.Rand = func(int) int { return 0 }
	reg.ProcStart = starts(nil)
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "robin" {
		t.Errorf("Pick = %q, want robin: the root's metacharacters must not hide the polecat directory wren", got.Name)
	}
}

func TestPolecatNamesSkipsARootThatIsNotThere(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.PolecatRoot = filepath.Join(t.TempDir(), "missing")
	names, err := reg.PolecatNames()
	if err != nil {
		t.Fatalf("PolecatNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("PolecatNames = %v, want none under a root that does not exist", names)
	}
}

func TestPolecatNamesSkipsARigWithNoPolecatDirectory(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := os.MkdirAll(filepath.Join(townDir(reg), "emptyrig"), 0o755); err != nil {
		t.Fatalf("creating a rig directory: %v", err)
	}
	names, err := reg.PolecatNames()
	if err != nil {
		t.Fatalf("PolecatNames: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("PolecatNames = %v, want none for a rig with no polecat directory", names)
	}
}

func TestPolecatNamesReportsARootItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	notADir := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing a regular file in place of the town root: %v", err)
	}
	reg.PolecatRoot = notADir
	if _, err := reg.PolecatNames(); err == nil {
		t.Fatal("PolecatNames accepted a town root that is a regular file")
	}
}

func TestBoundReadsThroughTheRegistry(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	if _, err := reg.Bind("wren", "gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	reg.Now = func() time.Time { return at(5) }
	if _, err := reg.Bind("robin", "gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	name, ok, err := reg.Bound("gt-a")
	if err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if !ok || name != "robin" {
		t.Errorf("Bound(gt-a) = %q, %v, want the newest name robin", name, ok)
	}
}

func TestBindLeavesTheStateFileAloneForAPairItAlreadyHas(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if _, err := reg.Bind("wren", "gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	writes := 0
	reg.Write = func(string, any) error {
		writes++
		return nil
	}
	added, err := reg.Bind("wren", "gt-a")
	if err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if added {
		t.Error("second Bind reported a pair it already had as added")
	}
	if writes != 0 {
		t.Errorf("a repeat Bind wrote the state file %d time(s), want none: nothing changed", writes)
	}
}

func TestRecordWritesTheAgentOfItsName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := reg.Record(Agent{Name: "wren", PID: 10, Start: "tok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	reg.Now = func() time.Time { return at(5) }
	if err := reg.Record(Agent{Name: "wren", PID: 11, Start: "tok2", Bead: "gt-a", Cwd: "/town"}); err != nil {
		t.Fatalf("second Record: %v", err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 {
		t.Fatalf("agents = %+v, want the one record of the name", st.Agents)
	}
	got := st.Agents[0]
	if got.PID != 11 || got.Start != "tok2" || got.Bead != "gt-a" || got.Cwd != "/town" {
		t.Errorf("agent = %+v, want the newer record to replace the one for the same name", got)
	}
	if !got.Started.Equal(at(5)) {
		t.Errorf("started = %v, want the clock's stamp at(5)", got.Started)
	}
}

func TestRecordDropsTheReservationForItsName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	picked, err := reg.Pick()
	if err != nil || picked.Name == "" {
		t.Fatalf("Pick = %+v, %v, want a name", picked, err)
	}
	name := picked.Name
	if err := reg.Record(Agent{Name: name, PID: 10, Start: "tok"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Reservations) != 0 {
		t.Errorf("reservations = %+v, want none for a name a process now holds", st.Reservations)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != name {
		t.Errorf("agents = %+v, want the record for %s", st.Agents, name)
	}
}

func TestRecordRefusesAnAgentThatCannotHoldAName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		agent Agent
		want  error
	}{
		{"a name no session could use", Agent{Name: "not a name", PID: 10}, ErrInvalidName},
		{"a record with no pid", Agent{Name: "wren"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, "wren")
			err := reg.Record(tc.agent)
			if err == nil {
				t.Fatalf("Record(%+v) = nil, want the record refused", tc.agent)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Record(%+v) = %v, want %v", tc.agent, err, tc.want)
			}
			st, err := reg.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(st.Agents) != 0 {
				t.Errorf("agents = %+v, want nothing written for a refused record", st.Agents)
			}
		})
	}
}

func TestClaimRefusesANameALiveSessionHolds(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "tok10", 11: "tok11"})
	claimed, err := reg.Claim(Agent{Name: "wren", PID: 10, Start: "tok10"})
	if err != nil || !claimed {
		t.Fatalf("the first Claim = %v, %v, want the name taken", claimed, err)
	}
	claimed, err = reg.Claim(Agent{Name: "wren", PID: 11, Start: "tok11"})
	if err != nil {
		t.Fatalf("the second Claim: %v", err)
	}
	if claimed {
		t.Error("the second Claim took a name a live session holds")
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 || st.Agents[0].PID != 10 {
		t.Errorf("agents = %+v, want only the session that took the name", st.Agents)
	}
}

// TestClaimRefusesANameAnotherLaunchReserved pins that a reservation holds its
// name against a claim: a name Pick handed out to one launch is not free for
// another to claim before that launch binds it.
func TestClaimRefusesANameAnotherLaunchReserved(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if _, err := reg.Pick(); err != nil {
		t.Fatalf("Pick: %v", err)
	}
	reg.ProcStart = starts(map[int]string{11: "tok11"})

	claimed, err := reg.Claim(Agent{Name: "wren", PID: 11, Start: "tok11"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed {
		t.Error("Claim took the name a live reservation holds")
	}
}

func TestClaimTakesANameWhoseSessionHasExited(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok10", Started: at(0)})
	reg.ProcStart = starts(map[int]string{11: "tok11"})

	claimed, err := reg.Claim(Agent{Name: "wren", PID: 11, Start: "tok11"})
	if err != nil || !claimed {
		t.Fatalf("Claim = %v, %v, want the name of an exited session", claimed, err)
	}
	if got, _ := reg.Load(); len(got.Agents) != 1 || got.Agents[0].PID != 11 {
		t.Errorf("agents = %+v, want the new session replacing the dead one", got.Agents)
	}
}

// TestClaimLetsASessionResumeABoundName pins the reason a binding is not a
// claim: the session that resumes a bead runs under the name the bead is bound
// to, which no process holds until that session starts.
func TestClaimLetsASessionResumeABoundName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := reg.Update(func(st *State) error {
		st.Bind("wren", "gt-a", at(0))
		return nil
	}); err != nil {
		t.Fatalf("seeding the binding: %v", err)
	}
	reg.ProcStart = starts(map[int]string{10: "tok10"})

	claimed, err := reg.Claim(Agent{Name: "wren", PID: 10, Start: "tok10", Bead: "gt-a"})
	if err != nil || !claimed {
		t.Fatalf("Claim of the bound name = %v, %v, want it taken by the resuming session", claimed, err)
	}
}

// TestClaimFailsWhenTheRegistryCannotBeReadOrWritten drives the two failures a
// claim answers with rather than guessing: a process table that will not say
// whether a name is held, and a state file that will not take the record.
func TestClaimFailsWhenTheRegistryCannotBeReadOrWritten(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		set  func(*Registry)
	}{
		{name: "a process table that cannot be read", set: func(reg *Registry) {
			seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok10"})
			reg.ProcStart = func(int) (string, bool, error) {
				return "", false, errors.New("the process table is unreadable")
			}
		}},
		{name: "a state file that cannot be written", set: func(reg *Registry) {
			reg.Write = func(string, any) error { return errors.New("the state file is not writable") }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, "wren")
			tc.set(reg)
			claimed, err := reg.Claim(Agent{Name: "wren", PID: 11, Start: "tok11"})
			if err == nil {
				t.Fatalf("Claim = %v, want the registry's failure", claimed)
			}
			if claimed {
				t.Error("Claim reported a name it could not record")
			}
		})
	}
}

func TestClaimRefusesANameNoSessionCouldUse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		agent Agent
		want  error
	}{
		{name: "a name no session could use", agent: Agent{Name: "not a name", PID: 10}, want: ErrInvalidName},
		{name: "a record with no pid", agent: Agent{Name: "wren"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, "wren")
			claimed, err := reg.Claim(tc.agent)
			if err == nil {
				t.Fatalf("Claim(%+v) = %v, want the claim refused", tc.agent, claimed)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("Claim(%+v) = %v, want %v", tc.agent, err, tc.want)
			}
			if st, loadErr := reg.Load(); loadErr != nil || len(st.Agents) != 0 {
				t.Errorf("agents = %+v, %v; want nothing written for a refused claim", st, loadErr)
			}
		})
	}
}

func TestBindRefusesANameNoSessionCouldUse(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	added, err := reg.Bind("not a name", "gt-a")
	if !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Bind = %v, %v, want the invalid name refused", added, err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Bindings) != 0 {
		t.Errorf("bindings = %+v, want nothing written for a refused name", st.Bindings)
	}
}

func TestOwnsAcceptsPoolAndBoundNames(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if _, err := reg.Bind("robin", "gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	cases := []struct {
		name string
		want bool
	}{
		{"wren", true},
		{"robin", true},
		{"lark", false},
	}
	for _, tc := range cases {
		got, err := reg.Owns(tc.name)
		if err != nil {
			t.Fatalf("Owns(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("Owns(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPickReturnsNoNameWhenThePoolIsExhausted(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	if err := reg.Update(func(st *State) error {
		st.Bind("wren", "gt-a", at(0))
		st.Bind("robin", "gt-b", at(0))
		return nil
	}); err != nil {
		t.Fatalf("seeding bindings: %v", err)
	}
	got, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Pick = %q, want no name from an exhausted pool", got.Name)
	}
}

func TestListPrunesADeadAgent(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("List = %+v, want no live agents", live)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 0 {
		t.Errorf("agents on disk = %+v, want the dead agent pruned", st.Agents)
	}
}

func TestListPrunesAnAgentWhosePIDWasReused(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "stranger"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("List = %+v, want the agent with a different start token pruned", live)
	}
}

func TestListKeepsALiveAgent(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "tok"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Name != "wren" {
		t.Errorf("List = %+v, want the live agent wren", live)
	}
}

func TestListReturnsTheLiveAgentsByName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "tok", 11: "tok"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	seedAgent(t, reg, Agent{Name: "heron", PID: 11, Start: "tok", Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 2 || live[0].Name != "heron" || live[1].Name != "wren" {
		t.Errorf("List = %+v, want heron and wren in name order", live)
	}
}

func TestListKeepsAnAgentWhoseLivenessItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	unreadable := errors.New("no process table")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, unreadable }
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	if _, err := reg.List(); !errors.Is(err, unreadable) {
		t.Fatalf("List = %v, want the read error", err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 {
		t.Errorf("agents on disk = %+v, want the agent kept: an unreadable process is not a dead one", st.Agents)
	}
}

func TestListKeepsAnAgentWithNoStartTokenWhileItsPIDLives(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "tok"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Name != "wren" {
		t.Errorf("List = %+v, want the agent with no token kept while its pid is there", live)
	}
}

func TestListPrunesAnAgentWithNoPID(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	seedAgent(t, reg, Agent{Name: "wren", Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("List = %+v, want a record with no pid pruned", live)
	}
}

func TestListPrunesAnAgentWithNoStartTokenWhosePIDIsGone(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Started: at(0)})
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("List = %+v, want the agent with no token pruned when its pid names no process", live)
	}
}

func TestTakenReadsAnAliveProcessWhoseTokenItCannotHave(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = func(int) (string, bool, error) { return "", true, nil }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	taken, err := reg.Taken()
	if err != nil {
		t.Fatalf("Taken: %v, want no error for a token the kernel will not give", err)
	}
	if !taken["robin"] {
		t.Errorf("taken = %v, want a process that is there to hold its name", taken)
	}
}

func TestListLeavesTheStateFileAloneWhenItPrunesNothing(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ProcStart = starts(map[int]string{10: "tok"})
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
	writes := 0
	reg.Write = func(string, any) error {
		writes++
		return nil
	}
	if _, err := reg.List(); err != nil {
		t.Fatalf("List: %v", err)
	}
	if writes != 0 {
		t.Errorf("List wrote the state file %d time(s), want none for a sweep that pruned nothing", writes)
	}
}

func TestStateRoundTripsAnAwkwardCwd(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	cwd := `/home/ann/"quoted dir" & more/back\slash, end`
	seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok", Cwd: cwd, Started: at(0)})
	data, err := os.ReadFile(reg.Path)
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("state file is not valid JSON: %s", data)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("decoding the state file: %v", err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Cwd != cwd {
		t.Errorf("agents = %+v, want the cwd %q back", st.Agents, cwd)
	}
}

// TestAWriterWaitingOnTheLockKeepsEveryEntry is the lock hand-off without
// goroutines or real waiting: writer B finds the lock held, its Sleep seam
// releases it (the holder finishing), and B must then write on top of what is
// there, not over it (gt-ed525).
func TestAWriterWaitingOnTheLockKeepsEveryEntry(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	if _, err := reg.Bind("robin", "gt-1"); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	held := flock.New(reg.Path + ".lock")
	if err := held.Lock(); err != nil {
		t.Fatalf("taking the lock for the test: %v", err)
	}
	waits := 0
	clockSleep := reg.Sleep
	reg.Sleep = func(d time.Duration) {
		waits++
		if err := held.Unlock(); err != nil {
			t.Errorf("releasing the lock: %v", err)
		}
		clockSleep(d)
	}
	if _, err := reg.Bind("wren", "gt-2"); err != nil {
		t.Fatalf("second writer: %v", err)
	}
	if waits == 0 {
		t.Fatalf("the second writer never waited: the test did not hold the lock against it")
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	bound := make(map[string]bool, len(st.Bindings))
	for _, b := range st.Bindings {
		bound[b.Name] = true
	}
	for _, name := range []string{"wren", "robin"} {
		if !bound[name] {
			t.Errorf("bindings = %+v, want the entry for %s not lost", st.Bindings, name)
		}
	}
}

// TestAFileInTheTownRootIsNoPolecatRig keeps the fail-closed read from failing
// in a real town: the root holds plain files beside the rig directories, and a
// file has no polecats under it (ENOTDIR), which is a miss and not an error
// (gt-ed525).
func TestAFileInTheTownRootIsNoPolecatRig(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := os.WriteFile(filepath.Join(reg.PolecatRoot, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(reg.PolecatRoot, "norigpolecats"), 0o755); err != nil {
		t.Fatalf("building a rig with no polecats: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(reg.PolecatRoot, "rig", "polecats", "furiosa"), 0o755); err != nil {
		t.Fatalf("building the rig: %v", err)
	}
	taken, err := reg.Taken()
	if err != nil {
		t.Fatalf("Taken with a plain file and a rig without polecats in the root: %v", err)
	}
	if !taken["furiosa"] {
		t.Errorf("Taken = %v, want the polecat furiosa", taken)
	}
}

// TestAStatErrorOtherThanMissingIsReported pins the fail-closed read: a literal
// component whose stat fails for any reason but "not there" (a directory above
// it that cannot be searched) is an error, never a taken set without the
// polecat directories under it (gt-ed525).
func TestAStatErrorOtherThanMissingIsReported(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "rig", "polecats", "furiosa"), 0o755); err != nil {
		t.Fatalf("building the fixture: %v", err)
	}
	denied := func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission }
	if got, err := expandGlobStat(root, filepath.Join("*", "polecats", "*"), denied); err == nil {
		t.Errorf("expandGlobStat = %v with no error, want the stat failure reported", got)
	}
}

func TestARegistryGivesUpOnALockItCannotTake(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.LockTimeout = 50 * time.Millisecond
	if err := os.MkdirAll(filepath.Dir(reg.Path), 0o755); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}
	held := flock.New(reg.Path + ".lock")
	if err := held.Lock(); err != nil {
		t.Fatalf("taking the lock for the test: %v", err)
	}
	if _, err := reg.Load(); err == nil {
		t.Errorf("Load waited out a lock the test holds, want it to give up after %s", reg.LockTimeout)
	}
	if err := held.Unlock(); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	if _, err := reg.Load(); err != nil {
		t.Errorf("Load after the lock was released: %v", err)
	}
}

func TestLoadReportsAPathThatIsADirectory(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := os.MkdirAll(reg.Path, 0o755); err != nil {
		t.Fatalf("creating a directory at the state path: %v", err)
	}
	if _, err := reg.Load(); err == nil {
		t.Fatal("Load accepted a directory as the state file")
	}
}

func TestLoadReportsADirectoryItCannotCreate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing a regular file in place of the directory: %v", err)
	}
	reg := testRegistry(t, "wren")
	reg.Path = filepath.Join(notADir, "state.json")
	if _, err := reg.Load(); err == nil {
		t.Fatal("Load accepted a state path under a regular file")
	}
}

func TestLoadReportsALockFileItCannotOpen(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.LockPath = filepath.Join(t.TempDir(), strings.Repeat("x", 300))
	if _, err := reg.Load(); err == nil {
		t.Fatal("Load accepted a lock file the kernel cannot open")
	}
}

func TestUpdateWritesNothingWhenFnFails(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if _, err := reg.Bind("wren", "gt-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	boom := errors.New("boom")
	err := reg.Update(func(st *State) error {
		st.Bind("robin", "gt-b", at(1))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update = %v, want the error fn returned", err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Bindings) != 1 {
		t.Errorf("bindings = %+v, want the state from before the failed update", st.Bindings)
	}
}

func TestUpdateReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.Write = func(string, any) error { return errors.New("disk full") }
	err := reg.Update(func(st *State) error {
		st.Bind("wren", "gt-a", at(0))
		return nil
	})
	if err == nil {
		t.Fatal("Update accepted a write that failed")
	}
	if _, statErr := os.Stat(reg.Path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("state file stat = %v after a failed write, want it never written", statErr)
	}
}

func TestEveryReadOfABrokenStateFails(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := os.MkdirAll(filepath.Dir(reg.Path), 0o755); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}
	if err := os.WriteFile(reg.Path, []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("writing a broken state file: %v", err)
	}
	if _, err := reg.Load(); err == nil {
		t.Error("Load accepted a broken state file")
	}
	if err := reg.Update(func(*State) error { return nil }); err == nil {
		t.Error("Update wrote over a broken state file")
	}
	if _, err := reg.List(); err == nil {
		t.Error("List accepted a broken state file")
	}
	if _, _, err := reg.Bound("gt-a"); err == nil {
		t.Error("Bound accepted a broken state file")
	}
	if _, err := reg.Owns("lark"); err == nil {
		t.Error("Owns accepted a broken state file")
	}
	if _, err := reg.Taken(); err == nil {
		t.Error("Taken accepted a broken state file")
	}
	if _, err := reg.Pick(); err == nil {
		t.Error("Pick accepted a broken state file")
	}
}

func TestStartTokenAnswersFromTheTwoKernelCalls(t *testing.T) {
	t.Parallel()
	boom := errors.New("probe failed")
	cases := []struct {
		name    string
		pid     int
		token   func(int) (string, bool)
		probe   func(int) (bool, error)
		want    string
		wantOK  bool
		wantErr bool
	}{
		{
			name:  "a token the kernel gave",
			pid:   10,
			token: func(int) (string, bool) { return "tok", true },
			probe: func(int) (bool, error) {
				t.Error("probe called although the token was read")
				return false, nil
			},
			want: "tok", wantOK: true,
		},
		{
			name:  "no such process",
			pid:   10,
			token: func(int) (string, bool) { return "", false },
			probe: func(int) (bool, error) { return false, nil },
		},
		{
			name:   "alive but no token",
			pid:    10,
			token:  func(int) (string, bool) { return "", false },
			probe:  func(int) (bool, error) { return true, nil },
			wantOK: true,
		},
		{
			name:    "the probe itself failed",
			pid:     10,
			token:   func(int) (string, bool) { return "", false },
			probe:   func(int) (bool, error) { return false, boom },
			wantErr: true,
		},
		{
			name:    "a platform whose probe has no answer",
			pid:     10,
			token:   func(int) (string, bool) { return "", false },
			probe:   func(int) (bool, error) { return false, ErrUnsupportedPlatform },
			wantErr: true,
		},
		{
			name: "a pid that names no process at all",
			pid:  0,
			token: func(int) (string, bool) {
				t.Error("token read for a pid that cannot name a process")
				return "", false
			},
			probe: func(int) (bool, error) {
				t.Error("probe called for a pid that cannot name a process")
				return false, nil
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, ok, err := tokenFrom(tc.pid, tc.token, tc.probe)
			if (err != nil) != tc.wantErr {
				t.Fatalf("tokenFrom = %q, %v, %v, want error %v", token, ok, err, tc.wantErr)
			}
			if token != tc.want || ok != tc.wantOK {
				t.Errorf("tokenFrom = %q, %v, want %q, %v", token, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestLivenessReadsFailWithoutAPlatformProbe covers the branch the non-unix
// build takes (proc_other.go) on every platform: its probe answers
// ErrUnsupportedPlatform, and every read that needs a liveness must fail with
// that answer and keep what it read rather than prune a bird whose process it
// cannot judge.
func TestLivenessReadsFailWithoutAPlatformProbe(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, ErrUnsupportedPlatform }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	if _, err := reg.List(); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("List = %v, want the platform's own answer", err)
	}
	if _, err := reg.Taken(); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("Taken = %v, want the platform's own answer", err)
	}
	if got, err := reg.Pick(); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("Pick = %+v, %v, want the platform's own answer", got, err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != "robin" {
		t.Errorf("agents on disk = %+v, want the record kept: an unreadable process is not a dead one", st.Agents)
	}
	if len(st.Reservations) != 0 {
		t.Errorf("reservations = %+v, want none: a Pick that failed reserves no name", st.Reservations)
	}
}
