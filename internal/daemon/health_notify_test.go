package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// noticeStore records the notice beads the transition notice files.
type noticeStore struct {
	created []beads.CreateOptions
	fail    error
}

func (n *noticeStore) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	n.created = append(n.created, opts)
	if n.fail != nil {
		return nil, n.fail
	}
	return &beads.Issue{ID: fmt.Sprintf("hq-notice%d", len(n.created))}, nil
}

// notifyRecorder records the argv the notify command was run with.
type notifyRecorder struct {
	argv [][]string
	fail error
}

func (r *notifyRecorder) run(_ context.Context, argv []string) error {
	r.argv = append(r.argv, argv)
	return r.fail
}

// notifyTown is a town whose config names command as its health notify
// command, with the command run and the notice bead's writer faked.
func notifyTown(t *testing.T, now time.Time, command string) (*Daemon, *noticeStore, *notifyRecorder) {
	t.Helper()
	town := t.TempDir()
	health := map[string]any{}
	if command != "" {
		health["notify_command"] = command
	}
	writeJSONFile(t, filepath.Join(town, "settings", "config.json"), map[string]any{
		"type": "town-settings", "version": 1,
		"operational": map[string]any{"health": health},
	})
	store, rec := &noticeStore{}, &notifyRecorder{}
	d := &Daemon{
		config:          &Config{TownRoot: town},
		logger:          log.New(io.Discard, "", 0),
		clock:           clockwork.NewFakeClockAt(now),
		notifyRun:       rec.run,
		openNoticeBeads: func() notifyBeadWriter { return store },
	}
	return d, store, rec
}

// report is a health report carrying just the verdict the notice reads.
func report(now time.Time, v townhealth.Verdict) townhealth.Report {
	return townhealth.Report{At: now, Verdict: v}
}

func TestNotifyArgv_AppendsTheSignalToTheConfiguredCommandLine(t *testing.T) {
	t.Parallel()
	line := "RED tick 0s ago: backup=none[R]"
	for _, tc := range []struct {
		name    string
		command string
		want    []string
	}{
		{"a bare program", "notify-me", []string{"notify-me", line}},
		{"arguments before the line", "terminal-notifier -title Gastown", []string{"terminal-notifier", "-title", "Gastown", line}},
		{"extra spaces", "  notify-me   -q  ", []string{"notify-me", "-q", line}},
		{"nothing configured", "   ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := notifyArgv(tc.command, line); !slices.Equal(got, tc.want) {
				t.Errorf("notifyArgv(%q) = %q, want %q", tc.command, got, tc.want)
			}
		})
	}
}

func TestNotifyCrossing_IsGreenToRedOrUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prev, cur townhealth.Verdict
		want      bool
	}{
		{townhealth.Green, townhealth.Red, true},
		{townhealth.Green, townhealth.VerdictUnknown, true},
		{townhealth.Green, townhealth.Green, false},
		{townhealth.Green, townhealth.Degraded, false},
		{townhealth.Degraded, townhealth.Red, false},
		{townhealth.Red, townhealth.Red, false},
		{townhealth.VerdictUnknown, townhealth.Red, false},
	} {
		if got := notifyCrossing(tc.prev, tc.cur); got != tc.want {
			t.Errorf("notifyCrossing(%s, %s) = %v, want %v", tc.prev, tc.cur, got, tc.want)
		}
	}
}

func TestNotifyHealthTransition_RunsTheCommandOnceAndFilesOneBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, rec := notifyTown(t, now, "terminal-notifier -title Gastown")
	prev, cur := report(now.Add(-3*time.Minute), townhealth.Green), report(now, townhealth.Red)
	line := "RED tick 0s ago: backup=none[R]"

	d.notifyHealthTransition(&prev, cur, line)

	if len(rec.argv) != 1 {
		t.Fatalf("ran the command %d times, want once", len(rec.argv))
	}
	want := []string{"terminal-notifier", "-title", "Gastown", line}
	if !slices.Equal(rec.argv[0], want) {
		t.Errorf("argv = %q, want the line appended to the configured command %q", rec.argv[0], want)
	}
	if len(store.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(store.created))
	}
	bead := store.created[0]
	if !strings.Contains(bead.Title, "green->red") || !strings.Contains(bead.Title, line) {
		t.Errorf("title = %q, want the crossing and the line", bead.Title)
	}
	if !strings.Contains(bead.Description, line) {
		t.Errorf("description = %q, want the line the operator was paged with", bead.Description)
	}
	if bead.Actor != daemonActor {
		t.Errorf("actor = %q, want %q", bead.Actor, daemonActor)
	}
}

// The notice is the operator's record, not work: a polecat that picks it up
// would be a polecat working an alert.
func TestNotifyHealthTransition_FilesABeadNoDispatcherTakes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, _ := notifyTown(t, now, "notify-me")
	prev, cur := report(now, townhealth.Green), report(now, townhealth.Red)

	d.notifyHealthTransition(&prev, cur, "RED tick 0s ago")

	if len(store.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(store.created))
	}
	bead := store.created[0]
	if !slices.Equal(bead.Labels, []string{constants.LabelTownHealth}) {
		t.Fatalf("labels = %q, want %q", bead.Labels, constants.LabelTownHealth)
	}
	if !beads.IsNonDispatchableBead(&beads.Issue{Title: bead.Title, Type: "task", Labels: bead.Labels}) {
		t.Error("a dispatchable notice bead is work a polecat can be slung; it must be non-dispatchable")
	}
}

func TestNotifyHealthTransition_SaysNothingWhenNothingCrossed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		prev townhealth.Verdict
		cur  townhealth.Verdict
	}{
		{"still green", townhealth.Green, townhealth.Green},
		{"only degraded", townhealth.Green, townhealth.Degraded},
		{"already red", townhealth.Red, townhealth.Red},
		{"degraded into red", townhealth.Degraded, townhealth.Red},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, store, rec := notifyTown(t, now, "notify-me")
			prev, cur := report(now, tc.prev), report(now, tc.cur)
			d.notifyHealthTransition(&prev, cur, "line")
			if len(rec.argv) != 0 || len(store.created) != 0 {
				t.Errorf("crossing %s->%s paged %d times and filed %d beads, want neither",
					tc.prev, tc.cur, len(rec.argv), len(store.created))
			}
		})
	}
}

func TestNotifyHealthTransition_NoCommandIsNoNotice(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, rec := notifyTown(t, now, "")
	prev, cur := report(now, townhealth.Green), report(now, townhealth.Red)

	d.notifyHealthTransition(&prev, cur, "RED tick 0s ago")

	if len(rec.argv) != 0 || len(store.created) != 0 {
		t.Errorf("a town with no notify command ran %d commands and filed %d beads, want neither",
			len(rec.argv), len(store.created))
	}
}

func TestNotifyHealthTransition_NoBaselineIsNoCrossing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, rec := notifyTown(t, now, "notify-me")

	d.notifyHealthTransition(nil, report(now, townhealth.Red), "RED tick 0s ago")

	if len(rec.argv) != 0 || len(store.created) != 0 {
		t.Errorf("a first tick with no previous report paged %d times and filed %d beads, want neither",
			len(rec.argv), len(store.created))
	}
}

// The command is the operator's pager, and it can fail on its own: the record
// of the crossing must not go with it, and the failure must say so.
func TestNotifyHealthTransition_FailedCommandStillFilesTheBead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, rec := notifyTown(t, now, "notify-me")
	rec.fail = errors.New("exit status 1")
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	prev, cur := report(now, townhealth.Green), report(now, townhealth.Red)

	d.notifyHealthTransition(&prev, cur, "RED tick 0s ago")

	if len(store.created) != 1 {
		t.Fatalf("filed %d beads after a failed page, want one", len(store.created))
	}
	if got := store.created[0].Description; !strings.Contains(got, "Outcome: failed") {
		t.Errorf("description = %q, want the failed outcome recorded", got)
	}
	if !strings.Contains(logs.String(), "exit status 1") {
		t.Errorf("log = %q, want the failure named", logs.String())
	}
}

// The notice is exported to the beads JSONL, so a pager that carries a token
// in one of its arguments must not leave it in the bead or in a title.
func TestNotifyHealthTransition_NeverRecordsTheConfiguredArguments(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, _ := notifyTown(t, now, "notify-me -H Authorization:Bearer-s3cret")
	prev, cur := report(now, townhealth.Green), report(now, townhealth.Red)

	d.notifyHealthTransition(&prev, cur, "RED tick 0s ago")

	if len(store.created) != 1 {
		t.Fatalf("filed %d beads, want one", len(store.created))
	}
	bead := store.created[0]
	for _, field := range []string{bead.Title, bead.Description} {
		if strings.Contains(field, "s3cret") {
			t.Errorf("the notice %q carries the command's arguments; only the program may be recorded", field)
		}
	}
	if !strings.Contains(bead.Description, "Notify command: notify-me\n") {
		t.Errorf("description = %q, want the program that ran", bead.Description)
	}
}

// A file that cannot be read must not cost the town the page for a crossing
// it makes on the same tick: the daemon's own copy of the previous report
// stands in.
func TestPreviousHealth_FallsBackToTheDaemonsOwnCopy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, rec := notifyTown(t, now, "notify-me")
	path := townhealth.Path(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	inMemory := report(now.Add(-3*time.Minute), townhealth.Green)
	d.lastTownHealth = &inMemory

	got := d.previousHealth()
	if got == nil || got.Verdict != townhealth.Green {
		t.Fatalf("previousHealth() = %+v with an unreadable file, want the daemon's own green copy", got)
	}

	d.notifyHealthTransition(got, report(now, townhealth.Red), "RED tick 0s ago")
	if len(rec.argv) != 1 || len(store.created) != 1 {
		t.Errorf("an unreadable baseline file lost the crossing: %d pages, %d beads, want one of each",
			len(rec.argv), len(store.created))
	}
}

func TestNotifyHealthTransition_AFailedBeadIsLogged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, store, _ := notifyTown(t, now, "notify-me")
	store.fail = errors.New("bd: no such database")
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	prev, cur := report(now, townhealth.Green), report(now, townhealth.Red)

	d.notifyHealthTransition(&prev, cur, "RED tick 0s ago")

	if !strings.Contains(logs.String(), "no such database") {
		t.Errorf("log = %q, want the filing failure named", logs.String())
	}
}

func TestNotifyTitle_IsOneTruncatedLine(t *testing.T) {
	t.Parallel()
	title := notifyTitle(townhealth.Green, townhealth.Red, strings.Repeat("x", 400))
	if runes := []rune(title); len(runes) > maxNotifyTitleLen {
		t.Errorf("title is %d runes, want at most %d: bd refuses the flag past its limit", len(runes), maxNotifyTitleLen)
	}
	title = notifyTitle(townhealth.Green, townhealth.VerdictUnknown, "UNKNOWN tick 0ms ago\nand a second line")
	if strings.ContainsAny(title, "\r\n") {
		t.Errorf("title = %q, want one line: bd refuses a newline in --title", title)
	}
	if !strings.Contains(title, "UNKNOWN tick 0ms ago") {
		t.Errorf("title = %q, want the first line kept", title)
	}
}

// The default writer has to be the plain bd at the town root: WithTimeout
// panics on a wrapper built any other way, and the notice belongs to the town
// database the way an escalation does.
func TestNoticeBeads_IsAUsableTownWriter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _, _ := notifyTown(t, now, "notify-me")
	if err := os.MkdirAll(filepath.Join(d.config.TownRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := d.noticeBeads(); w == nil {
		t.Error("noticeBeads() = nil, want a client pinned to the town's own database")
	}
}

func TestHealthNotifyCommand_ReadsTheOperatorConfig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _, _ := notifyTown(t, now, "  terminal-notifier -title Gastown  ")
	if got := d.healthNotifyCommand(); got != "terminal-notifier -title Gastown" {
		t.Errorf("healthNotifyCommand() = %q, want the trimmed configured command", got)
	}
	d2, _, _ := notifyTown(t, now, "")
	if got := d2.healthNotifyCommand(); got != "" {
		t.Errorf("healthNotifyCommand() = %q with no key set, want empty", got)
	}
}

// The notice fires from the heartbeat's own tick, off the report the tick
// wrote, exactly once for the crossing however many green ticks came before
// it and however many red ones follow (gt-s3rec.3).
func TestWriteTownHealth_NotifiesOnceOnTheCrossingToRed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	store, rec := &noticeStore{}, &notifyRecorder{}
	d.notifyRun, d.openNoticeBeads = rec.run, func() notifyBeadWriter { return store }
	writeJSONFile(t, filepath.Join(d.config.TownRoot, "settings", "config.json"), map[string]any{
		"type": "town-settings", "version": 1,
		"operational": map[string]any{"health": map[string]any{"notify_command": "notify-me"}},
	})
	// The town was green when the daemon last looked.
	if err := townhealth.Write(d.config.TownRoot, townhealth.Report{At: now.Add(-3 * time.Minute), Verdict: townhealth.Green}); err != nil {
		t.Fatal(err)
	}

	d.writeTownHealth()

	if len(rec.argv) != 1 || len(store.created) != 1 {
		t.Fatalf("the crossing paged %d times and filed %d beads, want one of each", len(rec.argv), len(store.created))
	}
	if line := rec.argv[0][len(rec.argv[0])-1]; !strings.HasPrefix(line, "RED tick ") || !strings.Contains(line, "backup=none") {
		t.Errorf("the command's last argument = %q, want the one-line signal", line)
	}

	// The town is still red on the next beat: one page per crossing.
	d.clock.(*clockwork.FakeClock).Advance(3 * time.Minute)
	d.writeTownHealth()
	if len(rec.argv) != 1 || len(store.created) != 1 {
		t.Errorf("three more minutes of red paged %d times and filed %d beads, want no repeat",
			len(rec.argv), len(store.created))
	}
}

// A daemon that restarted while the town was red has the last report on disk,
// not in memory: it must not page the operator a second time for a crossing
// they already heard about.
func TestWriteTownHealth_RestartDoesNotReAnnounceARedTown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	store, rec := &noticeStore{}, &notifyRecorder{}
	d.notifyRun, d.openNoticeBeads = rec.run, func() notifyBeadWriter { return store }
	writeJSONFile(t, filepath.Join(d.config.TownRoot, "settings", "config.json"), map[string]any{
		"type": "town-settings", "version": 1,
		"operational": map[string]any{"health": map[string]any{"notify_command": "notify-me"}},
	})
	if err := townhealth.Write(d.config.TownRoot, townhealth.Report{At: now.Add(-time.Hour), Verdict: townhealth.Red}); err != nil {
		t.Fatal(err)
	}

	d.writeTownHealth()

	if len(rec.argv) != 0 || len(store.created) != 0 {
		t.Errorf("a restart onto an already-red town paged %d times and filed %d beads, want neither",
			len(rec.argv), len(store.created))
	}
}
