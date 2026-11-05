package metrics

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bdDailyFailedGap runs the daily snapshot over the rig gastown in town, whose
// bd fails with err in every directory, and returns the gap text that rig's
// line carries.
func bdDailyFailedGap(t *testing.T, town string, err error) string {
	t.Helper()
	env := Env{
		Town: town,
		ReadFile: func(path string) ([]byte, error) {
			if path == filepath.Join(town, "mayor", "rigs.json") {
				return []byte(bdDailyRigs("gastown")), nil
			}
			return nil, fs.ErrNotExist
		},
		RunDir: func(context.Context, string, []string) ([]byte, error) { return nil, err },
		Now:    func() time.Time { return bdDailyNow },
	}
	res, runErr := RunBdDaily(context.Background(), env)
	var gaps *BdDailyGapsError
	if !errors.As(runErr, &gaps) || !reflect.DeepEqual(gaps.Rigs, []string{"gastown"}) {
		t.Fatalf("RunBdDaily = %+v, %v, want a gap error naming gastown", res, runErr)
	}
	lines := bdDailyRead(t, env)
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want the one gap", len(lines))
	}
	gap, ok := lines[0]["gap"].(string)
	if !ok {
		t.Fatalf("line %v carries no gap text", lines[0])
	}
	return gap
}

// bdDailyGapPrefix is the text a failed listing of the gastown rig in town
// starts with.
func bdDailyGapPrefix(town string) string {
	return "bd list failed in " + filepath.Join(town, "gastown") + ": "
}

// A failed listing's gap names bd's first non-blank stderr line, so the cause
// is on the receipt and not only in the bd output.
func TestBdDailyGapNamesBdsFirstStderrLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	err := &exitError{
		msg:    "bd: exit status 1: \n  dolt connection refused\nretrying in 5s\n",
		stderr: "\n  dolt connection refused\nretrying in 5s\n",
		err:    errors.New("exit status 1"),
	}
	gap := bdDailyFailedGap(t, town, err)
	if want := bdDailyGapPrefix(town) + "dolt connection refused"; gap != want {
		t.Fatalf("gap = %q, want %q", gap, want)
	}
}

// The stderr that a gap names is bounded to 200 characters, and a NAME=value
// pair in it is replaced by "...": the value is never on the receipt.
func TestBdDailyGapStderrIsBoundedAndScrubbed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	stderr := "dolt: KEY=secretvalue " + strings.Repeat("a", 5000)
	err := &exitError{msg: "bd: exit status 1", stderr: stderr, err: errors.New("exit status 1")}
	gap := bdDailyFailedGap(t, town, err)
	// "dolt: ... " is 10 characters, so the 200 kept are that and 190 a's.
	want := bdDailyGapPrefix(town) + "dolt: ... " + strings.Repeat("a", 190)
	if gap != want {
		t.Fatalf("gap = %q (%d bytes), want %q", gap, len(gap), want)
	}
}

// A credential in a URL in stderr is replaced by "...", and the host is kept so
// the receipt still says where bd was pointed.
func TestBdDailyGapScrubsCredentialURL(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	err := &exitError{
		msg:    "bd: exit status 1",
		stderr: "dial dolt://root:hunter2@127.0.0.1:3307/beads failed\n",
		err:    errors.New("exit status 1"),
	}
	gap := bdDailyFailedGap(t, town, err)
	if want := bdDailyGapPrefix(town) + "dial dolt://...@127.0.0.1:3307/beads failed"; gap != want {
		t.Fatalf("gap = %q, want %q", gap, want)
	}
}

// A failure with no stderr names the error's own text, and so does an exit
// whose stderr is blank.
func TestBdDailyGapWithoutStderrNamesTheError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"plain error", errors.New("signal: killed"), "signal: killed"},
		{"exit with blank stderr", &exitError{msg: "bd: exit status 1", stderr: " \n\t\n", err: errors.New("exit status 1")}, "bd: exit status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if gap, want := bdDailyFailedGap(t, town, tc.err), bdDailyGapPrefix(town)+tc.want; gap != want {
				t.Fatalf("gap = %q, want %q", gap, want)
			}
		})
	}
}
