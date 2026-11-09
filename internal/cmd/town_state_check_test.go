package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/townstate"
)

// dayOneFailingWriter is an output that refuses every write, so a report's error
// path can be driven.
type dayOneFailingWriter struct{}

func (dayOneFailingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestReportDayOne(t *testing.T) {
	t.Parallel()
	rows := []townstate.Row{
		{Check: "gt status --line", Measured: "exit 0 in 0.40s", Threshold: "exit 0 within 2s", Result: "pass", OK: true},
		{Check: "daemon log: starts", Measured: "-", Threshold: "<= 1 start in 5m after the cut-over", Result: "not run: no cut-over record at daemon/town-state-cutover.json, so the cut-over has not run"},
	}

	t.Run("json prints the same rows and exits 1 when one did not pass", func(t *testing.T) {
		var buf bytes.Buffer
		err := reportDayOne(&buf, rows, true)
		var exitErr *ExitCodeError
		if !errors.As(err, &exitErr) || exitErr.Code != 1 {
			t.Fatalf("err = %v, want an exit code 1", err)
		}
		var got []townstate.Row
		if jerr := json.Unmarshal(buf.Bytes(), &got); jerr != nil {
			t.Fatalf("output is not JSON: %v\n%s", jerr, buf.String())
		}
		if !reflect.DeepEqual(got, rows) {
			t.Errorf("JSON rows = %+v, want %+v", got, rows)
		}
	})

	t.Run("the table prints a header and one line per row", func(t *testing.T) {
		var buf bytes.Buffer
		if err := reportDayOne(&buf, rows, false); err == nil {
			t.Fatal("err = nil, want an exit code 1 for the row that did not run")
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != len(rows)+1 {
			t.Fatalf("got %d lines, want a header and %d rows:\n%s", len(lines), len(rows), buf.String())
		}
		if !strings.HasPrefix(lines[0], "CHECK") || !strings.Contains(lines[0], "RESULT") {
			t.Errorf("header = %q, want the CHECK and RESULT columns", lines[0])
		}
		if !strings.Contains(lines[2], "not run: no cut-over record") {
			t.Errorf("the not-run row = %q, want its reason", lines[2])
		}
	})

	t.Run("all rows passing exits 0", func(t *testing.T) {
		var buf bytes.Buffer
		if err := reportDayOne(&buf, rows[:1], false); err != nil {
			t.Errorf("err = %v, want nil when every row passed", err)
		}
	})

	t.Run("a writer that fails surfaces the error for JSON and for the table", func(t *testing.T) {
		if err := reportDayOne(dayOneFailingWriter{}, rows[:1], true); err == nil || !strings.Contains(err.Error(), "encoding the check rows") {
			t.Errorf("JSON err = %v, want the encoding failure", err)
		}
		if err := reportDayOne(dayOneFailingWriter{}, rows[:1], false); err == nil || !strings.Contains(err.Error(), "printing the check table") {
			t.Errorf("table err = %v, want the printing failure", err)
		}
	})
}

// townWithRig is a town root whose registry names one rig, for a command run
// that reaches the checks or the baseline record.
func townWithRig(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigs := `{"version":1,"rigs":{"gastown":{"git_url":"https://example.invalid/gastown.git"}}}`
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(rigs), 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}

func TestRunTownStateCheckIn(t *testing.T) {
	t.Parallel()
	instant := func() time.Time { return time.Unix(0, 0) }
	silent := func(context.Context, string, string, ...string) (string, int, error) { return "", 0, nil }

	t.Run("force without the record is refused before the town is found", func(t *testing.T) {
		var buf bytes.Buffer
		err := runTownStateCheckIn(context.Background(), townStateCheckEnv{
			force:    true,
			findTown: func() (string, error) { t.Fatal("the town was looked up for a refused flag set"); return "", nil },
			run:      silent, now: instant, out: &buf,
		})
		if err == nil || !strings.Contains(err.Error(), "--force applies only with --record-baseline") {
			t.Fatalf("err = %v, want the flag refusal", err)
		}
	})

	t.Run("a town root that cannot be found is named", func(t *testing.T) {
		var buf bytes.Buffer
		err := runTownStateCheckIn(context.Background(), townStateCheckEnv{
			findTown: func() (string, error) { return "", errors.New("no town here") },
			run:      silent, now: instant, out: &buf,
		})
		if err == nil || !strings.Contains(err.Error(), "finding the town root") {
			t.Fatalf("err = %v, want the town root named", err)
		}
	})

	t.Run("the baseline is recorded once, and a second record is refused", func(t *testing.T) {
		town := townWithRig(t)
		var first bytes.Buffer
		env := townStateCheckEnv{
			record:   true,
			findTown: func() (string, error) { return town, nil },
			run:      silent, now: instant, out: &first,
		}
		if err := runTownStateCheckIn(context.Background(), env); err != nil {
			t.Fatalf("first record: %v", err)
		}
		if !strings.Contains(first.String(), "recorded the git speed baseline at") {
			t.Errorf("output = %q, want the recorded line", first.String())
		}
		var second bytes.Buffer
		env.out = &second
		err := runTownStateCheckIn(context.Background(), env)
		if err == nil || !strings.Contains(err.Error(), "already recorded") {
			t.Fatalf("second record err = %v, want the already-recorded refusal", err)
		}
	})
}

func TestRunTownStateCheckInCommandsThatCannotStart(t *testing.T) {
	t.Parallel()
	// Every command fails to start, so each check that runs one is not run, and
	// the command exits 1 rather than reading the failed starts as a clean pass.
	notFound := errors.New(`exec: "gt": executable file not found in $PATH`)
	var buf bytes.Buffer
	err := runTownStateCheckIn(context.Background(), townStateCheckEnv{
		findTown: func() (string, error) { return townWithRig(t), nil },
		run: func(context.Context, string, string, ...string) (string, int, error) {
			return "", 0, notFound
		},
		now: func() time.Time { return time.Unix(0, 0) },
		out: &buf,
	})
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("err = %v, want an exit code 1", err)
	}
	if !strings.Contains(buf.String(), "not run: "+notFound.Error()) {
		t.Errorf("table = %q, want a not-run row carrying the start error", buf.String())
	}
}
