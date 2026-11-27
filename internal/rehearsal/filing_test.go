package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// probeTasks returns n tasks titled "probe 01" through "probe 0n", each with an acceptance row.
func probeTasks(n int) []Task {
	tasks := make([]Task, 0, n)
	for i := 1; i <= n; i++ {
		tasks = append(tasks, Task{Title: fmt.Sprintf("probe %02d", i), Body: "body\n## Acceptance\n- [ ] one\n"})
	}
	return tasks
}

func TestRehearsalFilingFileSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		count int
	}{
		{name: "sixteen tasks give sixteen ids in order", count: 16},
		{name: "no tasks give no ids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var want []string
			for i := 1; i <= tc.count; i++ {
				want = append(want, fmt.Sprintf("rh-%02d", i))
			}
			// With no rig, Create's argv is bd create --silent --title=..., so argv[3]
			// is the title; the fake answers with its probe number.
			town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
				n, _ := strings.CutPrefix(argv[3], "--title=probe ")
				return []byte("rh-" + n + "\n"), nil
			}}
			ids, err := FileSet(context.Background(), town, probeTasks(tc.count))
			if err != nil || !slices.Equal(ids, want) {
				t.Fatalf("FileSet = %q, %v; want %q, nil", ids, err, want)
			}
		})
	}
}

func TestRehearsalFilingCloseOpen(t *testing.T) {
	t.Parallel()
	var got []string
	town := Town{Rig: "rehearsal", Run: func(_ context.Context, argv []string) ([]byte, error) {
		got = append(got, strings.Join(argv, " "))
		return nil, nil
	}}
	want := []string{
		"bd -C rehearsal close rh-01 --reason=run done",
		"bd -C rehearsal close rh-02 --reason=run done",
		"bd -C rehearsal close rh-03 --reason=run done",
	}

	if err := CloseOpen(context.Background(), town, []string{"rh-01", "rh-02", "rh-03"}, "run done"); err != nil {
		t.Fatalf("CloseOpen = %v; want nil", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("CloseOpen ran %q, want %q", got, want)
	}
}

func TestRehearsalFilingCloseOpenTriesEvery(t *testing.T) {
	t.Parallel()
	closeErr, laterErr := errors.New("close refused"), errors.New("later close refused")
	cases := []struct {
		name    string
		fails   map[string]error
		want    error
		dropped error
	}{
		{name: "a failed close reports the failure and closes the rest", fails: map[string]error{"rh-02": closeErr}, want: closeErr},
		{name: "two failed closes report the first", fails: map[string]error{"rh-01": closeErr, "rh-03": laterErr}, want: closeErr, dropped: laterErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
				got = append(got, argv[2])
				return nil, tc.fails[argv[2]]
			}}

			err := CloseOpen(context.Background(), town, []string{"rh-01", "rh-02", "rh-03"}, "run done")
			if !errors.Is(err, tc.want) || (tc.dropped != nil && errors.Is(err, tc.dropped)) {
				t.Fatalf("CloseOpen = %v; want the first failure %v and not %v", err, tc.want, tc.dropped)
			}
			if want := []string{"rh-01", "rh-02", "rh-03"}; !slices.Equal(got, want) {
				t.Errorf("CloseOpen closed %q, want %q", got, want)
			}
		})
	}
}

func TestRehearsalFilingFileSetClosesWhenCanceled(t *testing.T) {
	t.Parallel()
	createErr, closeErr := errors.New("create refused"), errors.New("close refused")
	cases := []struct {
		name      string
		failClose string
	}{
		{name: "a cancel during a later create closes the beads filed before it and returns the create error"},
		{name: "a failed cleanup close after the cancel is reported with the create error", failClose: "rh-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var closes []string
			town := Town{Run: func(runCtx context.Context, argv []string) ([]byte, error) {
				// The fake town fails any command under a canceled ctx, as a real exec does.
				if err := runCtx.Err(); err != nil {
					return nil, err
				}
				if argv[1] == "close" {
					closes = append(closes, strings.Join(argv, " "))
					if argv[2] == tc.failClose {
						return nil, closeErr
					}
					return nil, nil
				}
				n, _ := strings.CutPrefix(argv[3], "--title=probe ")
				if n == "03" {
					cancel()
					return nil, createErr
				}
				return []byte("rh-" + n + "\n"), nil
			}}

			ids, err := FileSet(ctx, town, probeTasks(3))
			if ids != nil || !errors.Is(err, createErr) || errors.Is(err, closeErr) != (tc.failClose != "") {
				t.Errorf("FileSet = %q, %v; want no ids, an error wrapping %v, and one wrapping %v iff close %q failed", ids, err, createErr, closeErr, tc.failClose)
			}
			wantCloses := []string{"bd close rh-01 --reason=filing failed", "bd close rh-02 --reason=filing failed"}
			if !slices.Equal(closes, wantCloses) {
				t.Errorf("FileSet closed %q, want %q", closes, wantCloses)
			}
		})
	}
}

func TestRehearsalFilingFileSetClosesOnFailure(t *testing.T) {
	t.Parallel()
	createErr, closeErr := errors.New("create refused"), errors.New("close refused")
	cases := []struct {
		name      string
		failClose string
	}{
		{name: "a failed create closes the beads filed before it"},
		{name: "a failed cleanup close is reported with the create failure", failClose: "rh-02"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var closes []string
			town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
				if argv[1] == "close" {
					closes = append(closes, strings.Join(argv, " "))
					if argv[2] == tc.failClose {
						return nil, closeErr
					}
					return nil, nil
				}
				n, _ := strings.CutPrefix(argv[3], "--title=probe ")
				if n == "03" {
					return nil, createErr
				}
				return []byte("rh-" + n + "\n"), nil
			}}

			ids, err := FileSet(context.Background(), town, probeTasks(3))
			if ids != nil || !errors.Is(err, createErr) || errors.Is(err, closeErr) != (tc.failClose != "") {
				t.Errorf("FileSet = %q, %v; want no ids, an error wrapping %v, and one wrapping %v iff close %q failed", ids, err, createErr, closeErr, tc.failClose)
			}
			wantCloses := []string{"bd close rh-01 --reason=filing failed", "bd close rh-02 --reason=filing failed"}
			if !slices.Equal(closes, wantCloses) {
				t.Errorf("FileSet closed %q, want %q", closes, wantCloses)
			}
		})
	}
}
