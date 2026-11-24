package daemon

import (
	"fmt"
	"strconv"
	"strings"
)

// Loose object count and size (KiB) at or above which the archive is packed.
const (
	jsonlRepackLooseCount = 2000
	jsonlRepackLooseKiB   = 1048576
)

// jsonlRepackArgs follows `git -C <repo>`: one pack thread, auto-gc off and a
// 256 MiB pack window bound it. --aggressive stays out: the measured run went unbounded.
var jsonlRepackArgs = []string{"-c", "pack.threads=1", "-c", "gc.auto=0", "-c", "pack.windowMemory=256m", "gc", "--prune=2.weeks.ago"}

// RepackPlan is one archive repository's decision. When Due, Args is the
// bounded repack command to run after `git -C <repo>`; otherwise Args is empty.
type RepackPlan struct {
	Due    bool
	Reason string
	Args   []string
}

// RepackPlanFor decides from `git count-objects -v` text (size in KiB) whether the
// repository needs a bounded pack: a loose count at or over
// jsonlRepackLooseCount or a loose size at or over jsonlRepackLooseKiB. A
// missing or non-numeric count or size line is an error naming the line.
func RepackPlanFor(countObjectsOutput string) (RepackPlan, error) {
	count, err := countObjectsValue(countObjectsOutput, "count")
	if err != nil {
		return RepackPlan{}, err
	}
	sizeKiB, err := countObjectsValue(countObjectsOutput, "size")
	if err != nil {
		return RepackPlan{}, err
	}
	due := count >= jsonlRepackLooseCount || sizeKiB >= jsonlRepackLooseKiB
	plan := RepackPlan{
		Due:    due,
		Reason: fmt.Sprintf("%d loose objects (threshold %d), %d KiB loose (threshold %d KiB)", count, jsonlRepackLooseCount, sizeKiB, jsonlRepackLooseKiB),
	}
	if due {
		plan.Args = append([]string(nil), jsonlRepackArgs...)
	}
	return plan, nil
}

// ReadRepackPlan reads one archive repository through run with
// `git -C <repo> count-objects -v` and returns its plan. Args in the plan are
// returned unchanged: the caller prefixes `git -C <repo>`. A run or parse error
// is returned naming the repository.
func ReadRepackPlan(gitRepo string, run func(argv ...string) ([]byte, error)) (RepackPlan, error) {
	out, err := run("git", "-C", gitRepo, "count-objects", "-v")
	if err != nil {
		return RepackPlan{}, fmt.Errorf("read count-objects of repository %q: %w", gitRepo, err)
	}
	plan, err := RepackPlanFor(string(out))
	if err != nil {
		return RepackPlan{}, fmt.Errorf("repository %q: %w", gitRepo, err)
	}
	return plan, nil
}

// countObjectsValue returns the value of the count-objects line whose key is
// exactly key, so size-pack does not answer for size.
func countObjectsValue(output, key string) (int64, error) {
	for _, line := range strings.Split(output, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("count-objects line %q is not a number", strings.TrimSpace(line))
		}
		return n, nil
	}
	return 0, fmt.Errorf("count-objects output has no %q line", key+":")
}
