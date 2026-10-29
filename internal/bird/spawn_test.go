package bird

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// cmuxLog is a runner whose cmux is recorded rather than run: each call appends
// its argv, and answers are what the calls in turn print.
type cmuxLog struct {
	calls   [][]string
	answers []string
	errs    []error
	next    int
}

// argv returns the arguments of the i-th cmux call.
func (c *cmuxLog) argv(i int) []string {
	if i >= len(c.calls) {
		return nil
	}
	return c.calls[i]
}

// spawnRunner is a Runner that opens no tab: every cmux call is answered from
// the log, and the environment says whether it is in cmux.
func spawnRunner(t *testing.T, reg *Registry, env []string, log *cmuxLog) *Runner {
	t.Helper()
	return &Runner{
		Registry: reg,
		Environ:  func() []string { return env },
		Cmux: func(argv []string) ([]byte, error) {
			log.calls = append(log.calls, slices.Clone(argv))
			i := log.next
			log.next++
			if i < len(log.errs) && log.errs[i] != nil {
				return nil, log.errs[i]
			}
			if i < len(log.answers) {
				return []byte(log.answers[i]), nil
			}
			return nil, errors.New("no answer scripted for this cmux call")
		},
	}
}

// inCmux is the environment of a shell running inside a cmux surface.
func inCmux() []string {
	return []string{cmuxSocketVar + "=/tmp/cmux.sock"}
}

func TestSpawnOpensATabUnderAFreeName(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren", "robin")
	log := &cmuxLog{answers: []string{`{"surface_ref":"surface:7"}`, ""}}

	got, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got != (SpawnResult{Name: "wren", Surface: "surface:7", Bead: "gt-wren"}) {
		t.Errorf("Spawn = %+v, want wren in surface:7 for gt-wren", got)
	}
	wantNew := []string{"cmux", "new-surface", "--type", "terminal", "--command",
		"gt bird start --name wren --bead gt-wren", "--json"}
	if calls := log.argv(0); !slices.Equal(calls, wantNew) {
		t.Errorf("new-surface ran %q, want %q", calls, wantNew)
	}
	wantRename := []string{"cmux", "rename-tab", "--tab", "surface:7", "wren"}
	if calls := log.argv(1); !slices.Equal(calls, wantRename) {
		t.Errorf("rename-tab ran %q, want %q", calls, wantRename)
	}
	assertReserved(t, reg, "wren")
}

// TestSpawnRefusesATakenName: a name the caller chose opens no tab while
// anything holds it — a binding, a live session or another spawn's reservation.
func TestSpawnRefusesATakenName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hold func(t *testing.T, reg *Registry)
	}{
		{
			name: "bound to a bead",
			hold: func(t *testing.T, reg *Registry) {
				if _, err := reg.Bind("wren", "gt-old"); err != nil {
					t.Fatalf("Bind: %v", err)
				}
			},
		},
		{
			name: "held by a live session",
			hold: func(t *testing.T, reg *Registry) {
				seedAgent(t, reg, Agent{Name: "wren", PID: 10, Start: "tok10", Started: at(0)})
				reg.ProcStart = starts(map[int]string{10: "tok10"})
			},
		},
		{
			name: "reserved by an earlier spawn",
			hold: func(t *testing.T, reg *Registry) {
				if held, err := reg.ReserveName("wren"); err != nil || held != "" {
					t.Fatalf("ReserveName = %q, %v, want the name reserved", held, err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, "wren")
			tc.hold(t, reg)
			log := &cmuxLog{}

			_, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "wren"})
			if err == nil || !strings.Contains(err.Error(), "wren is taken") {
				t.Fatalf("Spawn = %v, want a refusal naming wren as taken", err)
			}
			if len(log.calls) != 0 {
				t.Errorf("cmux ran %q, want no tab for a name that is taken", log.calls)
			}
		})
	}
}

// TestSpawnOutsideCmuxPrintsTheStartLine: with no workspace to open a tab in,
// the gt bird start line is the answer, and no name is drawn or reserved.
func TestSpawnOutsideCmuxPrintsTheStartLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  SpawnRequest
		want string
	}{
		{
			name: "a name and a mission",
			req:  SpawnRequest{Name: "wren", Mission: "fix the flaky test"},
			want: "gt bird start --name wren --bead gt-wren '/bird-spawn fix the flaky test'",
		},
		{
			name: "no name draws one in the session",
			req:  SpawnRequest{},
			want: "gt bird start",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := testRegistry(t, "wren", "robin")
			log := &cmuxLog{}

			_, err := spawnRunner(t, reg, nil, log).Spawn(tc.req)
			var notCmux *NotInCmuxError
			if !errors.As(err, &notCmux) {
				t.Fatalf("Spawn = %v, want a NotInCmuxError", err)
			}
			if notCmux.Line != tc.want {
				t.Errorf("line = %q, want %q", notCmux.Line, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error %q does not print the line to run", err)
			}
			if len(log.calls) != 0 {
				t.Errorf("cmux ran %q outside cmux, want no tab", log.calls)
			}
			st, err := reg.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(st.Reservations) != 0 {
				t.Errorf("reservations = %+v, want none: no session was opened to claim one", st.Reservations)
			}
		})
	}
}

// TestSpawnKeepsAMissionAsOnePlainArgument: a mission holding a quote, a
// backtick and a $( ) is quoted for the tab's shell, so the new session gets it
// as the text it was.
func TestSpawnKeepsAMissionAsOnePlainArgument(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	log := &cmuxLog{answers: []string{`{"surface_ref":"surface:7"}`, ""}}
	mission := "it's `x` $(rm -rf /)"

	if _, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "wren", Mission: mission}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	want := `gt bird start --name wren --bead gt-wren '/bird-spawn it'\''s ` + "`x`" + ` $(rm -rf /)'`
	if calls := log.argv(0); len(calls) != 7 || calls[5] != want {
		t.Errorf("the tab runs %q, want the mission as one quoted argument: %q", calls, want)
	}
}

// TestSpawnReleasesTheNameWhenNoTabOpens: a cmux that fails leaves no
// reservation behind, since nothing runs under the name.
func TestSpawnReleasesTheNameWhenNoTabOpens(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	log := &cmuxLog{errs: []error{errors.New("cmux refused")}}

	_, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "wren"})
	if err == nil || !strings.Contains(err.Error(), "cmux refused") {
		t.Fatalf("Spawn = %v, want cmux's failure", err)
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Reservations) != 0 {
		t.Errorf("reservations = %+v, want the name released when no tab opened", st.Reservations)
	}
}

// TestSpawnKeepsTheNameWhenTheTabOpened: once a tab exists, the reservation
// stays for the session in it to claim, even when the surface is unknown.
func TestSpawnKeepsTheNameWhenTheTabOpened(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	log := &cmuxLog{answers: []string{`{"ok":true}`}}

	_, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "wren"})
	if err == nil || !strings.Contains(err.Error(), "surface") {
		t.Fatalf("Spawn = %v, want an error about the surface it could not name", err)
	}
	assertReserved(t, reg, "wren")
}

// TestSpawnRefusesANameNoSessionCouldHold: a name with a space in it never
// reaches cmux, and exits as a refused argument does.
func TestSpawnRefusesANameNoSessionCouldHold(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	log := &cmuxLog{}

	_, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "not a name"})
	if err == nil || !strings.Contains(err.Error(), "bird name") {
		t.Fatalf("Spawn = %v, want a usage error for the name", err)
	}
	var usage *UsageError
	if !errors.As(err, &usage) || usage.ExitStatus() != 2 {
		t.Errorf("Spawn = %v, want a usage error with exit status 2", err)
	}
	if len(log.calls) != 0 {
		t.Errorf("cmux ran %q, want no tab for an unusable name", log.calls)
	}
}

// TestStartTakesTheNameASpawnReserved runs the two halves of a spawn together:
// the tab's gt bird start is the launch the reservation was made for, so it
// claims the name instead of refusing it as one an earlier launch reserved.
func TestStartTakesTheNameASpawnReserved(t *testing.T) {
	t.Parallel()
	r, reg, calls := testRunner(t, "wren")
	inCmuxEnv := inCmux()
	r.Environ = func() []string { return slices.Clone(inCmuxEnv) }
	r.Cmux = func([]string) ([]byte, error) { return []byte(`{"surface_ref":"surface:7"}`), nil }

	if _, err := r.Spawn(SpawnRequest{Name: "wren"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// The tab runs the line the spawn built, from a shell inside cmux whose
	// environment carries no bird identity yet.
	if err := r.Start([]string{"--name", "wren", "--bead", "gt-wren"}); err != nil {
		t.Fatalf("gt bird start in the spawned tab: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("claude started %d times, want once", len(*calls))
	}
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Agents) != 1 || st.Agents[0].Name != "wren" || st.Agents[0].PID != 99 {
		t.Errorf("agents = %+v, want the tab's session holding wren", st.Agents)
	}
	if len(st.Reservations) != 0 {
		t.Errorf("reservations = %+v, want none left by the claim", st.Reservations)
	}
	if bound, ok := st.Bound("gt-wren"); !ok || bound != "wren" {
		t.Errorf("binding of gt-wren = %q, %v; want wren", bound, ok)
	}
}

// TestSpawnRefusesAMissionThatWouldEndTheLine: a newline in the mission is the
// Enter that runs the tab's command, so the words after it would run as
// commands of their own.
func TestSpawnRefusesAMissionThatWouldEndTheLine(t *testing.T) {
	t.Parallel()
	reg := testRegistry(t, "wren")
	log := &cmuxLog{}

	_, err := spawnRunner(t, reg, inCmux(), log).Spawn(SpawnRequest{Name: "wren", Mission: "fix it\nrm -rf /"})
	if err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("Spawn = %v, want a refusal of the mission's newline", err)
	}
	if len(log.calls) != 0 {
		t.Errorf("cmux ran %q, want no tab for a mission it cannot type", log.calls)
	}
}

// assertReserved fails unless reg holds a reservation on name.
func assertReserved(t *testing.T, reg *Registry, name string) {
	t.Helper()
	st, err := reg.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, res := range st.Reservations {
		if res.Name == name {
			return
		}
	}
	t.Errorf("reservations = %+v, want one on %s", st.Reservations, name)
}
