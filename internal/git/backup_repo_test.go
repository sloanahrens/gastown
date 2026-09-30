package git

import (
	"reflect"
	"testing"
	"time"
)

func TestBackupRepoCommandsSendTheirArgv(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"init -b main":                     ok("Initialized empty Git repository\n"),
		"config http.postBuffer 524288000": ok(""),
		"commit -m backup 2026: hq=3 --author=Gas Town Daemon <daemon@gastown.local>": ok(""),
	})
	g := newTestGit(t, s)
	if err := g.InitRepo("main"); err != nil {
		t.Fatal(err)
	}
	if err := g.ConfigSet("http.postBuffer", "524288000"); err != nil {
		t.Fatal(err)
	}
	if err := g.CommitWithAuthor("backup 2026: hq=3", "Gas Town Daemon <daemon@gastown.local>"); err != nil {
		t.Fatal(err)
	}
	s.noUnscripted(t)
}

func TestPackSizeReadsTheSizePackLine(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"count-objects -v": ok("count: 3\nsize: 12\nin-pack: 40\npacks: 1\nsize-pack: 5120\nprune-packable: 0\n")})
	if got, err := newTestGit(t, s).PackSize(); err != nil || got != "5120" {
		t.Fatalf("PackSize = %q, %v; want 5120", got, err)
	}
	s = newScripted(map[string]reply{"count-objects -v": ok("count: 0\n")})
	if got, err := newTestGit(t, s).PackSize(); err != nil || got != "" {
		t.Fatalf("PackSize without a size-pack line = %q, %v; want empty", got, err)
	}
}

func TestLogAllParsesEntries(t *testing.T) {
	t.Parallel()
	const argv = "log --all --topo-order --max-count 64 --format=%H\x1f%cI\x1f%s"
	s := newScripted(map[string]reply{argv: ok(
		"bbb\x1f2026-09-30T13:00:00-05:00\x1fbackup 2026-09-30 13:00: hq=3\n" +
			"aaa\x1fnot-a-date\x1finit\n")})
	got, err := newTestGit(t, s).LogAll(64)
	want := []LogEntry{
		{Hash: "bbb", Time: time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC), Subject: "backup 2026-09-30 13:00: hq=3"},
		{Hash: "aaa", Subject: "init"},
	}
	if err != nil || len(got) != 2 || got[0].Hash != want[0].Hash || !got[0].Time.Equal(want[0].Time) || got[0].Subject != want[0].Subject || !reflect.DeepEqual(got[1], want[1]) {
		t.Fatalf("LogAll = %+v, %v; want %+v", got, err, want)
	}
	s = newScripted(map[string]reply{argv: ok("")})
	if got, err := newTestGit(t, s).LogAll(64); err != nil || len(got) != 0 {
		t.Fatalf("LogAll of an empty repository = %v, %v; want none", got, err)
	}
}
