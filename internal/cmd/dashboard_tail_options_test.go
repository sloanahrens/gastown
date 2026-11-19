package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/landings"
)

// dashboardTestRigs stands in for the town's rig registry, so the landings
// source is built for one rig without a registry file in the test town.
func dashboardTestRigs(string) ([]string, error) { return []string{"gastown"}, nil }

// TestDashboardTailOptionsReadsNoEventsJournal: the hub's feed asks for the
// landings, daemon and watch sources only, opens no store's bd events journal,
// and builds no events source from those options.
func TestDashboardTailOptionsReadsNoEventsJournal(t *testing.T) {
	t.Parallel()
	opts := dashboardTailOptions(t.TempDir(), tailNow, tailTestLoc, dashboardTestRigs, nil, nil, nil)
	want := map[string]bool{tailKindLandings: true, tailKindDaemon: true, tailKindWatch: true}
	if !reflect.DeepEqual(opts.kinds, want) {
		t.Fatalf("kinds = %v, want %v", opts.kinds, want)
	}
	if opts.journalFor != nil {
		t.Fatal("the hub's feed carries a journal opener; it must open no store's events journal")
	}
	sources, _, err := buildTailSources(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if _, ok := s.(*eventsSource); ok {
			t.Fatalf("buildTailSources built an events source: %T", s)
		}
	}
}

// TestDashboardTailOptionsFeedMergesTheThreeSourcesByTime: over a landings
// file, a daemon log and a watch feed, the hub's feed shows the lines of all
// three, merged by time. The landings source is first in the source order, so
// a feed that did not sort would show its line first.
func TestDashboardTailOptionsFeedMergesTheThreeSourcesByTime(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	landingsPath, err := landings.Path(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(landingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, landingsPath, `{"bead":"gt-abc","rig":"gastown","landed_at":"2026-09-30T13:55:00Z"}`+"\n")

	daemonDir := filepath.Join(town, "daemon")
	if err := os.MkdirAll(daemonDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 08:01 in the CDT zone is 13:01 UTC.
	appendFile(t, filepath.Join(daemonDir, "daemon.log"), "2026/09/30 08:01:00 hm witness started\n")

	watchDir := dashboardWatchDir(town)
	if err := os.MkdirAll(watchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(watchDir, "alerts.jsonl"),
		`{"ts":"2026-09-30T13:50:00Z","class":"bd-slow","severity":"low","text":"bd took 4s"}`+"\n")

	sources, preface, err := buildTailSources(dashboardTailOptions(town, at("2026-09-30T13:00:00Z"), tailTestLoc, dashboardTestRigs, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(preface) != 0 {
		t.Fatalf("preface = %q, want none", texts(preface))
	}
	got := texts(mergeTail(pollTailSources(sources)...))
	want := []string{
		"town daemon hm witness started",
		"watch watch bd-slow: bd took 4s",
		"gastown landings landed gt-abc - -> - commit=- patch=- gate=- om=-/0.00 route=-",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("feed:\n%q\nwant\n%q", got, want)
	}
}
