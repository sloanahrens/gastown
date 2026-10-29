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
	return api.EventsPage{Events: out}, nil
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
	dir, src, err := openTrackerReplay(replayFixture(nil), "gastown")
	if err != nil || src == nil {
		t.Fatalf("openTrackerReplay(gastown) = %q, %v, %v; want the rig directory and a feed", dir, src, err)
	}
	if dir != "/town/gastown" {
		t.Errorf("rig directory = %q, want /town/gastown", dir)
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
	want := "bd create --id=gt-1 \"--title=Add x\"\ndry run: 1 bd calls, none run; would apply through seq 1\n"
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
	want := "replay stopped: last applied seq 4; fix the cause, then rerun with --since-seq 4\n"
	if errOut.String() != want {
		t.Errorf("stop hint = %q, want %q", errOut.String(), want)
	}
}

func strPtr(s string) *string { return &s }
