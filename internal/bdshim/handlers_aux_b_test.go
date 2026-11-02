package bdshim

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The config, events and stats handlers' tests answer the five golden rows
// through Main against the fixture the goldens were recorded from, then drive
// every refusal each handler can make: a subcommand it does not answer, a flag
// or operand a golden row does not cover, a store that cannot be reached or
// read, a feed with records, a feed pruned past the since, an unreadable
// deferral, and an answer that cannot be printed.

// TestHandlersConfig answers every config golden row.
func TestHandlersConfig(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "config")
}

// TestHandlersEvents answers every events golden row.
func TestHandlersEvents(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "events")
}

// TestHandlersStats answers every stats golden row.
func TestHandlersStats(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "stats")
}

// TestHandlersConfigReadsBackWhatItSets: the value config set stores is the
// value config get reads, so the line the handler prints is the write it made.
func TestHandlersConfigReadsBackWhatItSets(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, stderr, _ := fixtureEnvWithStore(t, town, fixture, []string{"config", "set", "export.auto", "true"})
	if code := Main(env); code != 0 {
		t.Fatalf("config set: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "Set export.auto = true (in config.yaml)\n"; stdout.String() != want {
		t.Errorf("config set stdout = %q, want %q", stdout.String(), want)
	}
	env.Args = []string{"config", "get", "export.auto"}
	stdout = &bytes.Buffer{}
	env.Stdout = stdout
	if code := Main(env); code != 0 {
		t.Fatalf("config get after set: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := "true\n"; stdout.String() != want {
		t.Errorf("config get after set = %q, want %q", stdout.String(), want)
	}
}

// TestHandlersConfigGetAnswersBdsDefaultAndNotSetLines: a key the tracker has
// no value for answers bd's documented default, and a key bd has no default
// for answers bd's "not set" line, both with exit 0 as bd exits.
func TestHandlersConfigGetAnswersBdsDefaultAndNotSetLines(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "export.auto has bd's default", arg: "export.auto", want: "false\n"},
		{name: "a key with no default is not set", arg: "custom.probe", want: "custom.probe (not set)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"config", "get", tc.arg})
			if code := Main(env); code != 0 {
				t.Fatalf("config get %s: exit code is %d, want 0 (stderr %q)", tc.arg, code, stderr.String())
			}
			if stdout.String() != tc.want {
				t.Errorf("stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}

// TestHandlersConfigGetActorReadsTheEnvironment: bd resolves the actor from
// BD_ACTOR before the store, and the replay names the recorder's actor.
func TestHandlersConfigGetActorReadsTheEnvironment(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"config", "get", configActorKey})
	if code := Main(env); code != 0 {
		t.Fatalf("config get actor: exit code is %d, want 0 (stderr %q)", code, stderr.String())
	}
	if want := fixtureActor + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want the recorder's actor %q", stdout.String(), want)
	}
}

// TestHandlersConfigRefusesTheCommandsItDoesNotAnswer: a subcommand, flag or
// operand a golden row does not cover is refused by name before the store is
// opened, with nothing on stdout and exit 1.
func TestHandlersConfigRefusesTheCommandsItDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "a bare config is refused by name", argv: []string{"config"}, want: "config is not implemented for tracker rig fx-"},
		{name: "config show is refused by name", argv: []string{"config", "show"}, want: "config show is not implemented for tracker rig fx-"},
		{name: "config unset is refused by name", argv: []string{"config", "unset", "export.auto"}, want: "config unset is not implemented for tracker rig fx-"},
		{name: "an undocumented subcommand is named", argv: []string{"config", "remove", "export.auto"}, want: "config remove is not implemented for tracker rig fx-"},
		{name: "config list refuses --json", argv: []string{"config", "list", "--json"}, want: "flag --json is not implemented for tracker rig fx-"},
		{name: "config list takes no operand", argv: []string{"config", "list", "extra"}, want: "config list takes no operands"},
		{name: "config get needs a key", argv: []string{"config", "get"}, want: "config get needs a key"},
		{name: "config get needs a non-empty key", argv: []string{"config", "get", ""}, want: "config get needs a non-empty key"},
		{name: "config get takes one key", argv: []string{"config", "get", "a", "b"}, want: "config get takes one key"},
		{name: "config get refuses a flag", argv: []string{"config", "get", "--json", "export.auto"}, want: "flag --json is not implemented for tracker rig fx-"},
		{name: "config set needs a key and a value", argv: []string{"config", "set"}, want: "config set needs a key and a value"},
		{name: "config set needs a value", argv: []string{"config", "set", "export.auto"}, want: "config set needs a value"},
		{name: "config set needs a non-empty key", argv: []string{"config", "set", "", "x"}, want: "config set needs a non-empty key"},
		{name: "config set takes a key and a value", argv: []string{"config", "set", "a", "b", "c"}, want: "config set takes a key and a value"},
		{name: "config set refuses a flag", argv: []string{"config", "set", "--json", "export.auto", "false"}, want: "flag --json is not implemented for tracker rig fx-"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersConfigRefusesTheActorItCannotResolve: bd resolves the actor from
// BEADS_ACTOR too, and from git identity and $USER below that; the shim's
// recorded answer is BD_ACTOR's, so a command that would answer from another
// name is refused rather than answered with the wrong actor.
func TestHandlersConfigRefusesTheActorItCannotResolve(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		env  map[string]string
		want string
	}{
		{
			name: "config list with only BEADS_ACTOR set",
			argv: []string{"config", "list"},
			env:  map[string]string{"BD_ACTOR": "", "BEADS_ACTOR": "someone-else"},
			want: "only BEADS_ACTOR set",
		},
		{
			name: "config get actor with only BEADS_ACTOR set",
			argv: []string{"config", "get", configActorKey},
			env:  map[string]string{"BD_ACTOR": "", "BEADS_ACTOR": "someone-else"},
			want: "only BEADS_ACTOR set",
		},
		{
			name: "config get actor with no actor environment",
			argv: []string{"config", "get", configActorKey},
			env:  map[string]string{"BD_ACTOR": "", "BEADS_ACTOR": ""},
			want: "without BD_ACTOR",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			inner := env.Getenv
			env.Getenv = func(name string) string {
				if value, ok := tc.env[name]; ok {
					return value
				}
				return inner(name)
			}
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersEventsRefusesTheCommandsItDoesNotAnswer: an events subcommand or
// flag a golden row does not cover is refused by name, with nothing on stdout
// and exit 1.
func TestHandlersEventsRefusesTheCommandsItDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "a bare events is refused by name", argv: []string{"events"}, want: "events is not implemented for tracker rig fx-"},
		{name: "events export is refused by name", argv: []string{"events", "export"}, want: "events export is not implemented for tracker rig fx-"},
		{name: "events prune is refused by name", argv: []string{"events", "prune"}, want: "events prune is not implemented for tracker rig fx-"},
		{name: "events tail refuses --limit", argv: []string{"events", "tail", "--limit", "1"}, want: "flag --limit is not implemented for tracker rig fx-"},
		{name: "events tail refuses --since", argv: []string{"events", "tail", "--since", "1"}, want: "flag --since is not implemented for tracker rig fx-"},
		{name: "events tail refuses --follow", argv: []string{"events", "tail", "--follow"}, want: "flag --follow is not implemented for tracker rig fx-"},
		{name: "events tail needs a seq", argv: []string{"events", "tail", "abc"}, want: "is not a seq"},
		{name: "events tail takes one seq", argv: []string{"events", "tail", "0", "1"}, want: "events tail takes at most one seq"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersStatsRefusesTheCommandsItDoesNotAnswer: a flag or operand a
// golden row does not cover, and a stats without --json, are refused by name.
func TestHandlersStatsRefusesTheCommandsItDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "stats without --json is refused", argv: []string{"stats"}, want: "stats without --json"},
		{name: "stats refuses --no-blocked", argv: []string{"stats", "--json", "--no-blocked"}, want: "flag --no-blocked is not implemented for tracker rig fx-"},
		{name: "stats takes no operands", argv: []string{"stats", "--json", "extra"}, want: "stats takes no operands"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to contain %q", tc.argv, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", tc.argv, stdout.String())
			}
		})
	}
}

// TestHandlersAuxBRefuseAStoreTheyCannotOpen: a rig with no tracker endpoint
// cannot answer any of the three verbs, and the refusal names what is missing
// rather than printing an empty answer.
func TestHandlersAuxBRefuseAStoreTheyCannotOpen(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	for _, argv := range [][]string{
		{"config", "list"},
		{"config", "get", "export.auto"},
		{"config", "set", "export.auto", "false"},
		{"events", "tail", "0"},
		{"stats", "--json"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			t.Parallel()
			env, stdout, stderr := fixtureEnv(t, town, fixture, argv)
			env.Getenv = func(string) string { return "" }
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", argv, code)
			}
			if !strings.Contains(stderr.String(), "TRACKER_URL") {
				t.Errorf("%v: stderr = %q, want it to name TRACKER_URL", argv, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("%v: wrote %q, want nothing", argv, stdout.String())
			}
		})
	}
}

// TestListConfigReportsEachReadItCannotMake: every read a config list makes can
// fail, and each failure stops the answer rather than printing a block with a
// key it could not read.
func TestListConfigReportsEachReadItCannotMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   stubAuxConfigStore
		want string
	}{
		{
			name: "a seeded key fails",
			st:   stubAuxConfigStore{get: func(string) (string, error) { return "", errors.New("store unavailable") }},
			want: "reading auto_compact_enabled",
		},
		{
			name: "issue_prefix is empty",
			st: stubAuxConfigStore{get: func(key string) (string, error) {
				if key == "issue_prefix" {
					return "", nil
				}
				return "false", nil
			}},
			want: "answered no issue_prefix",
		},
		{
			name: "issue_prefix cannot be read",
			st: stubAuxConfigStore{get: func(key string) (string, error) {
				if key == "issue_prefix" {
					return "", errors.New("prefix read refused")
				}
				return "false", nil
			}},
			want: "reading issue_prefix",
		},
		{
			name: "the answer cannot be printed",
			st:   stubAuxConfigStore{get: func(string) (string, error) { return "false", nil }},
			want: "writing bd's answer",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stderr := &bytes.Buffer{}
			env := Env{Getenv: func(string) string { return "" }, Stdout: failingWriter{}, Stderr: stderr}
			if code := listConfig(env, tc.st, "fx-"); code != 1 {
				t.Errorf("exit code is %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

// TestGetConfigReportsEachReadItCannotMake: a key read that fails, or an answer
// that cannot be printed, is the shim's error rather than a value.
func TestGetConfigReportsEachReadItCannotMake(t *testing.T) {
	t.Parallel()
	t.Run("the key read fails", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		env := Env{Stdout: &bytes.Buffer{}, Stderr: stderr}
		st := stubAuxConfigStore{get: func(string) (string, error) { return "", errors.New("store unavailable") }}
		if code := getConfig(env, st, "custom.probe", "fx-"); code != 1 {
			t.Errorf("exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "store unavailable") {
			t.Errorf("stderr = %q, want the store's error reported", stderr.String())
		}
	})
	t.Run("the answer cannot be printed", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		env := Env{Stdout: failingWriter{}, Stderr: stderr}
		st := stubAuxConfigStore{get: func(string) (string, error) { return "v", nil }}
		if code := getConfig(env, st, "custom.probe", "fx-"); code != 1 {
			t.Errorf("exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "writing bd's answer") {
			t.Errorf("stderr = %q, want the failed print reported", stderr.String())
		}
	})
}

// TestConfigSetReportsAWriteOrAnswerThatFails: a refused write is reported and
// nothing is printed; a write whose answer cannot be printed says the value
// was stored rather than reading as a set to retry.
func TestConfigSetReportsAWriteOrAnswerThatFails(t *testing.T) {
	t.Parallel()
	t.Run("the write fails", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		env := Env{Stdout: &bytes.Buffer{}, Stderr: stderr}
		st := stubAuxConfigStore{set: func(string, string) error { return errors.New("write refused") }}
		if code := setConfig(env, st, "export.auto", "false"); code != 1 {
			t.Errorf("exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "write refused") {
			t.Errorf("stderr = %q, want the store's error reported", stderr.String())
		}
	})
	t.Run("the answer cannot be printed", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		env := Env{Stdout: failingWriter{}, Stderr: stderr}
		st := stubAuxConfigStore{set: func(string, string) error { return nil }}
		if code := setConfig(env, st, "export.auto", "false"); code != 1 {
			t.Errorf("exit code is %d, want 1", code)
		}
		if !strings.Contains(stderr.String(), "the value was stored") {
			t.Errorf("stderr = %q, want it to say the write landed", stderr.String())
		}
	})
}

// TestTailEventsReportsAFeedItCannotAnswer: a read that fails, a read pruned
// past the since, a store that answers no page, and a feed with records are
// each refused rather than answered as an empty journal.
func TestTailEventsReportsAFeedItCannotAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   stubAuxEventsStore
		want string
	}{
		{
			name: "the read fails",
			st:   stubAuxEventsStore{tail: func(int64, int) (*beads.EventsPage, error) { return nil, errors.New("store unavailable") }},
			want: "store unavailable",
		},
		{
			name: "the read was pruned past the since",
			st: stubAuxEventsStore{tail: func(since int64, _ int) (*beads.EventsPage, error) {
				return nil, &beads.EventsTruncatedError{Since: since, Floor: 5, Head: 9}
			}},
			want: "pruned past --since 0",
		},
		{
			name: "the store answers no page",
			st:   stubAuxEventsStore{tail: func(int64, int) (*beads.EventsPage, error) { return nil, nil }},
			want: "answered no page",
		},
		{
			name: "the feed holds a record",
			st: stubAuxEventsStore{tail: func(int64, int) (*beads.EventsPage, error) {
				return &beads.EventsPage{Records: []beads.EventRecord{{Seq: 1}}}, nil
			}},
			want: "events tail of 1 record(s) is not implemented for tracker rig fx-",
		},
		{
			name: "the page says more follow",
			st: stubAuxEventsStore{tail: func(int64, int) (*beads.EventsPage, error) {
				return &beads.EventsPage{More: true}, nil
			}},
			want: "with more records than the page",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			env := Env{Stdout: stdout, Stderr: stderr}
			if code := tailEvents(env, tc.st, 0, "fx-"); code != 1 {
				t.Errorf("exit code is %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("wrote %q, want nothing for a feed the golden does not cover", stdout.String())
			}
		})
	}
}

// TestStatsCountsTheBoardTheGoldenRecords: the summary covers every status,
// the blocked set, the ready view and an epic whose children are all closed.
func TestStatsCountsTheBoardTheGoldenRecords(t *testing.T) {
	t.Parallel()
	list := func(opts beads.ListOptions) ([]*beads.Issue, error) {
		if opts.Ephemeral {
			return nil, nil
		}
		return []*beads.Issue{
			{ID: "fx-1", Status: "open", Type: "task", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks", Status: "open"}}},
			{ID: "fx-2", Status: "open", Type: "task"},
			{ID: "fx-epic", Status: "open", Type: "epic"},
			{ID: "fx-kid", Status: "closed", Type: "task", Parent: "fx-epic"},
			{ID: "fx-closed", Status: "closed", Type: "task"},
			// A closed issue held by an open blocker is in the blocked set but
			// is not one of the blocked issues bd's stats counts.
			{ID: "fx-closed-blocked", Status: "closed", Type: "task", Dependencies: []beads.IssueDep{{ID: "fx-2", DependencyType: "blocks", Status: "open"}}},
			{ID: "fx-deferred", Status: "deferred", Type: "task"},
			{ID: "fx-pinned", Status: "pinned", Type: "task"},
		}, nil
	}
	showMany := func(ids []string) (map[string]*beads.Issue, error) {
		all, _ := list(beads.ListOptions{})
		out := map[string]*beads.Issue{}
		for _, is := range all {
			out[is.ID] = is
		}
		return out, nil
	}
	st := stubStatsStore{list: list, showMany: showMany}
	summary, err := readStats(st, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("readStats: %v", err)
	}
	want := bdStatsSummary{
		TotalIssues: 8, OpenIssues: 3, ClosedIssues: 3, BlockedIssues: 1,
		DeferredIssues: 1, ReadyIssues: 2, PinnedIssues: 1, EpicsEligibleForClosure: 1,
	}
	if summary != want {
		t.Errorf("summary = %+v, want %+v", summary, want)
	}
}

// TestReadyForStatsRefusesAnUnreadableDeferralOrMissingParent: a defer_until
// the shim cannot parse, and a parent link the store does not answer, stop the
// count rather than reading as no deferral or as ready.
func TestReadyForStatsRefusesAnUnreadableDeferralOrMissingParent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("an unreadable defer_until", func(t *testing.T) {
		t.Parallel()
		is := &beads.Issue{ID: "fx-1", Status: "open", DeferUntil: "not a time"}
		if _, err := readyForStats(is, map[string]*beads.Issue{"fx-1": is}, nil, now); err == nil {
			t.Error("readyForStats accepted a defer_until it could not read")
		}
	})
	t.Run("a parent the store did not answer", func(t *testing.T) {
		t.Parallel()
		is := &beads.Issue{ID: "fx-1", Status: "open", Parent: "fx-missing"}
		if _, err := readyForStats(is, map[string]*beads.Issue{"fx-1": is}, nil, now); err == nil {
			t.Error("readyForStats accepted a parent the store did not answer")
		}
	})
	t.Run("an unreadable parent defer_until", func(t *testing.T) {
		t.Parallel()
		child := &beads.Issue{ID: "fx-1", Status: "open", Parent: "fx-parent"}
		parent := &beads.Issue{ID: "fx-parent", Status: "open", DeferUntil: "not a time"}
		if _, err := readyForStats(child, map[string]*beads.Issue{"fx-1": child, "fx-parent": parent}, nil, now); err == nil {
			t.Error("readyForStats accepted a parent defer_until it could not read")
		}
	})
	t.Run("a future deferral keeps the issue off ready", func(t *testing.T) {
		t.Parallel()
		is := &beads.Issue{ID: "fx-1", Status: "open", DeferUntil: now.Add(time.Hour).Format(time.RFC3339)}
		ready, err := readyForStats(is, map[string]*beads.Issue{"fx-1": is}, nil, now)
		if err != nil || ready {
			t.Errorf("readyForStats = %v, %v; want not ready", ready, err)
		}
	})
	t.Run("a deferred parent keeps its child off ready", func(t *testing.T) {
		t.Parallel()
		child := &beads.Issue{ID: "fx-1", Status: "open", Parent: "fx-parent"}
		parent := &beads.Issue{ID: "fx-parent", Status: "open", DeferUntil: now.Add(time.Hour).Format(time.RFC3339)}
		ready, err := readyForStats(child, map[string]*beads.Issue{"fx-1": child, "fx-parent": parent}, nil, now)
		if err != nil || ready {
			t.Errorf("readyForStats = %v, %v; want not ready", ready, err)
		}
	})
}

// TestReadStatsReportsEachReadItCannotMake: every read the summary makes can
// fail, and each failure stops the answer rather than printing a partial one.
func TestReadStatsReportsEachReadItCannotMake(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		st   stubStatsStore
		want string
	}{
		{
			name: "the list fails",
			st: stubStatsStore{list: func(beads.ListOptions) ([]*beads.Issue, error) {
				return nil, errors.New("list refused")
			}},
			want: "list refused",
		},
		{
			name: "the link read fails",
			st: stubStatsStore{
				list: func(beads.ListOptions) ([]*beads.Issue, error) {
					return []*beads.Issue{{ID: "fx-1", Status: "open"}}, nil
				},
				showMany: func([]string) (map[string]*beads.Issue, error) {
					return nil, errors.New("links refused")
				},
			},
			want: "links refused",
		},
		{
			name: "a deferral it cannot read",
			st: stubStatsStore{
				list: func(beads.ListOptions) ([]*beads.Issue, error) {
					return []*beads.Issue{{ID: "fx-1", Status: "open", DeferUntil: "not a time"}}, nil
				},
				showMany: func([]string) (map[string]*beads.Issue, error) {
					return map[string]*beads.Issue{"fx-1": {ID: "fx-1", Status: "open", DeferUntil: "not a time"}}, nil
				},
			},
			want: "is not a time",
		},
		{
			name: "the wisp list fails",
			st: stubStatsStore{
				list: func(opts beads.ListOptions) ([]*beads.Issue, error) {
					if opts.Ephemeral {
						return nil, errors.New("wisps refused")
					}
					return nil, nil
				},
				showMany: func([]string) (map[string]*beads.Issue, error) { return map[string]*beads.Issue{}, nil },
			},
			want: "wisps refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := readStats(tc.st, now); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("readStats = %v, want an error holding %q", err, tc.want)
			}
		})
	}
}

// stubAuxConfigStore is a configuration store a test scripts.
type stubAuxConfigStore struct {
	get func(string) (string, error)
	set func(string, string) error
}

func (s stubAuxConfigStore) ConfigGet(key string) (string, error) {
	if s.get == nil {
		return "", errors.New("stubAuxConfigStore: ConfigGet was not scripted")
	}
	return s.get(key)
}

func (s stubAuxConfigStore) ConfigSet(key, value string) error {
	if s.set == nil {
		return errors.New("stubAuxConfigStore: ConfigSet was not scripted")
	}
	return s.set(key, value)
}

// stubAuxEventsStore is an events feed a test scripts.
type stubAuxEventsStore struct {
	tail func(int64, int) (*beads.EventsPage, error)
}

func (s stubAuxEventsStore) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	if s.tail == nil {
		return nil, errors.New("stubAuxEventsStore: EventsTail was not scripted")
	}
	return s.tail(since, limit)
}

// stubStatsStore is a read store a test scripts.
type stubStatsStore struct {
	list     func(beads.ListOptions) ([]*beads.Issue, error)
	showMany func([]string) (map[string]*beads.Issue, error)
}

func (s stubStatsStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.list == nil {
		return nil, errors.New("stubStatsStore: List was not scripted")
	}
	return s.list(opts)
}

func (s stubStatsStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.showMany == nil {
		return nil, errors.New("stubStatsStore: ShowMultiple was not scripted")
	}
	return s.showMany(ids)
}

func (s stubStatsStore) Children(string) ([]*beads.Issue, error) {
	return nil, fmt.Errorf("stubStatsStore: Children was not scripted")
}

func (s stubStatsStore) Comments(string) ([]beads.Comment, error) {
	return nil, fmt.Errorf("stubStatsStore: Comments was not scripted")
}
