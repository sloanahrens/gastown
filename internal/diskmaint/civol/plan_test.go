package civol

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

const (
	day = 24 * time.Hour
	gb  = 1 << 30 // bytes in one GB, the unit of Entry.Bytes
)

// file is an entry aged age before testNow.
func file(path string, age time.Duration, bytes int64) Entry {
	return Entry{Path: path, MTime: testNow.Add(-age), Bytes: bytes}
}

// volume is an existing volume of sizeGB holding entries.
func volume(name string, sizeGB int64, entries ...Entry) VolumeInput {
	return VolumeInput{Name: name, Exists: true, SizeKB: sizeGB * kbPerGB, Entries: entries}
}

// quiet is a run with no job running, over the live limits.
func quiet(vols ...VolumeInput) Input {
	return Input{Volumes: vols, Limits: DefaultLimits}
}

// removed is the Ref of every action, in plan order.
func removed(p diskmaint.Plan) []string {
	var refs []string
	for _, a := range p.Actions {
		refs = append(refs, a.Ref)
	}
	return refs
}

// keptRule is the rule ref was kept under, and whether it was kept at all.
func keptRule(p diskmaint.Plan, ref string) (string, bool) {
	for _, k := range p.Kept {
		if k.Ref == ref {
			return k.Rule, true
		}
	}
	return "", false
}

// checkRemoved fails unless the plan removes exactly want, in order, and keeps
// every other entry it was given, so nothing is silently dropped. Volume-level
// kept rows carry no ":" in their Ref, so only entry rows are counted.
func checkRemoved(t *testing.T, p diskmaint.Plan, want []string, entries int) {
	t.Helper()
	if !slices.Equal(removed(p), want) {
		t.Fatalf("removed = %v, want %v", removed(p), want)
	}
	if p.Refused != "" {
		t.Fatalf("plan refused: %s", p.Refused)
	}
	var entryRows int
	for _, k := range p.Kept {
		if strings.Contains(k.Ref, ":") {
			entryRows++
		}
	}
	if entryRows != entries-len(want) {
		t.Fatalf("kept %d entry rows, want %d (every entry not removed is kept)", entryRows, entries-len(want))
	}
}

func TestMissingAndUnderCapVolumesLoseNothing(t *testing.T) {
	t.Parallel()
	old := []Entry{file("a", 9*day, gb), file("b", 8*day, gb)}
	tests := map[string]struct {
		vol      VolumeInput
		wantRule string
	}{
		"missing volume is skipped": {
			vol:      VolumeInput{Name: gocacheVolume, Exists: false, SizeKB: 9 * kbPerGB, Entries: old},
			wantRule: ruleMissing,
		},
		"under cap keeps its size": {
			vol:      volume(gocacheVolume, 7, old...),
			wantRule: "under 8 GB cap at 7.0 GB",
		},
		"exactly at cap is left alone": {
			vol:      volume(gocacheVolume, 8, old...),
			wantRule: "under 8 GB cap at 8.0 GB",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := BuildPlan(quiet(tc.vol))
			if len(p.Actions) != 0 {
				t.Fatalf("removed %v, want nothing", removed(p))
			}
			rule, ok := keptRule(p, tc.vol.Name)
			if !ok {
				t.Fatalf("volume %s has no kept row", tc.vol.Name)
			}
			if rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", rule, tc.wantRule)
			}
		})
	}
}

func TestOverCapLosesOldestFirstUntilNeedIsMet(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		vol  VolumeInput
		want []string
	}{
		// need is 4 GB; the 3 GB entry that crosses it is removed too.
		"the entry that crosses need is removed": {
			vol: volume(gocacheVolume, 9,
				file("a", 1*day, 3*gb),
				file("b", 2*day, 3*gb),
				file("c", 3*day, 3*gb)),
			want: []string{"ci-gocache:c", "ci-gocache:b"},
		},
		// need is 4 GB; four 1 GB entries meet it exactly and the rest stay.
		"exact fit stops at need": {
			vol: volume(gocacheVolume, 9,
				file("e1", 1*day, gb), file("e6", 6*day, gb), file("e3", 3*day, gb),
				file("e5", 5*day, gb), file("e2", 2*day, gb), file("e4", 4*day, gb)),
			want: []string{"ci-gocache:e6", "ci-gocache:e5", "ci-gocache:e4", "ci-gocache:e3"},
		},
		// need is 4 GB but only 2 GB exists; every candidate goes.
		"short of need removes every candidate": {
			vol:  volume(gocacheVolume, 9, file("a", 1*day, gb), file("b", 2*day, gb)),
			want: []string{"ci-gocache:b", "ci-gocache:a"},
		},
		// ci-npm 2 GB against its live 1 GB target: need is 1 GB, one entry.
		"npm uses the live 1 GB target": {
			vol: VolumeInput{Name: npmVolume, Exists: true, SizeKB: 2 * kbPerGB, Entries: []Entry{
				file("pkg-a", 1*day, gb), file("pkg-b", 2*day, gb),
			}},
			want: []string{"ci-npm:pkg-b"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRemoved(t, BuildPlan(quiet(tc.vol)), tc.want, len(tc.vol.Entries))
		})
	}
}

func TestGoModuleCacheRemovesOnlyWholeModuleDirectories(t *testing.T) {
	t.Parallel()
	// over the 5 GB gomod cap, need is 3 GB. The oldest entries are the cache
	// and a single file inside a module, neither of which may be an action.
	vol := volume(gomodVolume, 6,
		file("pkg/mod/cache/download/github.com/a/b/@v/v1.0.0.zip", 9*day, 4*gb),
		file("pkg/mod/cache", 8*day, 4*gb),
		file("pkg/mod/github.com/a/b@v1.0.0/go.mod", 7*day, gb),
		file("pkg/mod/github.com/a/b@v1.0.0", 6*day, 5*gb),
		file("pkg/mod/github.com/c/d@v2.0.0", 5*day, gb),
	)
	checkRemoved(t, BuildPlan(quiet(vol)),
		[]string{"ci-gomod:pkg/mod/github.com/a/b@v1.0.0"}, len(vol.Entries))
}

func TestRunningJobTrimsOnlyGocacheOlderThanTheGuard(t *testing.T) {
	t.Parallel()
	// the guard is 65 minutes before the oldest job start, 3h05m before now.
	in := Input{
		Volumes: []VolumeInput{
			volume(gocacheVolume, 9,
				file("old", 4*time.Hour, gb),
				file("boundary", 3*time.Hour+5*time.Minute, gb),
				file("recent", 3*time.Hour, gb)),
			volume(gomodVolume, 6, file("pkg/mod/github.com/a/b@v1.0.0", 9*day, 6*gb)),
			volume(npmVolume, 2, file("pkg-a", 9*day, gb)),
		},
		Limits:         DefaultLimits,
		RunningJobs:    1,
		OldestJobStart: testNow.Add(-2 * time.Hour),
	}
	p := BuildPlan(in)
	if want := []string{"ci-gocache:old"}; !slices.Equal(removed(p), want) {
		t.Fatalf("removed = %v, want %v", removed(p), want)
	}
	// the wording is the one the CI cache script's dry run prints.
	const want = "a job is running; waits for a quiet run"
	for _, ref := range []string{gomodVolume, npmVolume} {
		if rule, ok := keptRule(p, ref); !ok || rule != want {
			t.Fatalf("%s kept under %q (kept=%v), want %q", ref, rule, ok, want)
		}
	}
}

func TestUnknownJobStartLeavesGocacheWaiting(t *testing.T) {
	t.Parallel()
	in := Input{
		Volumes:     []VolumeInput{volume(gocacheVolume, 9, file("old", 9*day, gb))},
		Limits:      DefaultLimits,
		RunningJobs: 1,
	}
	p := BuildPlan(in)
	if len(p.Actions) != 0 {
		t.Fatalf("removed %v, want nothing while the job start is unknown", removed(p))
	}
	if rule, _ := keptRule(p, gocacheVolume); rule != ruleJobUnknown {
		t.Fatalf("rule = %q, want %q", rule, ruleJobUnknown)
	}
}

func TestUnsafePathsRefuseTheWholePlan(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"parent escape":    "../escape",
		"nested parent":    "pkg/mod/../../etc",
		"absolute path":    "/etc/passwd",
		"volume root":      ".",
		"volume root dir":  "./",
		"dot segment":      "aa/./bb",
		"trailing slash":   "aa/",
		"embedded newline": "line\nbreak",
		"carriage return":  "carriage\rreturn",
		"empty path":       "",
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			vol := volume(gocacheVolume, 9, file("good", 9*day, gb), file(path, 8*day, gb))
			p := BuildPlan(quiet(vol))
			if p.Refused == "" {
				t.Fatalf("plan not refused for path %q", path)
			}
			if len(p.Actions) != 0 || len(p.Kept) != 0 {
				t.Fatalf("refused plan carries rows: actions=%v kept=%v", removed(p), p.Kept)
			}
		})
	}
}

func TestDotsInsideANameDoNotRefuseThePlan(t *testing.T) {
	t.Parallel()
	// "cache..v1" holds ".." but no ".." segment, so it is an ordinary entry.
	// It is the oldest and covers the 4 GB need, so it is the one removal.
	vol := volume(gocacheVolume, 9, file("cache..v1", 9*day, 4*gb), file("b", 1*day, gb))
	p := BuildPlan(quiet(vol))
	if p.Refused != "" {
		t.Fatalf("plan refused for a name containing \"..\": %s", p.Refused)
	}
	if want := []string{"ci-gocache:cache..v1"}; !slices.Equal(removed(p), want) {
		t.Fatalf("removed = %v, want %v", removed(p), want)
	}
}

func TestOverCapWithoutEnoughRemovableEntriesExplainsItself(t *testing.T) {
	t.Parallel()
	const want = "over 8 GB cap at 9.0 GB; the removable entries do not reach the 5 GB target"
	tests := map[string]VolumeInput{
		"no entries": volume(gocacheVolume, 9),
		// need is 4 GB; 2 GB exists, so every candidate goes and the volume is still over target.
		"short of need": volume(gocacheVolume, 9, file("a", 1*day, gb), file("b", 2*day, gb)),
	}
	for name, vol := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := BuildPlan(quiet(vol))
			if rule, ok := keptRule(p, gocacheVolume); !ok || rule != want {
				t.Fatalf("volume kept under %q (kept=%v), want %q", rule, ok, want)
			}
		})
	}
}

func TestOverCapReachingTargetHasNoVolumeRow(t *testing.T) {
	t.Parallel()
	// need is 4 GB; the 4 GB entry meets it, so the volume has no shortfall row.
	p := BuildPlan(quiet(volume(gocacheVolume, 9, file("a", 1*day, 4*gb))))
	if _, ok := keptRule(p, gocacheVolume); ok {
		t.Fatalf("volume has a shortfall row although the target was reached")
	}
}

func TestReadErrsRefuseTheWholePlan(t *testing.T) {
	t.Parallel()
	in := quiet(volume(gocacheVolume, 9, file("old", 9*day, gb)))
	in.ReadErrs = []string{"ci-gocache: docker volume inspect failed"}
	p := BuildPlan(in)
	if !strings.Contains(p.Refused, "unreadable") {
		t.Fatalf("Refused = %q, want an unreadable-state refusal", p.Refused)
	}
	if len(p.Actions) != 0 || len(p.Kept) != 0 {
		t.Fatalf("refused plan carries rows: actions=%v kept=%v", removed(p), p.Kept)
	}
}

func TestPlanOrdersByVolumeThenAge(t *testing.T) {
	t.Parallel()
	// volumes and entries are given out of order; the plan lists gocache before
	// npm, and within a volume the oldest entry first.
	in := quiet(
		VolumeInput{Name: npmVolume, Exists: true, SizeKB: 2 * kbPerGB, Entries: []Entry{
			file("pkg-b", 2*day, gb), file("pkg-a", 1*day, gb),
		}},
		volume(gocacheVolume, 9,
			file("f2", 2*day, gb), file("f5", 5*day, gb), file("f1", 1*day, gb),
			file("f4", 4*day, gb), file("f3", 3*day, gb)),
	)
	checkRemoved(t, BuildPlan(in),
		[]string{"ci-gocache:f5", "ci-gocache:f4", "ci-gocache:f3", "ci-gocache:f2", "ci-npm:pkg-b"}, 7)
}
