package rehearsal

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

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
			var tasks []Task
			var want []string
			for i := 1; i <= tc.count; i++ {
				n := fmt.Sprintf("%02d", i)
				tasks = append(tasks, Task{Title: "probe " + n, Body: "body\n## Acceptance\n- [ ] one\n"})
				want = append(want, "rh-"+n)
			}
			// With no rig, Create's argv is bd create --silent --title=..., so argv[3]
			// is the title; the fake answers with its probe number.
			town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
				n, _ := strings.CutPrefix(argv[3], "--title=probe ")
				return []byte("rh-" + n + "\n"), nil
			}}
			ids, err := FileSet(context.Background(), town, tasks)
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

func TestRehearsalFilingFileSetFailure(t *testing.T) {
	t.Parallel()
	bdFailed := errors.New("exit status 1")
	tasks := []Task{{Title: "probe 01", Body: "body\n## Acceptance\n- [ ] one\n"}, {Title: "probe 02", Body: "body\n## Acceptance\n- [ ] one\n"}, {Title: "probe 03", Body: "body\n## Acceptance\n- [ ] one\n"}}
	town := Town{Run: func(_ context.Context, argv []string) ([]byte, error) {
		if argv[3] == "--title=probe 03" {
			return nil, bdFailed
		}
		n, _ := strings.CutPrefix(argv[3], "--title=probe ")
		return []byte("rh-" + n + "\n"), nil
	}}

	ids, err := FileSet(context.Background(), town, tasks)
	if !errors.Is(err, bdFailed) || !slices.Equal(ids, []string{"rh-01", "rh-02"}) {
		t.Fatalf("FileSet = %q, %v; want the two ids filed before the failure and an error wrapping it", ids, err)
	}
}

func TestRehearsalFilingCloseOpenFailure(t *testing.T) {
	t.Parallel()
	bdFailed := errors.New("exit status 1")
	closes := 0
	town := Town{Run: func(_ context.Context, _ []string) ([]byte, error) {
		closes++
		return nil, bdFailed
	}}

	err := CloseOpen(context.Background(), town, []string{"rh-01", "rh-02"}, "run done")
	if !errors.Is(err, bdFailed) || closes != 1 {
		t.Fatalf("CloseOpen = %v after %d closes; want an error wrapping %v after one close", err, closes, bdFailed)
	}
}
