package metrics

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// exitErrorFromOSRun is the error osRun returns when argv exits non-zero with
// stderr: its message names the command, the exit error and the stderr tail,
// so a fixture built with it matches the error the production path sees.
func exitErrorFromOSRun(argv0 string, err error, stderr string) *exitError {
	return &exitError{
		msg:    fmt.Sprintf("%s: %v: %s", argv0, err, tailString(stderr, 400)),
		stderr: stderr,
		err:    err,
	}
}

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
	err := exitErrorFromOSRun("bd", errors.New("exit status 1"),
		"\n  dolt connection refused\nretrying in 5s\n")
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
	err := exitErrorFromOSRun("bd", errors.New("exit status 1"), stderr)
	gap := bdDailyFailedGap(t, town, err)
	// "dolt: ... " is 10 characters, so the 200 kept are that and 190 a's.
	want := bdDailyGapPrefix(town) + "dolt: ... " + strings.Repeat("a", 190)
	if gap != want {
		t.Fatalf("gap = %q (%d bytes), want %q", gap, len(gap), want)
	}
}

// A credential in stderr is replaced by "...", whether it is a URL's userinfo
// or a driver DSN's user and password. The host is kept, so the receipt still
// says where bd was pointed.
func TestBdDailyGapScrubsCredentialForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "url userinfo",
			stderr: "dial dolt://root:hunter2@127.0.0.1:3307/beads failed",
			want:   "dial dolt://...@127.0.0.1:3307/beads failed",
		},
		{
			name:   "dsn over tcp",
			stderr: "open root:hunter2@tcp(127.0.0.1:3307)/beads failed",
			want:   "open ...@tcp(127.0.0.1:3307)/beads failed",
		},
		{
			name:   "dsn over unix",
			stderr: "open root:hunter2@unix(/run/dolt.sock)/beads failed",
			want:   "open ...@unix(/run/dolt.sock)/beads failed",
		},
		{
			name:   "dsn with default protocol",
			stderr: "open root:hunter2@/beads failed",
			want:   "open ...@/beads failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			err := exitErrorFromOSRun("bd", errors.New("exit status 1"), tc.stderr)
			gap := bdDailyFailedGap(t, town, err)
			if want := bdDailyGapPrefix(town) + tc.want; gap != want {
				t.Fatalf("gap = %q, want %q", gap, want)
			}
		})
	}
}

// A failure with no stderr names the error's own text, and so does an exit
// whose stderr is blank.
func TestBdDailyGapWithoutStderrNamesTheError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		// msg is the message osRun builds for err. A fixture that does not
		// carry it is not the error the production path sees, so the stderr
		// fallback would be tested against a shape bd never produces.
		msg  string
		want string
	}{
		{"plain error", errors.New("signal: killed"), "", "signal: killed"},
		{
			name: "exit with blank stderr",
			err:  exitErrorFromOSRun("bd", errors.New("exit status 1"), " \n\t\n"),
			msg:  "bd: exit status 1: ",
			want: "bd: exit status 1:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.msg != "" && tc.err.Error() != tc.msg {
				t.Fatalf("fixture message = %q, want osRun's %q", tc.err.Error(), tc.msg)
			}
			town := t.TempDir()
			if gap, want := bdDailyFailedGap(t, town, tc.err), bdDailyGapPrefix(town)+tc.want; gap != want {
				t.Fatalf("gap = %q, want %q", gap, want)
			}
		})
	}
}
