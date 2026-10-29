package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/trackerreplay"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: route selection and the replay's refusals over fakes; no
// filesystem, no network and no environment beyond the map each test sets.

// feedStub serves a fixed feed, or fails every read with err.
type feedStub struct {
	events []api.Event
	err    error
}

func (f feedStub) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	if f.err != nil {
		return api.EventsPage{}, f.err
	}
	var out []api.Event
	for _, e := range f.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	// The store's end of feed echoes the cursor; the oldest held seq says
	// whether the log is there at all.
	page := api.EventsPage{Events: out, NextAfter: after}
	if len(out) > 0 {
		page.NextAfter = out[len(out)-1].Seq
	}
	if len(f.events) > 0 {
		page.FirstSeq = f.events[0].Seq
	}
	return page, nil
}

// replayFixture is a town with one Dolt rig, gastown, and one tracker-routed
// rig, agentlab, and a feed that serves events.
func replayFixture(events []api.Event) replayEnv {
	routes := []beads.Route{
		{Prefix: "gt-", Path: "gastown"},
		{Prefix: "ag-", Path: "agentlab", Backend: beads.BackendTracker},
	}
	return replayEnv{
		findTown:   func() (string, error) { return "/town", nil },
		loadRoutes: func(string) ([]beads.Route, error) { return routes, nil },
		getenv: func(key string) string {
			if key == "TRACKER_URL" {
				return "http://tracker.test"
			}
			return ""
		},
		newSource: func(client.Config) (trackerreplay.Source, error) {
			return feedStub{events: events}, nil
		},
		newApply: func(string) trackerreplay.Apply {
			return func([]string) error { return nil }
		},
	}
}

func TestPickRigRouteFindsTheRigAndRefusesATrackerRoute(t *testing.T) {
	t.Parallel()
	routes := []beads.Route{
		{Prefix: "gt-", Path: "gastown"},
		{Prefix: "ag-", Path: "agentlab", Backend: beads.BackendTracker},
	}
	cases := map[string]struct {
		rig     string
		want    string
		wantErr string
	}{
		"by beads prefix":       {rig: "gt", want: "gastown"},
		"by beads prefix dash":  {rig: "gt-", want: "gastown"},
		"by path":               {rig: "gastown", want: "gastown"},
		"tracker-routed refuse": {rig: "ag", wantErr: "still routed to the tracker"},
		"unknown rig":           {rig: "nope", wantErr: `no rig "nope"`},
	}
	for name, tc := range cases {
		got, err := pickRigRoute(routes, tc.rig)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: pickRigRoute(%q) error = %v, want one containing %q", name, tc.rig, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: pickRigRoute(%q) = %v, want the %s route", name, tc.rig, err, tc.want)
			continue
		}
		if got.Path != tc.want {
			t.Errorf("%s: pickRigRoute(%q).Path = %q, want %q", name, tc.rig, got.Path, tc.want)
		}
	}
}

func TestOpenTrackerReplayRefusesWhatItCannotResolve(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit    func(env *replayEnv)
		rig     string
		wantErr string
	}{
		"no town": {
			edit:    func(env *replayEnv) { env.findTown = func() (string, error) { return "", errors.New("not in a town") } },
			rig:     "gastown",
			wantErr: "not in a town",
		},
		"route table unreadable": {
			edit: func(env *replayEnv) {
				env.loadRoutes = func(string) ([]beads.Route, error) { return nil, errors.New("routes.jsonl is torn") }
			},
			rig:     "gastown",
			wantErr: "routes.jsonl is torn",
		},
		"rig not in the route table": {
			edit:    func(*replayEnv) {},
			rig:     "nope",
			wantErr: `no rig "nope"`,
		},
		"rig still on the tracker": {
			edit:    func(*replayEnv) {},
			rig:     "ag",
			wantErr: "flip it back to dolt",
		},
		"rig prefix is the start of another rig's": {
			edit: func(env *replayEnv) {
				env.loadRoutes = func(string) ([]beads.Route, error) {
					return []beads.Route{{Prefix: "gt-", Path: "gastown"}, {Prefix: "gt-x-", Path: "xrig"}}, nil
				}
			},
			rig:     "gastown",
			wantErr: "cannot tell their events apart",
		},
		"tracker url unset": {
			edit:    func(env *replayEnv) { env.getenv = func(string) string { return "" } },
			rig:     "gastown",
			wantErr: "TRACKER_URL is not set",
		},
		"feed client refuses its config": {
			edit: func(env *replayEnv) {
				env.newSource = func(client.Config) (trackerreplay.Source, error) { return nil, errors.New("no token") }
			},
			rig:     "gastown",
			wantErr: "no token",
		},
	}
	for name, tc := range cases {
		env := replayFixture(nil)
		tc.edit(&env)
		_, _, err := openTrackerReplay(env, tc.rig)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: openTrackerReplay(%q) error = %v, want one containing %q", name, tc.rig, err, tc.wantErr)
		}
	}
}

func TestOpenTrackerReplayResolvesTheRigDirectoryUnderTheTown(t *testing.T) {
	t.Parallel()
	target, src, err := openTrackerReplay(replayFixture(nil), "gastown")
	if err != nil || src == nil {
		t.Fatalf("openTrackerReplay(gastown) = %+v, %v, %v; want the rig directory and a feed", target, src, err)
	}
	if target.dir != "/town/gastown" || target.prefix != "gt-" {
		t.Errorf("rig target = %+v, want dir /town/gastown with prefix gt-", target)
	}
}

func TestLiveReplayEnvRefusesAFeedClientWithNoToken(t *testing.T) {
	t.Parallel()
	_, err := liveReplayEnv().newSource(client.Config{BaseURL: "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("newSource with no token = %v, want a refusal naming the token", err)
	}
}

func TestBdApplyReturnsTheFailureOfTheCallItRan(t *testing.T) {
	t.Parallel()
	bdErr := errors.New("bd: database locked")
	var ran []string
	apply := bdApply(func(args ...string) ([]byte, error) {
		ran = args
		return nil, bdErr
	})
	err := apply([]string{"update", "gt-1", "--status=open"})
	if !errors.Is(err, bdErr) {
		t.Errorf("apply = %v, want the bd failure", err)
	}
	if strings.Join(ran, " ") != "update gt-1 --status=open" {
		t.Errorf("bd ran %q, want the replayed argv", ran)
	}
}

func TestRunTrackerReplayRefusesAnUnresolvableRigBeforeReplaying(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	err := runTrackerReplay(replayFixture(nil), &out, &errOut, "nope", 0, false)
	if err == nil || !strings.Contains(err.Error(), `no rig "nope"`) {
		t.Fatalf("runTrackerReplay for an unknown rig = %v, want the route refusal", err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("a refused replay printed %q and %q, want nothing: it never started", out.String(), errOut.String())
	}
}

func TestRunTrackerReplayDryRunPrintsTheCallAndRunsNone(t *testing.T) {
	t.Parallel()
	events := []api.Event{{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: strPtr("Add x")}}
	env := replayFixture(events)
	applied := 0
	env.newApply = func(string) trackerreplay.Apply {
		return func([]string) error { applied++; return nil }
	}
	var out, errOut bytes.Buffer
	if err := runTrackerReplay(env, &out, &errOut, "gastown", 0, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	want := "bd create --id=gt-1 '--title=Add x'\ndry run: 1 bd calls, none run; would apply through seq 1\n"
	if out.String() != want {
		t.Errorf("dry run output = %q, want %q", out.String(), want)
	}
	if applied != 0 {
		t.Errorf("a dry run ran %d bd calls, want none", applied)
	}
}

func TestRunTrackerReplayStopsWithTheSeqToResumeFrom(t *testing.T) {
	t.Parallel()
	env := replayFixture(nil)
	feedErr := errors.New("tracker unreachable")
	env.newSource = func(client.Config) (trackerreplay.Source, error) { return feedStub{err: feedErr}, nil }
	var out, errOut bytes.Buffer
	err := runTrackerReplay(env, &out, &errOut, "gastown", 4, false)
	if !errors.Is(err, feedErr) {
		t.Fatalf("runTrackerReplay = %v, want the feed error", err)
	}
	want := "replay stopped before applying anything (seq 4); fix the cause, then rerun with --since-seq 4\n"
	if errOut.String() != want {
		t.Errorf("stop hint = %q, want %q", errOut.String(), want)
	}
}

func TestRunTrackerReplayPrintsTheSeqToResumeAfterAFailingBdCall(t *testing.T) {
	t.Parallel()
	events := []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: strPtr("a")},
		{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: strPtr("open")},
		{Seq: 3, Op: "closed", IssueID: "gt-1"},
	}
	env := replayFixture(events)
	bdErr := errors.New("bd: database locked")
	env.newApply = func(string) trackerreplay.Apply {
		ran := 0
		return func([]string) error {
			ran++
			if ran == 2 {
				return bdErr
			}
			return nil
		}
	}
	var out, errOut bytes.Buffer
	err := runTrackerReplay(env, &out, &errOut, "gastown", 0, false)
	if !errors.Is(err, bdErr) {
		t.Fatalf("runTrackerReplay = %v, want the bd failure", err)
	}
	want := "warning: 1 created issue(s) replayed with bd's defaults for priority, type and description (the feed carries only the title); review them in the rig\nreplay stopped: last applied seq 1; fix the cause, then rerun with --since-seq 1\n"
	if errOut.String() != want {
		t.Errorf("stop hint = %q, want %q", errOut.String(), want)
	}
}

// A creation replays with bd's defaults because the feed carries only its title;
// the run says so on stderr instead of filling the fields in silently (gt-p85pw.71).
func TestRunTrackerReplayWarnsWhenACreationTookBdDefaults(t *testing.T) {
	t.Parallel()
	env := replayFixture([]api.Event{{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: strPtr("Add x")}})
	env.newApply = func(string) trackerreplay.Apply { return func([]string) error { return nil } }
	var out, errOut bytes.Buffer
	if err := runTrackerReplay(env, &out, &errOut, "gastown", 0, false); err != nil {
		t.Fatalf("replay: %v", err)
	}
	want := "warning: 1 created issue(s) replayed with bd's defaults for priority, type and description (the feed carries only the title); review them in the rig\n"
	if errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
}

// A replay with no creation says nothing about defaults.
func TestRunTrackerReplayIsQuietWhenNothingWasCreated(t *testing.T) {
	t.Parallel()
	env := replayFixture([]api.Event{{Seq: 1, Op: "status_changed", IssueID: "gt-1", NewValue: strPtr("closed")}})
	env.newApply = func(string) trackerreplay.Apply { return func([]string) error { return nil } }
	var out, errOut bytes.Buffer
	if err := runTrackerReplay(env, &out, &errOut, "gastown", 0, false); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
}

// The feed carries every rig on the tracker, so the command says how many
// events it skipped for other rigs, and says nothing when there were none
// (gt-0n2s6).
func TestRunTrackerReplayReportsSkippedEventsOnlyWhenItSkippedSome(t *testing.T) {
	t.Parallel()
	const defaults = "warning: 1 created issue(s) replayed with bd's defaults for priority, type and description (the feed carries only the title); review them in the rig\n"
	cases := map[string]struct {
		events []api.Event
		want   string
	}{
		"events for another rig are counted": {
			events: []api.Event{
				{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: strPtr("a")},
				{Seq: 2, Op: "created", IssueID: "dv-1", NewValue: strPtr("b")},
			},
			want: "note: skipped 1 event(s) for other rigs (issue ids without the \"gt-\" prefix)\n" + defaults,
		},
		"a feed of the rig's own events prints no count": {
			events: []api.Event{{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: strPtr("a")}},
			want:   defaults,
		},
	}
	for name, tc := range cases {
		env := replayFixture(tc.events)
		env.newApply = func(string) trackerreplay.Apply { return func([]string) error { return nil } }
		var out, errOut bytes.Buffer
		if err := runTrackerReplay(env, &out, &errOut, "gastown", 0, false); err != nil {
			t.Errorf("%s: replay = %v, want no error", name, err)
			continue
		}
		if errOut.String() != tc.want {
			t.Errorf("%s: stderr = %q, want %q", name, errOut.String(), tc.want)
		}
	}
}

// A negative --since-seq is refused before anything is read (gt-p85pw.71).
func TestRunTrackerReplayRefusesANegativeSinceSeq(t *testing.T) {
	t.Parallel()
	env := replayFixture(nil)
	var out, errOut bytes.Buffer
	err := runTrackerReplay(env, &out, &errOut, "gastown", -1, false)
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("runTrackerReplay since -1 = %v, want a refusal naming the negative value", err)
	}
}

func TestRunTrackerReplaySaysSoWhenARefusalAppliedNothing(t *testing.T) {
	t.Parallel()
	events := []api.Event{{Seq: 7, Op: "commented", IssueID: "gt-1"}}
	var out, errOut bytes.Buffer
	err := runTrackerReplay(replayFixture(events), &out, &errOut, "gastown", 6, false)
	var u *trackerreplay.UnreplayableError
	if !errors.As(err, &u) {
		t.Fatalf("runTrackerReplay = %v, want the refusal", err)
	}
	want := "replay stopped before applying anything (seq 6); fix the cause, then rerun with --since-seq 6\n"
	if errOut.String() != want {
		t.Errorf("stop hint = %q, want %q", errOut.String(), want)
	}
}

func strPtr(s string) *string { return &s }
