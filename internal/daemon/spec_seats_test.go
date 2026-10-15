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

// specSeatPool is a settings file block opening the pool seat on agent at cap.
func specSeatPool(agent string, cap int) string {
	return fmt.Sprintf(`"polecat_pool": {"agent": %q, "max_seats": %d}`, agent, cap)
}

// specSeatSecond is a settings file block putting the second seat on agent at
// cap, in the "daemon" section the two-file layout keeps the patrol document in.
func specSeatSecond(agent string, cap int) string {
	return fmt.Sprintf(`"daemon": {"type": "daemon-patrol-config", "version": 1, "patrols": {"spec_dispatch": {"enabled": true, "hooked_agent": %q, "max_hooked": %d}}}`,
		agent, cap)
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
	if n := countContaining(lines(), "spec_dispatch: seats now pool_agent=claude-sonnet max_seats=2"); n != 1 {
		t.Errorf("first-tick seat line = %d, want one naming the mix it ran with:\n%s", n, strings.Join(lines(), "\n"))
	}

	// An unchanged file is not a change, however many ticks read it.
	if !specSeatTick(d) || !specSeatTick(d) {
		t.Fatal("the ticker stopped ticking an unchanged town")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now"); n != 1 {
		t.Errorf("seat lines = %d, want the one the first tick logged:\n%s", n, strings.Join(lines(), "\n"))
	}

	// The operator moves the second seat onto deepseek-flash: the next tick
	// runs with it and says so, once.
	writeSpecSeatTown(t, root, specSeatPool("claude-sonnet", 2), specSeatSecond("deepseek-flash", 2))
	if !specSeatTick(d) || !specSeatTick(d) {
		t.Fatal("the ticker did not tick after the seat change")
	}
	if n := countContaining(lines(), "spec_dispatch: seats now", "hooked_agent=deepseek-flash", "max_hooked=2"); n != 1 {
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

	writeSpecSeatBody(t, root, `{"type": "town-settings", "version": 1, "polecat_pool": {"agent": "claude-sonnet", "max_seats":`)
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
	if n := countContaining(lines(), "spec_dispatch: seats now"); n != 1 {
		t.Errorf("seat lines = %d, want the first tick's alone: the seats did not change:\n%s", n, strings.Join(lines(), "\n"))
	}
	if calls := fake.argvs("spec", "dispatch"); len(calls) != 2 {
		t.Errorf("tick calls = %d, want the resumed tick to have run (%v)", len(calls), calls)
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
	if first != "pool_agent=claude-sonnet max_seats=2" {
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
	if got, err := tr.read(root); err != nil || !strings.Contains(got, "max_seats=40") {
		t.Errorf("read after the file moved = %q, %v, want the new cap", got, err)
	}
}

// The picture names the seat keys the files set, in the order the dispatcher
// reads them, and leaves out a key written as its zero — a closed seat reads as
// the key going away, except pro_max, whose 0 is the closed pro seat against an
// absent key's default of 1 (gt-jdnab).
func TestSpecSeatPictureNamesTheKeysTheFilesSet(t *testing.T) {
	t.Parallel()
	two, zero, four := 2, 0, 4
	for _, tc := range []struct {
		name string
		pool *config.PolecatPool
		sd   *config.SpecDispatchConfig
		want string
	}{
		{"no blocks at all", nil, nil, ""},
		{"the pool seat", &config.PolecatPool{Agent: "claude-sonnet", MaxSeats: 2}, nil, "pool_agent=claude-sonnet max_seats=2"},
		{"a pool seat closed by removing max_seats", &config.PolecatPool{Agent: "claude-sonnet"}, nil, "pool_agent=claude-sonnet"},
		{"the second seat", nil, &config.SpecDispatchConfig{HookedAgent: "deepseek-flash", MaxHooked: 2}, "hooked_agent=deepseek-flash max_hooked=2"},
		{"a second seat closed by max_hooked 0", nil, &config.SpecDispatchConfig{HookedAgent: "deepseek-flash"}, "hooked_agent=deepseek-flash"},
		{"the hooked seat preferred", nil, &config.SpecDispatchConfig{PreferHooked: true}, "prefer_hooked=true"},
		{"a written pro_max 0 is the closed pro seat", &config.PolecatPool{ProMax: &zero}, nil, "pro_max=0"},
		{"the pro seat's own agent, cap and label", &config.PolecatPool{ProAgent: "codex", ProMax: &two, ProLabel: "needs-pro"}, nil,
			"pro_agent=codex pro_max=2 pro_label=needs-pro"},
		{"a total cap", &config.PolecatPool{MaxTotal: &four}, nil, "max_total=4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specSeatPicture(tc.pool, tc.sd); got != tc.want {
				t.Errorf("specSeatPicture(%+v, %+v) = %q, want %q", tc.pool, tc.sd, got, tc.want)
			}
		})
	}
}
