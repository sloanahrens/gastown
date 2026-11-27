package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeAnswer is what the fake runner returns for one argv. out is returned
// with err too, so a test can check that the error does not carry it.
type fakeAnswer struct {
	out string
	err error
}

// fakeRun answers each argv from answers, keyed by the argv joined with spaces,
// and appends every argv it runs to ran.
func fakeRun(answers map[string]fakeAnswer, ran *[]string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, argv []string) ([]byte, error) {
		key := strings.Join(argv, " ")
		*ran = append(*ran, key)
		a, ok := answers[key]
		if !ok {
			return nil, fmt.Errorf("unexpected argv %q", key)
		}
		return []byte(a.out), a.err
	}
}

func TestRehearsalTownReadBead(t *testing.T) {
	t.Parallel()
	const showArgv, commentsArgv = "bd show gt-1 --json", "bd comments gt-1 --json"
	const showOK = `{"data":[{"id":"gt-1","status":"open","notes":"READY"}]}`
	const commentsOK = `{"data":[{"text":"first"},{"text":"second"}]}`
	cause := errors.New("exit status 1")
	const leaked = "bd: secret output"
	cases := []struct {
		name    string
		answers map[string]fakeAnswer
		wantRan []string
		want    BeadState
		wantErr bool
		cause   error
		// named is the command the error must name, so a caller can tell which read failed.
		named string
	}{
		{name: "status, notes and comment texts are decoded", answers: map[string]fakeAnswer{
			showArgv: {out: showOK}, commentsArgv: {out: commentsOK}},
			wantRan: []string{showArgv, commentsArgv}, want: BeadState{Status: "open", Notes: "READY", Comments: []string{"first", "second"}}},
		{name: "a bead with no comments has none", answers: map[string]fakeAnswer{
			showArgv: {out: showOK}, commentsArgv: {out: `{"data":[]}`}},
			wantRan: []string{showArgv, commentsArgv}, want: BeadState{Status: "open", Notes: "READY"}},
		{name: "a failed show stops the read and carries no output", answers: map[string]fakeAnswer{
			showArgv: {out: leaked, err: cause}},
			wantRan: []string{showArgv}, wantErr: true, cause: cause, named: "bd show gt-1"},
		{name: "a failed comments read is wrapped and carries no output", answers: map[string]fakeAnswer{
			showArgv: {out: showOK}, commentsArgv: {out: leaked, err: cause}},
			wantRan: []string{showArgv, commentsArgv}, wantErr: true, cause: cause, named: "bd comments gt-1"},
		{name: "a show answer that does not decode is an error", answers: map[string]fakeAnswer{
			showArgv: {out: "not json"}}, wantRan: []string{showArgv}, wantErr: true, named: "bd show gt-1"},
		{name: "a show answer with no entry is an error", answers: map[string]fakeAnswer{
			showArgv: {out: `{"data":[]}`}}, wantRan: []string{showArgv}, wantErr: true, named: "bd show gt-1"},
		{name: "a comments answer that does not decode is an error", answers: map[string]fakeAnswer{
			showArgv: {out: showOK}, commentsArgv: {out: "{"}},
			wantRan: []string{showArgv, commentsArgv}, wantErr: true, named: "bd comments gt-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			got, err := Town{Run: fakeRun(tc.answers, &ran)}.Bead(context.Background(), "gt-1")
			if !slices.Equal(ran, tc.wantRan) {
				t.Errorf("Bead ran %q, want %q", ran, tc.wantRan)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Bead = %+v, nil; want an error", got)
				}
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Errorf("Bead error = %v, want it to wrap %v", err, tc.cause)
				}
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("Bead error %q carries command output", err)
				}
				if !strings.Contains(err.Error(), tc.named) {
					t.Errorf("Bead error = %v, want it to name %q", err, tc.named)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bead error = %v, want nil", err)
			}
			if got.Status != tc.want.Status || got.Notes != tc.want.Notes || !slices.Equal(got.Comments, tc.want.Comments) {
				t.Errorf("Bead = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRehearsalTownReadPolecats(t *testing.T) {
	t.Parallel()
	const argv = "gt polecat list gastown --json"
	cause := errors.New("exit status 1")
	const leaked = "gt: secret output"
	cases := []struct {
		name    string
		answer  fakeAnswer
		want    string
		wantErr bool
	}{
		{name: "the listing's bytes are returned as printed", answer: fakeAnswer{out: `[{"name":"furiosa"}]`}, want: `[{"name":"furiosa"}]`},
		{name: "a failed command is wrapped and carries no output", answer: fakeAnswer{out: leaked, err: cause}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			got, err := Town{Rig: "gastown", Run: fakeRun(map[string]fakeAnswer{argv: tc.answer}, &ran)}.Polecats(context.Background())
			if !slices.Equal(ran, []string{argv}) {
				t.Fatalf("Polecats ran %q, want %q", ran, argv)
			}
			if tc.wantErr {
				if !errors.Is(err, cause) || strings.Contains(err.Error(), leaked) {
					t.Errorf("Polecats error = %v, want it to wrap %v and carry no output", err, cause)
				}
				if !strings.Contains(err.Error(), "gt polecat list") {
					t.Errorf("Polecats error = %v, want it to name %q", err, "gt polecat list")
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("Polecats = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

func TestRehearsalTownReadDaemonLog(t *testing.T) {
	t.Parallel()
	const argv = "gt daemon logs -n 400000"
	cause := errors.New("exit status 1")
	const leaked = "gt: secret output"
	cases := []struct {
		name    string
		answer  fakeAnswer
		want    string
		wantErr bool
	}{
		{name: "the log's bytes are returned as printed", answer: fakeAnswer{out: "2026/11/27 01:00:00 tick\n"}, want: "2026/11/27 01:00:00 tick\n"},
		{name: "a failed command is wrapped and carries no output", answer: fakeAnswer{out: leaked, err: cause}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			got, err := Town{Run: fakeRun(map[string]fakeAnswer{argv: tc.answer}, &ran)}.DaemonLog(context.Background())
			if !slices.Equal(ran, []string{argv}) {
				t.Fatalf("DaemonLog ran %q, want %q", ran, argv)
			}
			if tc.wantErr {
				if !errors.Is(err, cause) || strings.Contains(err.Error(), leaked) {
					t.Errorf("DaemonLog error = %v, want it to wrap %v and carry no output", err, cause)
				}
				if !strings.Contains(err.Error(), "gt daemon logs") {
					t.Errorf("DaemonLog error = %v, want it to name %q", err, "gt daemon logs")
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Errorf("DaemonLog = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}
