package bird

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// testRegistry returns a Registry over a fresh directory, with a fixed clock,
// a Rand that picks the first free name and no live processes.
func testRegistry(t *testing.T, names ...string) *Registry {
	t.Helper()
	dir := t.TempDir()
	reg := &Registry{
		Path:        filepath.Join(dir, ".runtime", "agents", "state.json"),
		Pool:        names,
		PolecatGlob: filepath.Join(dir, "*", "polecats", "*"),
		Now:         func() time.Time { return at(0) },
		Rand:        func(int) int { return 0 },
		ProcStart:   starts(nil),
	}
	return reg
}

// townDir is the town root testRegistry built its paths under.
func townDir(reg *Registry) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(reg.Path)))
}

// starts returns a process-info function answering from procs; a pid absent
// from procs has no live process.
func starts(procs map[int]string) func(int) (string, bool, error) {
	return func(pid int) (string, bool, error) {
		token, ok := procs[pid]
		return token, ok, nil
	}
}

// seedAgent records a agent in the state file without going through the
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
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "robin" {
		t.Errorf("Pick = %q, %v, want the unbound robin", name, ok)
	}
}

func TestPickSkipsLiveAgents(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "robin", "wren")
	reg.ProcStart = starts(map[int]string{10: "tok"})
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "wren" {
		t.Errorf("Pick = %q, %v, want the name of no live process", name, ok)
	}
}

func TestPickSkipsPolecatDirectories(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "lark", "wren")
	seated := filepath.Join(townDir(reg), "gastown", "polecats", "lark")
	if err := os.MkdirAll(seated, 0o755); err != nil {
		t.Fatalf("creating a polecat directory: %v", err)
	}
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "wren" {
		t.Errorf("Pick = %q, %v, want a name no polecat directory holds", name, ok)
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
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "lark" {
		t.Errorf("Pick = %q, %v, want the name the source selected", name, ok)
	}
}

func TestPickSkipsNamesNoSessionCouldUse(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "not a name", "wren")
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "wren" {
		t.Errorf("Pick = %q, %v, want the one usable name", name, ok)
	}
}

func TestPickReservesTheNameItReturns(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	first, ok, err := reg.Pick()
	if err != nil || !ok {
		t.Fatalf("first Pick = %q, %v, %v, want a name", first, ok, err)
	}
	second, ok, err := reg.Pick()
	if err != nil || !ok {
		t.Fatalf("second Pick = %q, %v, %v, want a name", second, ok, err)
	}
	if second == first {
		t.Errorf("two Picks returned %q, want the first one reserved so a second launch gets another name", first)
	}
}

func TestPickHandsOutANameAgainOnceItsReservationExpires(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.ReservationTTL = 5 * time.Minute
	if name, ok, err := reg.Pick(); err != nil || !ok || name != "wren" {
		t.Fatalf("Pick = %q, %v, %v, want wren from a pool of one", name, ok, err)
	}
	reg.Now = func() time.Time { return at(10) }
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick after the ttl: %v", err)
	}
	if !ok || name != "wren" {
		t.Errorf("Pick = %q, %v, want a launcher that died to leave no name held", name, ok)
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
	if _, _, err := reg.Pick(); err == nil {
		t.Fatal("Pick accepted a polecat glob that cannot match")
	}
}

func TestPickReportsALivenessItCannotRead(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	unreadable := errors.New("no process table")
	reg.ProcStart = func(int) (string, bool, error) { return "", false, unreadable }
	seedAgent(t, reg, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
	if name, ok, err := reg.Pick(); !errors.Is(err, unreadable) {
		t.Fatalf("Pick = %q, %v, %v, want the read error rather than a guess", name, ok, err)
	}
}

func TestNewEscapesTheTownRootInItsGlob(t *testing.T) {
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
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if !ok || name != "robin" {
		t.Errorf("Pick = %q, %v, want robin: the root's metacharacters must not hide the polecat directory wren", name, ok)
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
	name, ok, err := reg.Pick()
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if ok {
		t.Errorf("Pick = %q, true, want no name from an exhausted pool", name)
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

func TestConcurrentWritersKeepEveryEntry(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	names := []string{"wren", "robin"}
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = reg.Bind(name, fmt.Sprintf("gt-%d", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Bindings) != len(names) {
		t.Fatalf("bindings = %+v, want one from each writer", st.Bindings)
	}
	bound := make(map[string]bool, len(st.Bindings))
	for _, b := range st.Bindings {
		bound[b.Name] = true
	}
	for _, name := range names {
		if !bound[name] {
			t.Errorf("bindings = %+v, want the entry for %s not lost", st.Bindings, name)
		}
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
	if _, _, err := reg.Pick(); err == nil {
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
			name:    "alive but no token",
			pid:     10,
			token:   func(int) (string, bool) { return "", false },
			probe:   func(int) (bool, error) { return true, nil },
			wantErr: true,
		},
		{
			name:    "the probe itself failed",
			pid:     10,
			token:   func(int) (string, bool) { return "", false },
			probe:   func(int) (bool, error) { return false, boom },
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
