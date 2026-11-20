package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doltTopSectionDepsFor builds the section's deps around reader, with a sleep
// that records and does not wait, so a test runs the two reads without spending
// the interval between them.
func doltTopSectionDepsFor(reader doltTopReader, sleeps *[]time.Duration) doltTopSectionDeps {
	return doltTopSectionDeps{
		open: func(context.Context) (doltTopReader, error) { return reader, nil },
		sleep: func(_ context.Context, d time.Duration) error {
			*sleeps = append(*sleeps, d)
			return nil
		},
	}
}

// Two process-list reads of four connections give the section the later read's
// connection count and the two busiest statement shapes by the share of the
// reads that saw each: the ranking `gt dolt top` prints, cut to two rows.
func TestDoltTopSectionShowsConnectionsAndShares(t *testing.T) {
	t.Parallel()

	reader := &fakeTopReader{samples: [][]doltTopSample{
		{
			{User: "root", Host: "a:1", Statement: "SELECT COUNT(*) FROM issues"},
			{User: "root", Host: "b:1", Statement: "SELECT 1 FROM dual"},
			{User: "root", Host: "c:1"},
			{User: "root", Host: "d:1"},
		},
		{
			{User: "root", Host: "a:1", Statement: "SELECT COUNT(*) FROM issues"},
			{User: "root", Host: "b:1", Statement: `SELECT * FROM issues WHERE id = 'gt-b'`},
			{User: "root", Host: "c:1"},
			{User: "root", Host: "d:1"},
		},
	}}
	var sleeps []time.Duration

	section, err := readDoltTopSection(context.Background(), doltTopSectionDepsFor(reader, &sleeps))
	if err != nil {
		t.Fatalf("readDoltTopSection: %v", err)
	}
	if reader.reads != 2 || !reader.closed {
		t.Errorf("the section made %d reads and closed=%v, want 2 reads of a closed reader", reader.reads, reader.closed)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Errorf("the section waited %v between its reads, want one second", sleeps)
	}
	if section.Title != "Dolt: connections and top statements" {
		t.Errorf("the section's title is %q, want the panel's own", section.Title)
	}
	if section.Note != "4 connections" {
		t.Errorf("the section's note is %q, want the later read's 4 connections", section.Note)
	}
	want := [][]string{
		{"100%", "SELECT COUNT(*) FROM issues"},
		{"50%", "SELECT * FROM issues WHERE id = ?"},
	}
	if len(section.Rows) != len(want) {
		t.Fatalf("the section has %d rows, want %d: %v", len(section.Rows), len(want), section.Rows)
	}
	for i := range want {
		if got := section.Rows[i]; len(got) != 2 || got[0] != want[i][0] || got[1] != want[i][1] {
			t.Errorf("row %d is %v, want %v", i, got, want[i])
		}
	}
	if section.Err != "" {
		t.Errorf("the section carries Err %q, want none", section.Err)
	}
}

// The section is registered, so the town's panel draws it without another file
// naming it, and its read goes through the town root the panel put in the
// context: a root it cannot read is refused rather than dialed.
func TestDoltTopSectionIsRegistered(t *testing.T) {
	t.Parallel()

	var registered *doltSectionReader
	readers := doltSections.snapshot()
	for i := range readers {
		if readers[i].key == "top" {
			registered = &readers[i]
		}
	}
	if registered == nil {
		t.Fatal(`no "top" section is registered, so the panel has no top section to draw`)
	}

	_, err := registered.read(withDoltTownRoot(context.Background(), filepath.Join(t.TempDir(), "missing")), time.Now())
	if err == nil {
		t.Fatal("the registered read accepted a town root that does not exist")
	}
	if !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Errorf("the registered read failed with %q, want it to name the town it could not find", err)
	}
}

// A town root the section cannot read is an error before any connection is
// opened: DefaultConfig answers a missing town with the default address, so
// dialing one would report on a server the panel was not asked about.
func TestDoltTopSectionRefusesATownRootItCannotRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-town")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", file, err)
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no town root", ctx: context.Background(), want: "no town root"},
		{name: "a root that does not exist", ctx: withDoltTownRoot(context.Background(), filepath.Join(dir, "missing")), want: "missing"},
		{name: "a file", ctx: withDoltTownRoot(context.Background(), file), want: "not a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reader, err := openDoltTopSection(tc.ctx)
			if err == nil {
				reader.Close()
				t.Fatal("the section opened a connection to a town root it cannot read")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the section failed with %q, want it to say %q", err, tc.want)
			}
		})
	}
}

// A read that does not complete is the section's error, never a table drawn
// from fewer than the two reads its shares are counted against.
func TestDoltTopSectionFailsWhenAReadDoesNotComplete(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		deps    func(sleeps *[]time.Duration) doltTopSectionDeps
		wantErr error
		want    string
	}{
		{
			name: "cannot open",
			deps: func(*[]time.Duration) doltTopSectionDeps {
				return doltTopSectionDeps{
					open:  func(context.Context) (doltTopReader, error) { return nil, errTopRead },
					sleep: func(context.Context, time.Duration) error { return nil },
				}
			},
			wantErr: errTopRead,
			want:    errTopRead.Error(),
		},
		{
			name: "the wait is canceled",
			deps: func(sleeps *[]time.Duration) doltTopSectionDeps {
				deps := doltTopSectionDepsFor(&fakeTopReader{}, sleeps)
				deps.sleep = func(context.Context, time.Duration) error { return context.Canceled }
				return deps
			},
			wantErr: context.Canceled,
			want:    context.Canceled.Error(),
		},
		{
			name: "a read fails",
			deps: func(sleeps *[]time.Duration) doltTopSectionDeps {
				return doltTopSectionDepsFor(&fakeTopReader{failRead: true, failAt: 1}, sleeps)
			},
			wantErr: errTopRead,
			want:    "reading the process list",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sleeps []time.Duration
			section, err := readDoltTopSection(context.Background(), tc.deps(&sleeps))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("readDoltTopSection error = %v, want %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error is %q, want it to say %q", err, tc.want)
			}
			if section.Title != "" || section.Note != "" || section.Rows != nil {
				t.Errorf("the failed read returned the section %+v, want none", section)
			}
		})
	}
}

// A server whose later read lists one connection says so in the singular, and
// an idle server is a section with the count and no statement rows.
func TestDoltTopSectionOneIdleConnection(t *testing.T) {
	t.Parallel()

	// The later read is the count: the first read's two connections both closed
	// before the second read, and the page must say one.
	section := doltTopSection(
		[]doltTopSample{{User: "root", Host: "a:1"}, {User: "root", Host: "b:1"}},
		[]doltTopSample{{User: "root", Host: "a:1"}},
	)
	if section.Note != "1 connection" {
		t.Errorf("the section's note is %q, want the later read's %q", section.Note, "1 connection")
	}
	if section.Rows != nil {
		t.Errorf("the idle section has rows %v, want none", section.Rows)
	}
	if section.Err != "" {
		t.Errorf("the idle section carries Err %q, want none", section.Err)
	}
}
