package daemon

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func countObjects(count, size string) string {
	return "count: " + count + "\nsize: " + size + "\nin-pack: 0\npacks: 0\nsize-pack: 0\ngarbage: 0\n"
}

// TestRepackPlanThresholds: due when the loose count or loose size reaches its
// threshold, and only then does the plan carry the bounded command.
func TestRepackPlanThresholds(t *testing.T) {
	t.Parallel()
	want := []string{"-c", "pack.threads=1", "-c", "gc.auto=0", "-c", "pack.windowMemory=256m", "gc", "--prune=2.weeks.ago"}
	for _, tc := range []struct {
		name, output string
		due          bool
	}{
		{"the live archive, 8103 objects and 13893132 KiB", countObjects("8103", "13893132"), true},
		{"a quiet repository, 100 objects and 4000 KiB", countObjects("100", "4000"), false},
		{"one object under both thresholds", countObjects("1999", "1048575"), false},
		{"the loose count alone at its threshold", countObjects("2000", "0"), true},
		{"the loose size alone at its threshold", countObjects("0", "1048576"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := RepackPlanFor(tc.output)
			if err != nil {
				t.Fatalf("RepackPlanFor(%q) error = %v", tc.output, err)
			}
			if plan.Due != tc.due {
				t.Errorf("RepackPlanFor(%q).Due = %v, want %v", tc.output, plan.Due, tc.due)
			}
			if got := reflect.DeepEqual(plan.Args, want); got != tc.due {
				t.Errorf("RepackPlanFor(%q).Args = %q, want the bounded %q exactly when due (never --aggressive)",
					tc.output, plan.Args, want)
			}
		})
	}
}

// TestReadRepackPlanRunsCountObjects: ReadRepackPlan asks the runner for
// `git -C <repo> count-objects -v`, plans the live archive's 8103 loose objects
// with the bounded args unchanged, and a run error is returned naming the
// repository.
func TestReadRepackPlanRunsCountObjects(t *testing.T) {
	t.Parallel()
	const repo = "/archive/git"

	t.Run("asks run for count-objects -v and plans the output", func(t *testing.T) {
		t.Parallel()
		var gotArgv []string
		plan, err := ReadRepackPlan(repo, func(argv ...string) ([]byte, error) {
			gotArgv = append([]string(nil), argv...)
			return []byte(countObjects("8103", "13893132")), nil
		})
		if err != nil {
			t.Fatalf("ReadRepackPlan(%q) error = %v", repo, err)
		}
		if want := []string{"git", "-C", repo, "count-objects", "-v"}; !reflect.DeepEqual(gotArgv, want) {
			t.Errorf("run argv = %q, want %q", gotArgv, want)
		}
		if !plan.Due || !reflect.DeepEqual(plan.Args, jsonlRepackArgs) {
			t.Errorf("ReadRepackPlan(%q) = %+v, want Due with the bounded %q unchanged", repo, plan, jsonlRepackArgs)
		}
	})

	t.Run("a run error is returned naming the repository", func(t *testing.T) {
		t.Parallel()
		runErr := errors.New("exit status 128")
		_, err := ReadRepackPlan(repo, func(...string) ([]byte, error) { return nil, runErr })
		if err == nil {
			t.Fatalf("ReadRepackPlan(%q) with a failing run = nil error, want one naming %q", repo, repo)
		}
		if !strings.Contains(err.Error(), repo) {
			t.Errorf("ReadRepackPlan(%q) error = %q, want it to name the repository", repo, err)
		}
		if !errors.Is(err, runErr) {
			t.Errorf("ReadRepackPlan(%q) error = %q, want it to wrap the run error", repo, err)
		}
	})

	t.Run("an unreadable count-objects line names the repository", func(t *testing.T) {
		t.Parallel()
		run := func(...string) ([]byte, error) { return []byte("size: 4000\n"), nil }
		if _, err := ReadRepackPlan(repo, run); err == nil || !strings.Contains(err.Error(), repo) {
			t.Errorf("ReadRepackPlan(%q) error = %v, want one naming %q", repo, err, repo)
		}
	})
}

// TestRepackPlanRefusesAnUnreadableLine: a missing, non-numeric or lookalike
// count or size line is an error naming the line, never a zero read as clean.
func TestRepackPlanRefusesAnUnreadableLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output, wantErr string
	}{
		{"no count line", "size: 4000\nin-pack: 0\npacks: 0\nsize-pack: 0\ngarbage: 0\n", "count:"},
		{"no size line", "count: 100\nin-pack: 0\npacks: 0\nsize-pack: 0\ngarbage: 0\n", "size:"},
		{"a size that is not a number", countObjects("100", "abc"), "size: abc"},
		{"a count that is not a number", countObjects("many", "4000"), "count: many"},
		{"keys that only end like count and size", "in-pack: 8103\npacks: 3\nsize-pack: 13893132\ngarbage: 0\n", "count:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := RepackPlanFor(tc.output)
			if err == nil {
				t.Fatalf("RepackPlanFor(%q) = %+v, want an error naming %q", tc.output, plan, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("RepackPlanFor(%q) error = %q, want it to name %q", tc.output, err, tc.wantErr)
			}
		})
	}
}
