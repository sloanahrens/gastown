package tiersweep

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeHost is the Host ParseOptions reads in a test: an environment map, the
// directory MkdirTemp hands back, and a record of the calls both directory
// seams saw.
type fakeHost struct {
	env      map[string]string
	tmpDir   string
	tmpErr   error
	mkdirErr error
	temps    []tempCall
	mkdirs   []string
	// readDirs records each directory a prune listed. listings is what ReadDir
	// answers per directory (a directory it does not name is empty), and infos
	// what Lstat answers per path (a path it does not name does not exist).
	// removed records each RemoveAll path, and removeErr fails one of them.
	readDirs  []string
	listings  map[string][]fs.DirEntry
	infos     map[string]fs.FileInfo
	infoErrs  map[string]error
	readErr   error
	removed   []string
	removeErr map[string]error
}

// tempCall is one MkdirTemp call: the parent and the pattern it took.
type tempCall struct{ dir, pattern string }

func (f *fakeHost) host() Host {
	return Host{
		Env: func(name string) string { return f.env[name] },
		MkdirTemp: func(dir, pattern string) (string, error) {
			f.temps = append(f.temps, tempCall{dir: dir, pattern: pattern})
			if f.tmpErr != nil {
				return "", f.tmpErr
			}
			return f.tmpDir, nil
		},
		MkdirAll: func(path string, perm fs.FileMode) error {
			f.mkdirs = append(f.mkdirs, path)
			return f.mkdirErr
		},
		ReadDir: func(dir string) ([]fs.DirEntry, error) {
			f.readDirs = append(f.readDirs, dir)
			if f.readErr != nil {
				return nil, f.readErr
			}
			return f.listings[dir], nil
		},
		Lstat: func(name string) (fs.FileInfo, error) {
			if err := f.infoErrs[name]; err != nil {
				return nil, err
			}
			info, ok := f.infos[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return info, nil
		},
		RemoveAll: func(path string) error {
			f.removed = append(f.removed, path)
			return f.removeErr[path]
		},
	}
}

// madeDir is what the fake MkdirTemp creates, standing in for
// mktemp -d "$TMPDIR/tier-sweep.XXXXXX".
const madeDir = "/tmp/tier-sweep.aB12"

// TestParseOptions: the tiers a command line names, and each environment name
// the run reads at its default and overridden. A name set to the empty string
// takes the default, the script's ${NAME:=...} rule.
func TestParseOptions(t *testing.T) {
	t.Parallel()
	defaults := Options{
		Tiers:        []string{TierShell, TierIntegration, TierRace},
		Timeout:      DefaultTimeout,
		LogDir:       madeDir,
		OwnsLogDir:   true,
		LogDirParent: DefaultTempDir,
		Jobs:         1,
	}
	defaultTemps := []tempCall{{dir: DefaultTempDir, pattern: logDirPattern}}
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    Options
		wantTmp []tempCall
		wantMk  []string
	}{
		{
			name:    "no arguments is the script's defaults",
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "the tiers keep the order given",
			args: []string{TierRace, TierShell},
			want: Options{Tiers: []string{TierRace, TierShell}, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Jobs: 1},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "an unknown tier stays in the list for the run to report",
			args: []string{TierShell, "bogus", TierRace},
			want: Options{Tiers: []string{TierShell, "bogus", TierRace}, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Jobs: 1},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "TMPDIR names the default log directory's parent",
			env:  map[string]string{"TMPDIR": "/var/tmp"},
			want: Options{Tiers: defaults.Tiers, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: "/var/tmp", Jobs: 1},
			wantTmp: []tempCall{{dir: "/var/tmp", pattern: logDirPattern}},
			wantMk:  []string{madeDir},
		},
		{
			name:    "an empty TMPDIR leaves the default parent",
			env:     map[string]string{"TMPDIR": ""},
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name:   "TIER_SWEEP_LOGDIR is created and used as given",
			env:    map[string]string{EnvLogDir: "/logs/sweep"},
			want:   Options{Tiers: defaults.Tiers, Timeout: DefaultTimeout, LogDir: "/logs/sweep", Jobs: 1},
			wantMk: []string{"/logs/sweep"},
		},
		{
			name:    "an empty TIER_SWEEP_LOGDIR takes the default",
			env:     map[string]string{EnvLogDir: ""},
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "TIER_SWEEP_SKIP is the skip list",
			env:  map[string]string{EnvSkip: "integration:./a race:./b:^TestC$"},
			want: Options{Tiers: defaults.Tiers, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Jobs: 1,
				SkipList: "integration:./a race:./b:^TestC$"},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name:    "an empty TIER_SWEEP_SKIP skips nothing",
			env:     map[string]string{EnvSkip: ""},
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "TIER_SWEEP_TIMEOUT is the per-package timeout",
			env:  map[string]string{EnvTimeout: "30m"},
			want: Options{Tiers: defaults.Tiers, Timeout: "30m", LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Jobs: 1},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name:    "an empty TIER_SWEEP_TIMEOUT takes 10m",
			env:     map[string]string{EnvTimeout: ""},
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "TIER_SWEEP_TRIGGER is passed through as given",
			env:  map[string]string{EnvTrigger: "0"},
			want: Options{Tiers: defaults.Tiers, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Trigger: "0", Jobs: 1},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name: "TIER_SWEEP_TESTED is the commit a run tests",
			env:  map[string]string{EnvTested: "abc123"},
			want: Options{Tiers: defaults.Tiers, Timeout: DefaultTimeout, LogDir: madeDir,
				OwnsLogDir: true, LogDirParent: DefaultTempDir, Tested: "abc123", Jobs: 1},
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
		{
			name:    "SHELL_TESTS is the shell-tier runner's, not the sweep's",
			env:     map[string]string{"SHELL_TESTS": "scripts/x_test.sh"},
			want:    defaults,
			wantTmp: defaultTemps,
			wantMk:  []string{madeDir},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeHost{env: tc.env, tmpDir: madeDir}
			got, err := ParseOptions(tc.args, f.host())
			if err != nil {
				t.Fatalf("ParseOptions(%q): %v", tc.args, err)
			}
			if len(got.Tiers) != len(tc.want.Tiers) {
				t.Fatalf("Tiers = %q, want %q", got.Tiers, tc.want.Tiers)
			}
			for i := range tc.want.Tiers {
				if got.Tiers[i] != tc.want.Tiers[i] {
					t.Errorf("Tiers = %q, want %q", got.Tiers, tc.want.Tiers)
					break
				}
			}
			got.Tiers = nil
			rest := tc.want
			rest.Tiers = nil
			if !reflect.DeepEqual(got, rest) {
				t.Errorf("options = %+v, want %+v", got, tc.want)
			}
			if len(f.temps) != len(tc.wantTmp) {
				t.Fatalf("MkdirTemp calls = %v, want %v", f.temps, tc.wantTmp)
			}
			for i, c := range tc.wantTmp {
				if f.temps[i] != c {
					t.Errorf("MkdirTemp call %d = %v, want %v", i, f.temps[i], c)
				}
			}
			if len(f.mkdirs) != len(tc.wantMk) {
				t.Fatalf("MkdirAll calls = %v, want %v", f.mkdirs, tc.wantMk)
			}
			for i, dir := range tc.wantMk {
				if f.mkdirs[i] != dir {
					t.Errorf("MkdirAll call %d = %q, want %q", i, f.mkdirs[i], dir)
				}
			}
		})
	}
}

// TestParseOptionsLogDirFailures: a log directory that cannot be created is an
// error, never a run with an empty LogDir and no place to write its logs.
func TestParseOptionsLogDirFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		host *fakeHost
	}{
		{name: "the default directory cannot be created", host: &fakeHost{tmpErr: errors.New("no space")}},
		{name: "TIER_SWEEP_LOGDIR cannot be created", host: &fakeHost{
			env:      map[string]string{EnvLogDir: "/logs/sweep"},
			mkdirErr: errors.New("permission denied"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, err := ParseOptions(nil, tc.host.host())
			if err == nil {
				t.Fatalf("ParseOptions: no error, options %+v", opts)
			}
			if opts.LogDir != "" || opts.Tiers != nil {
				t.Errorf("ParseOptions returned options %+v beside %v, want none", opts, err)
			}
		})
	}
}

// TestParseOptionsJobsAccepts: TIER_SWEEP_JOBS unset is one package at a time,
// and each value the range allows parses as that count.
func TestParseOptionsJobsAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  int
	}{
		{value: "", want: 1},
		{value: "1", want: 1},
		{value: "4", want: 4},
		{value: "16", want: MaxJobs},
	}
	for _, tc := range cases {
		t.Run("value "+tc.value, func(t *testing.T) {
			t.Parallel()
			f := &fakeHost{env: map[string]string{EnvJobs: tc.value}, tmpDir: madeDir}
			opts, err := ParseOptions(nil, f.host())
			if err != nil {
				t.Fatalf("ParseOptions(EnvJobs=%q): %v", tc.value, err)
			}
			if opts.Jobs != tc.want {
				t.Errorf("Jobs = %d, want %d", opts.Jobs, tc.want)
			}
		})
	}
}

// TestParseOptionsJobsRefusals: a count outside 1 to 16, or one that is not a
// whole number, is a usage error that names the variable and the value, so a
// typo never changes how many tests run at once.
func TestParseOptionsJobsRefusals(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-1", "17", "abc", "2.0", "1 0"} {
		t.Run("value "+value, func(t *testing.T) {
			t.Parallel()
			f := &fakeHost{env: map[string]string{EnvJobs: value}, tmpDir: madeDir}
			opts, err := ParseOptions(nil, f.host())
			if err == nil {
				t.Fatalf("ParseOptions(EnvJobs=%q): no error, options %+v", value, opts)
			}
			if opts.Jobs != 0 {
				t.Errorf("ParseOptions returned Jobs %d beside %v, want none", opts.Jobs, err)
			}
			for _, want := range []string{EnvJobs, value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
			// The same refusal through the command: the usage exit, and no
			// log directory made on the way.
			var stderr bytes.Buffer
			code := Run(nil, io.Discard, &stderr, Deps{Host: f.host()})
			if code != UsageExit {
				t.Errorf("Run exit = %d, want the usage exit %d", code, UsageExit)
			}
			if len(f.mkdirs) != 0 || len(f.temps) != 0 {
				t.Errorf("a refused value made directories: %v %v", f.temps, f.mkdirs)
			}
		})
	}
}

// TestParseOptionsUnknownTier: the line the command prints for a tier it does
// not offer, the exit code it leaves with, and the daemon's regexp over that
// line (tierSweepUnknownTierRE in internal/daemon/tier_sweep.go).
func TestParseOptionsUnknownTier(t *testing.T) {
	t.Parallel()
	const want = "tier-sweep: unknown tier bogus (want shell, integration or race)"
	if got := (&UnknownTierError{Tier: "bogus"}).Error(); got != want {
		t.Errorf("unknown tier line = %q, want %q", got, want)
	}
	if UsageExit != 2 {
		t.Errorf("UsageExit = %d, want 2 (the script's exit for an unknown tier)", UsageExit)
	}
	// A copy of the daemon's tierSweepUnknownTierRE.
	unknownTierRE := regexp.MustCompile(`^tier-sweep: unknown tier (\S+)`)
	m := unknownTierRE.FindStringSubmatch(want)
	if m == nil || m[1] != "bogus" {
		t.Errorf("the daemon's regexp over %q = %q, want the tier bogus", want, m)
	}
	for _, tier := range []string{TierShell, TierIntegration, TierRace} {
		if !KnownTier(tier) {
			t.Errorf("KnownTier(%q) = false, want true", tier)
		}
	}
	if KnownTier("bogus") {
		t.Error("KnownTier(\"bogus\") = true, want false")
	}
}

// TestSkipForTable: TIER_SWEEP_SKIP's entries and the package each one
// answers for, including the first entry winning and a package whose name
// merely extends another's not matching it.
func TestSkipForTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		list        string
		tier        string
		pkg         string
		wantWhole   bool
		wantPattern string
	}{
		{name: "no entries skips nothing", tier: TierIntegration, pkg: "./internal/cmd"},
		{
			name:      "a whole-package entry",
			list:      "integration:./internal/cmd",
			tier:      TierIntegration,
			pkg:       "./internal/cmd",
			wantWhole: true,
		},
		{
			name:        "a regexp entry",
			list:        "integration:./internal/cmd:^TestA$",
			tier:        TierIntegration,
			pkg:         "./internal/cmd",
			wantPattern: "^TestA$",
		},
		{
			name:        "the regexp is everything after the third colon",
			list:        "integration:./internal/cmd:^TestA$|TestB:sub",
			tier:        TierIntegration,
			pkg:         "./internal/cmd",
			wantPattern: "^TestA$|TestB:sub",
		},
		{
			name:        "the first matching entry wins",
			list:        "integration:./p:^TestA$ integration:./p:^TestB$",
			tier:        TierIntegration,
			pkg:         "./p",
			wantPattern: "^TestA$",
		},
		{
			name:      "a whole entry early in the list wins over a later regexp one",
			list:      "integration:./p integration:./p:^TestA$",
			tier:      TierIntegration,
			pkg:       "./p",
			wantWhole: true,
		},
		{
			name: "another tier's entry does not match",
			list: "race:./p",
			tier: TierIntegration,
			pkg:  "./p",
		},
		{
			name: "another package's entry does not match",
			list: "integration:./q",
			tier: TierIntegration,
			pkg:  "./p",
		},
		{
			name: "a package whose name extends this one does not match",
			list: "integration:./cmd2",
			tier: TierIntegration,
			pkg:  "./cmd",
		},
		{
			name:      "a nested module's package",
			list:      "integration:./tracker/store",
			tier:      TierIntegration,
			pkg:       "./tracker/store",
			wantWhole: true,
		},
		{
			name:        "a nested module's package with a regexp",
			list:        "integration:./tracker/store:TestPostgres",
			tier:        TierIntegration,
			pkg:         "./tracker/store",
			wantPattern: "TestPostgres",
		},
		{
			name:        "entries split on any whitespace",
			list:        "race:./a\nintegration:./b\t\n  integration:./c:^TestC$",
			tier:        TierIntegration,
			pkg:         "./c",
			wantPattern: "^TestC$",
		},
		{
			name:      "another tier's entry before this one's is skipped",
			list:      "race:./p integration:./p",
			tier:      TierIntegration,
			pkg:       "./p",
			wantWhole: true,
		},
		{
			name: "a trailing colon names no pattern and skips nothing",
			list: "integration:./p:",
			tier: TierIntegration,
			pkg:  "./p",
		},
		{
			name:        "a trailing colon does not shadow a later entry (gt-jtyrs)",
			list:        "integration:./p: integration:./p:^TestA$",
			tier:        TierIntegration,
			pkg:         "./p",
			wantPattern: "^TestA$",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			whole, pattern := SkipFor(tc.list, tc.tier, tc.pkg)
			if whole != tc.wantWhole || pattern != tc.wantPattern {
				t.Errorf("SkipFor(%q, %q, %q) = (%t, %q), want (%t, %q)",
					tc.list, tc.tier, tc.pkg, whole, pattern, tc.wantWhole, tc.wantPattern)
			}
		})
	}
}
