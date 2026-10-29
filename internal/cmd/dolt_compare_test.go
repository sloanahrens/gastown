package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSnapshotReader answers a server from memory, so a test's readers never
// open a connection.
type fakeSnapshotReader struct {
	snapshots []dbSnapshot
	err       error
}

func (f *fakeSnapshotReader) Read(context.Context) ([]dbSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snapshots, nil
}

// compareTestDeps answers the command's readers by port, so an endpoint
// argument selects which fake server answers.
func compareTestDeps(w *bytes.Buffer, servers map[int]*fakeSnapshotReader) doltCompareDeps {
	return doltCompareDeps{
		newReader: func(ep doltCompareEndpoint, _ time.Duration) snapshotReader {
			if f, ok := servers[ep.Port]; ok {
				return f
			}
			return &fakeSnapshotReader{err: errors.New("no fake server on port " + strconv.Itoa(ep.Port))}
		},
		lookupPassword: func(string) string { return "" },
		out:            w,
	}
}

// testCompareCmd builds the command over fake readers and runs its RunE, the
// way the shell would reach it with the two endpoints.
func testCompareCmd(t *testing.T, deps doltCompareDeps, sourcePort, targetPort int) error {
	t.Helper()
	cmd := newDoltCompareCmd(deps)
	cmd.SetContext(context.Background())
	return cmd.RunE(cmd, []string{
		"127.0.0.1:" + strconv.Itoa(sourcePort),
		"127.0.0.1:" + strconv.Itoa(targetPort),
	})
}

func TestCompareSnapshots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source []dbSnapshot
		target []dbSnapshot
		want   map[string]string // database → rendered status
	}{
		{
			name:   "equal servers",
			source: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "aaa"}},
			target: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "aaa"}},
			want:   map[string]string{"hq": "equal"},
		},
		{
			name:   "differing row count shows both numbers",
			source: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "aaa"}},
			target: []dbSnapshot{{Name: "hq", Rows: 9, Hash: "aaa"}},
			want:   map[string]string{"hq": "row count differs (source 12, target 9)"},
		},
		{
			name:   "differing content hash shows both hashes",
			source: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "aaa"}},
			target: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "bbb"}},
			want:   map[string]string{"hq": "content hash differs (source aaa, target bbb)"},
		},
		{
			name:   "row count and content hash both differ",
			source: []dbSnapshot{{Name: "hq", Rows: 12, Hash: "aaa"}},
			target: []dbSnapshot{{Name: "hq", Rows: 9, Hash: "bbb"}},
			want:   map[string]string{"hq": "row count differs (source 12, target 9); content hash differs (source aaa, target bbb)"},
		},
		{
			name:   "database on the source only",
			source: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "aaa"}, {Name: "gt", Rows: 2, Hash: "ccc"}},
			target: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "aaa"}},
			want:   map[string]string{"hq": "equal", "gt": "present on source only"},
		},
		{
			name:   "database on the target only",
			source: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "aaa"}},
			target: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "aaa"}, {Name: "gt", Rows: 2, Hash: "ccc"}},
			want:   map[string]string{"hq": "equal", "gt": "present on target only"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diffs := compareSnapshots(tt.source, tt.target)
			got := make(map[string]string, len(diffs))
			for _, d := range diffs {
				got[d.Name] = d.status()
			}
			if len(got) != len(tt.want) {
				t.Fatalf("compared %d databases, want %d: %v", len(got), len(tt.want), got)
			}
			for name, want := range tt.want {
				if got[name] != want {
					t.Errorf("%s status = %q, want %q", name, got[name], want)
				}
			}
		})
	}
}

// A comparison is read from both servers and printed in name order, whatever
// order the servers list their databases in.
func TestCompareSnapshotsOrdersByDatabaseName(t *testing.T) {
	t.Parallel()
	diffs := compareSnapshots(
		[]dbSnapshot{{Name: "zz", Rows: 1, Hash: "a"}, {Name: "aa", Rows: 1, Hash: "a"}},
		[]dbSnapshot{{Name: "mm", Rows: 1, Hash: "a"}, {Name: "aa", Rows: 1, Hash: "a"}},
	)
	var names []string
	for _, d := range diffs {
		names = append(names, d.Name)
	}
	if want := []string{"aa", "mm", "zz"}; !slices.Equal(names, want) {
		t.Errorf("database order = %v, want %v", names, want)
	}
}

func TestCompareSameServersSucceeds(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	deps := compareTestDeps(out, map[int]*fakeSnapshotReader{
		3307: {snapshots: []dbSnapshot{{Name: "hq", Rows: 7, Hash: "aaa"}}},
		3401: {snapshots: []dbSnapshot{{Name: "hq", Rows: 7, Hash: "aaa"}}},
	})
	if err := testCompareCmd(t, deps, 3307, 3401); err != nil {
		t.Fatalf("compare of equal servers = %v, want nil (exit 0)", err)
	}
	for _, want := range []string{"hq", "equal"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q should contain %q", out.String(), want)
		}
	}
}

func TestCompareAnyDifferenceFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target []dbSnapshot
		reason string
	}{
		{
			name:   "row count differs",
			target: []dbSnapshot{{Name: "hq", Rows: 6, Hash: "aaa"}},
			reason: "row count differs (source 7, target 6)",
		},
		{
			name:   "content hash differs",
			target: []dbSnapshot{{Name: "hq", Rows: 7, Hash: "bbb"}},
			reason: "content hash differs (source aaa, target bbb)",
		},
		{
			name:   "database missing on the target",
			target: nil,
			reason: "present on source only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := &bytes.Buffer{}
			deps := compareTestDeps(out, map[int]*fakeSnapshotReader{
				3307: {snapshots: []dbSnapshot{{Name: "hq", Rows: 7, Hash: "aaa"}}},
				3401: {snapshots: tt.target},
			})
			err := testCompareCmd(t, deps, 3307, 3401)
			if err == nil {
				t.Fatal("compare with a difference exited 0, want non-zero")
			}
			if !strings.Contains(err.Error(), "hq") {
				t.Errorf("error %q should name the differing database", err)
			}
			if !strings.Contains(out.String(), tt.reason) {
				t.Errorf("output %q should report %q", out.String(), tt.reason)
			}
		})
	}
}

// A database the target serves and the source does not is a difference too:
// the restore proof compares the same set of databases.
func TestCompareDatabaseOnlyOnTargetFails(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	deps := compareTestDeps(out, map[int]*fakeSnapshotReader{
		3307: {snapshots: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "a"}}},
		3401: {snapshots: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "a"}, {Name: "gt", Rows: 2, Hash: "b"}}},
	})
	err := testCompareCmd(t, deps, 3307, 3401)
	if err == nil || !strings.Contains(err.Error(), "gt") {
		t.Fatalf("compare with an extra target database = %v, want an error naming gt", err)
	}
	if !strings.Contains(out.String(), "present on target only") {
		t.Errorf("output %q should report the extra database", out.String())
	}
}

// A server that cannot be read is not a server that matched: the comparison
// fails instead of reporting the databases it managed to read (fail closed).
func TestCompareUnreadableServerFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source *fakeSnapshotReader
		target *fakeSnapshotReader
	}{
		{
			name:   "source unreadable",
			source: &fakeSnapshotReader{err: errors.New("connection refused")},
			target: &fakeSnapshotReader{snapshots: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "a"}}},
		},
		{
			name:   "target unreadable",
			source: &fakeSnapshotReader{snapshots: []dbSnapshot{{Name: "hq", Rows: 1, Hash: "a"}}},
			target: &fakeSnapshotReader{err: errors.New("connection refused")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := &bytes.Buffer{}
			deps := doltCompareDeps{
				newReader: func(ep doltCompareEndpoint, _ time.Duration) snapshotReader {
					if ep.Port == 3307 {
						return tt.source
					}
					return tt.target
				},
				lookupPassword: func(string) string { return "" },
				out:            out,
			}
			err := testCompareCmd(t, deps, 3307, 3401)
			if err == nil {
				t.Fatal("compare of an unreadable server exited 0, want non-zero")
			}
			if !strings.Contains(err.Error(), "connection refused") {
				t.Errorf("error %q should keep the read failure", err)
			}
		})
	}
}

// Two servers that serve nothing compare equal by definition, and say so
// rather than printing an empty table a reader could take for a proof.
func TestCompareBothServersEmptySucceeds(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	deps := compareTestDeps(out, map[int]*fakeSnapshotReader{
		3307: {snapshots: nil},
		3401: {snapshots: nil},
	})
	if err := testCompareCmd(t, deps, 3307, 3401); err != nil {
		t.Fatalf("compare of two empty servers = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "no databases") {
		t.Errorf("output %q should say both servers serve no databases", out.String())
	}
}

// Each side's password comes from its own variable, and neither reaches the
// output.
func TestComparePasswordsComeFromTheEnvironmentPerSide(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	var asked []string
	var got []doltCompareEndpoint
	deps := doltCompareDeps{
		newReader: func(ep doltCompareEndpoint, _ time.Duration) snapshotReader {
			got = append(got, ep)
			return &fakeSnapshotReader{}
		},
		lookupPassword: func(key string) string {
			asked = append(asked, key)
			return "secret-" + key
		},
		out: out,
	}
	if err := testCompareCmd(t, deps, 3307, 3401); err != nil {
		t.Fatalf("compare = %v, want nil", err)
	}
	if want := []string{doltCompareSourcePasswordEnv, doltCompareTargetPasswordEnv}; !slices.Equal(asked, want) {
		t.Errorf("password variables read = %v, want %v", asked, want)
	}
	if len(got) != 2 {
		t.Fatalf("readers built = %d, want 2", len(got))
	}
	if got[0].Password != "secret-"+doltCompareSourcePasswordEnv || got[1].Password != "secret-"+doltCompareTargetPasswordEnv {
		t.Errorf("reader passwords did not follow their side: %q, %q", got[0].Password, got[1].Password)
	}
	if strings.Contains(out.String(), "secret-") {
		t.Errorf("output %q printed a password", out.String())
	}
}

func TestParseDoltCompareEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    doltCompareEndpoint
		wantErr bool
	}{
		{"host and port", "127.0.0.1:3401", doltCompareEndpoint{Host: "127.0.0.1", Port: 3401, User: "root"}, false},
		{"port defaults to the Dolt port", "db.example.com", doltCompareEndpoint{Host: "db.example.com", Port: 3307, User: "root"}, false},
		{"IPv6 is bracketed", "[::1]:3401", doltCompareEndpoint{Host: "::1", Port: 3401, User: "root"}, false},
		{"empty endpoint", "", doltCompareEndpoint{}, true},
		{"non-numeric port", "127.0.0.1:abc", doltCompareEndpoint{}, true},
		{"port above the range", "127.0.0.1:70000", doltCompareEndpoint{}, true},
		{"zero port", "127.0.0.1:0", doltCompareEndpoint{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDoltCompareEndpoint(tt.raw, "root", "")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDoltCompareEndpoint(%q) = %+v, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDoltCompareEndpoint(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("parseDoltCompareEndpoint(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

// The reader reaches the endpoint it was built for, and the DSN's address is
// that endpoint rather than a default local one.
func TestDoltCompareEndpointDSN(t *testing.T) {
	t.Parallel()
	ep := doltCompareEndpoint{Host: "127.0.0.2", Port: 3401, User: "prover", Password: "hunter2"}
	dsn := ep.dsn(time.Minute)
	for _, want := range []string{"prover:hunter2@tcp(127.0.0.2:3401)/", "timeout=1m0s"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q should contain %q", dsn, want)
		}
	}
	if label := ep.label(); label != "127.0.0.2:3401" {
		t.Errorf("label = %q, want host:port without a credential", label)
	}
}

func TestQuoteCompareIdentifier(t *testing.T) {
	t.Parallel()
	got, err := quoteCompareIdentifier("gastown-hq_2")
	if err != nil || got != "`gastown-hq_2`" {
		t.Errorf("quoteCompareIdentifier = %q, %v", got, err)
	}
	for _, bad := range []string{"", "-leading", "has space", "semi;colon", "back`tick", "quote'"} {
		if got, err := quoteCompareIdentifier(bad); err == nil {
			t.Errorf("quoteCompareIdentifier(%q) = %q, want an error", bad, got)
		}
	}
}
