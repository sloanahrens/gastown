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
func starts(procs map[int]string) func(int) (string, bool) {
	return func(pid int) (string, bool) {
		token, ok := procs[pid]
		return token, ok
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
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "robin", PID: 10, Start: "tok", Started: at(0)})
		return nil
	}); err != nil {
		t.Fatalf("seeding a live agent: %v", err)
	}
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

func TestPickRefusesABadPolecatGlob(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	reg.PolecatGlob = "["
	if _, _, err := reg.Pick(); err == nil {
		t.Fatal("Pick accepted a polecat glob that cannot match")
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
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
		return nil
	}); err != nil {
		t.Fatalf("seeding an agent: %v", err)
	}
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
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
		return nil
	}); err != nil {
		t.Fatalf("seeding an agent: %v", err)
	}
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
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "wren", PID: 10, Start: "tok", Started: at(0)})
		return nil
	}); err != nil {
		t.Fatalf("seeding an agent: %v", err)
	}
	live, err := reg.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].Name != "wren" {
		t.Errorf("List = %+v, want the live agent wren", live)
	}
}

func TestStateRoundTripsAnAwkwardCwd(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	cwd := `/home/ann/"quoted dir" & more/back\slash, end`
	if err := reg.Update(func(st *State) error {
		st.Agents = append(st.Agents, Agent{Name: "wren", PID: 10, Start: "tok", Cwd: cwd, Started: at(0)})
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
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

func TestLoadRefusesAnUnreadableState(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	if err := os.MkdirAll(filepath.Dir(reg.Path), 0o755); err != nil {
		t.Fatalf("creating the state directory: %v", err)
	}
	if err := os.WriteFile(reg.Path, []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("writing a broken state file: %v", err)
	}
	if _, err := reg.Load(); err == nil {
		t.Fatal("Load accepted a state file that is not JSON")
	}
}
