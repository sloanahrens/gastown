package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// specSeatPool is a settings file block opening one seat on agent at cap.
func specSeatPool(agent string, cap int) string {
	return fmt.Sprintf(`"polecat_pool": {"seats": {%q: {"cap": %d}}}`, agent, cap)
}

// specSeatPools is a settings file block opening a seat per agent=cap pair.
func specSeatPools(seats map[string]int) string {
	var parts []string
	for agent, cap := range seats {
		parts = append(parts, fmt.Sprintf(`%q: {"cap": %d}`, agent, cap))
	}
	return `"polecat_pool": {"seats": {` + strings.Join(parts, ", ") + `}}`
}

// writeSpecSeatTown writes the town's whole settings/config.json from the
// blocks given, so a test can hand the ticker a seat mix and then move it. The
// daemon patrol document goes under its "daemon" section: both seat blocks are
// then one file, as they are on a migrated town.
func writeSpecSeatTown(t *testing.T, root string, blocks ...string) {
	t.Helper()
	body := `{"type": "town-settings", "version": 1`
	for _, b := range blocks {
		body += ", " + b
	}
	body += "}\n"
	writeSpecSeatBody(t, root, body)
}

// writeSpecSeatBody writes settings/config.json's bytes.
func writeSpecSeatBody(t *testing.T, root, body string) {
	t.Helper()
	path := config.TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// specSeatDaemon is a daemon whose ticker ticks a temp town, with the gt
// subprocess faked: it reports a tick that slung nothing.
func specSeatDaemon(t *testing.T, root string) (*Daemon, *fakeCLI, func() []string) {
	t.Helper()
	fake := newFakeCLI(func([]string) cliReply {
		return cliReply{stdout: `{"roster": "claude-sonnet 0/2", "candidates": 0}`}
	})
	logger, lines := lineLogger()
	return &Daemon{
		config:  &Config{TownRoot: root},
		ctx:     context.Background(),
		logger:  logger,
		execCmd: fake.run,
	}, fake, lines
}

// tick runs one tick and waits for it, reporting whether the ticker started.
func specSeatTick(d *Daemon) bool {
	if !d.triggerSpecDispatch() {
		return false
	}
	d.specDispatchCycles.Wait()
	return true
}

// A seat the file gains is the seat the next tick runs with, and the daemon
// says so once: the ticker reads the mix on every tick, so an operator who
// moves the second seat sees it land instead of restarting the daemon to force
// it, which costs the tick or the landing in flight (gt-jdnab).
func TestTriggerSpecDispatchAnnouncesASeatChangeAndTicksOnIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	d, fake, lines := specSeatDaemon(t, root)

	if !specSeatTick(d) {
		t.Fatal("the first tick did not start")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now claude-sonnet=2"); n != 1 {
		t.Errorf("first-tick seat line = %d, want one naming the mix it ran with:\n%s", n, strings.Join(lines(), "\n"))
	}

	// An unchanged file is not a change, however many ticks read it.
	if !specSeatTick(d) || !specSeatTick(d) {
		t.Fatal("the ticker stopped ticking an unchanged town")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now"); n != 1 {
		t.Errorf("seat lines = %d, want the one the first tick logged:\n%s", n, strings.Join(lines(), "\n"))
	}

	// The operator opens a seat on deepseek-flash: the next tick runs with it
	// and says so, once.
	writeSpecSeatTown(t, root, specSeatPools(map[string]int{"claude-sonnet": 2, "deepseek-flash": 2}))
	if !specSeatTick(d) || !specSeatTick(d) {
		t.Fatal("the ticker did not tick after the seat change")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now", "claude-sonnet=2 deepseek-flash=2"); n != 1 {
		t.Errorf("seat-change lines = %d, want one for the new mix:\n%s", n, strings.Join(lines(), "\n"))
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 5 {
		t.Errorf("tick calls = %d, want one per trigger (%v)", len(calls), calls)
	}
}

// Closing every seat is a change like any other: the keys go away, and the
// ticker says the mix is empty rather than going quiet about it (gt-jdnab).
func TestTriggerSpecDispatchAnnouncesEverySeatClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	d, _, lines := specSeatDaemon(t, root)
	if !specSeatTick(d) {
		t.Fatal("the first tick did not start")
	}

	writeSpecSeatTown(t, root)
	if !specSeatTick(d) {
		t.Fatal("the ticker stopped ticking")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now none"); n != 1 {
		t.Errorf("closed-seat lines = %d, want one:\n%s", n, strings.Join(lines(), "\n"))
	}
}

// A seat file that does not parse parks the ticker instead of ticking on a
// picture that lost a seat to a typo: no tick runs, the seats in force stay the
// ones the last good read named, and the error is one line per distinct error
// rather than one per tick (gt-jdnab).
func TestTriggerSpecDispatchParksOnASeatFileThatDoesNotParse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	d, fake, lines := specSeatDaemon(t, root)
	if !specSeatTick(d) {
		t.Fatal("the first tick did not start")
	}

	writeSpecSeatBody(t, root, `{"type": "town-settings", "version": 1, "polecat_pool": {"seats":`)
	for i := 0; i < 3; i++ {
		if specSeatTick(d) {
			t.Fatal("the ticker ticked on a seat file that does not parse")
		}
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 1 {
		t.Errorf("tick calls = %d, want only the one before the file broke (%v)", len(calls), calls)
	}
	if n := countContaining(lines(), "spec_dispatch: not ticking:", "seat settings unreadable"); n != 1 {
		t.Errorf("park lines = %d, want one for one error:\n%s", n, strings.Join(lines(), "\n"))
	}
	// townhealth reads the park off the tracker: a parked ticker runs no tick
	// to record, so this is what keeps it from reading as a silent dispatcher
	// (gt-jdnab).
	if why := d.specSeats.parked(); !strings.Contains(why, "seat settings could not be read") {
		t.Errorf("parked reason = %q, want the read error townhealth reports", why)
	}

	// A different error is its own line, and still one per error.
	writeSpecSeatBody(t, root, "{\n  \"type\": \"town-settings\",\n")
	for i := 0; i < 2; i++ {
		specSeatTick(d)
	}
	if n := countContaining(lines(), "spec_dispatch: not ticking:", "seat settings unreadable"); n != 2 {
		t.Errorf("park lines = %d, want one per distinct error:\n%s", n, strings.Join(lines(), "\n"))
	}

	// A file that parses again resumes the ticker, and the mix it names is the
	// one already announced: unchanged seats are no new line (gt-jdnab).
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	if !specSeatTick(d) {
		t.Fatal("the ticker did not resume after the file parsed again")
	}
	if n := countContaining(lines(), "seat settings readable again"); n != 1 {
		t.Errorf("resume lines = %d, want one:\n%s", n, strings.Join(lines(), "\n"))
	}
	if why := d.specSeats.parked(); why != "" {
		t.Errorf("parked reason = %q, want none once a read worked again", why)
	}
	if n := countContaining(lines(), "spec_dispatch: seats now"); n != 1 {
		t.Errorf("seat lines = %d, want the first tick's alone: the seats did not change:\n%s", n, strings.Join(lines(), "\n"))
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 2 {
		t.Errorf("tick calls = %d, want the resumed tick to have run (%v)", len(calls), calls)
	}
}

// A read that failed is retried on the next tick even with both files where
// they were: a failure that clears without an operator edit — a writer that was
// mid-file when the tick read it — must not park the ticker until a stamp
// happens to move (gt-jdnab).
func TestSpecSeatTrackerRetriesAReadThatFailed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	path := config.TownSettingsPath(root)
	body, err := os.ReadFile(path) //nolint:gosec // G304: a temp town this test wrote
	if err != nil {
		t.Fatal(err)
	}
	good := string(body)
	// One byte moved turns the body into one that does not parse without
	// changing its size, so the good body can be put back under a stamp the
	// failed read already saw.
	broken := strings.Replace(good, `"cap":`, `"cap",`, 1)
	if broken == good || len(broken) != len(good) {
		t.Fatalf("broken body = %q, want the good one with one byte moved", broken)
	}
	writeSpecSeatBody(t, root, broken)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var tr specSeatTracker
	if _, err := tr.read(root); err == nil {
		t.Fatal("a read of a body that does not parse reported no error")
	}

	// The good body is back at the size and mtime the failed read was keyed
	// by, so a tracker that answered that read from its cache stays parked.
	writeSpecSeatBody(t, root, good)
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	picture, err := tr.read(root)
	if err != nil || picture != "claude-sonnet=2" {
		t.Errorf("read after the file parsed again = %q, %v, want the seat picture", picture, err)
	}
}

// The read opens nothing for writing: a town with no settings file reads as the
// dispatcher's defaults and leaves that file as absent as it found it (gt-jdnab).
func TestSpecSeatReadLeavesTheFilesItReadsAlone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var tr specSeatTracker
	picture, err := tr.read(root)
	if err != nil || picture != "" {
		t.Fatalf("read of a town with no seat keys = %q, %v, want an empty picture", picture, err)
	}
	for _, path := range specSeatFiles(root) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the read left %s behind (stat %v): reading a seat file must not create one", path, err)
		}
	}
}

// The read is keyed by the size and mtime of the seat files, so a tick with no
// operator edit costs one stat each (gt-jdnab).
func TestSpecSeatTrackerReadsOnlyWhenAFileMoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2))
	var tr specSeatTracker
	first, err := tr.read(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != "claude-sonnet=2" {
		t.Fatalf("picture = %q", first)
	}

	// A rewrite that leaves the stamp where it was is answered from the read
	// already made: same size, mtime put back.
	path := config.TownSettingsPath(root)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 4))
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got, err := tr.read(root); err != nil || got != first {
		t.Errorf("read under an unchanged stamp = %q, %v, want the picture already read (%q)", got, err, first)
	}

	// Moving the stamp reads the file again.
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 40))
	if got, err := tr.read(root); err != nil || !strings.Contains(got, "claude-sonnet=40") {
		t.Errorf("read after the file moved = %q, %v, want the new cap", got, err)
	}
}

// The picture names the seat map in the loader's order, sorted by agent, with a
// closed window beside its seat, and the total cap last. A seat with a cap of 0
// is a closed seat and renders as one; max_total is a pointer, so its 0 is the
// cap explicitly removed and renders (gt-jdnab, gt-mukgk).
func TestSpecSeatPictureNamesTheSeatMap(t *testing.T) {
	t.Parallel()
	four, zero := 4, 0
	for _, tc := range []struct {
		name string
		pool *config.PolecatPool
		want string
	}{
		{"no pool at all", nil, ""},
		{"a pool with no seats", &config.PolecatPool{MinSpawnGap: "4m"}, ""},
		{"one seat", &config.PolecatPool{Seats: map[string]config.SeatConfig{"claude-sonnet": {Cap: 2}}}, "claude-sonnet=2"},
		{"seats are sorted by agent", &config.PolecatPool{Seats: map[string]config.SeatConfig{
			"deepseek-flash": {Cap: 3}, "claude-haiku": {Cap: 1}, "claude-sonnet": {Cap: 2},
		}}, "claude-haiku=1 claude-sonnet=2 deepseek-flash=3"},
		{"a seat closed by its cap", &config.PolecatPool{Seats: map[string]config.SeatConfig{"claude-sonnet": {Cap: 0}}}, "claude-sonnet=0"},
		{"a closed window", &config.PolecatPool{Seats: map[string]config.SeatConfig{
			"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"},
		}}, "deepseek-flash=3(closed 20:00-08:00)"},
		{"a total cap", &config.PolecatPool{MaxTotal: &four}, "max_total=4"},
		{"a total cap removed", &config.PolecatPool{MaxTotal: &zero}, "max_total=0"},
		{"seats then the total", &config.PolecatPool{
			Seats: map[string]config.SeatConfig{"claude-sonnet": {Cap: 2}}, MaxTotal: &four,
		}, "claude-sonnet=2 max_total=4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specSeatPicture(tc.pool); got != tc.want {
				t.Errorf("specSeatPicture(%+v) = %q, want %q", tc.pool, got, tc.want)
			}
		})
	}
}

// A settings file that still holds a retired seat key parks the ticker with the
// pointer to the migration, rather than ticking on seats the dispatcher no
// longer reads (gt-mukgk).
func TestTriggerSpecDispatchParksOnARetiredSeatKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecSeatTown(t, root, `"polecat_pool": {"agent": "claude-sonnet", "max_seats": 2}`)
	d, fake, lines := specSeatDaemon(t, root)
	if specSeatTick(d) {
		t.Fatal("the ticker ticked on a file holding polecat_pool.agent")
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 0 {
		t.Errorf("tick calls = %d, want none (%v)", len(calls), calls)
	}
	if n := countContaining(lines(), "spec_dispatch: not ticking:", "polecat_pool.agent was replaced by", "gt pool migrate"); n != 1 {
		t.Errorf("park lines = %d, want one naming the replacement and the migration:\n%s", n, strings.Join(lines(), "\n"))
	}
}
