package tiersweep

import (
	"bytes"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// intPtr is a niceness a sweep takes from TIER_SWEEP_NICE.
func intPtr(n int) *int { return &n }

// TestSlotPrefixNice: the argv a package runs under, at every value the A/B
// measurement needs. Unset is today's argv exactly, so the default is unchanged.
func TestSlotPrefixNice(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		nice *int
		want []string
	}{
		{
			name: "unset passes no --nice",
			want: []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1"},
		},
		{
			name: "0 is passed through",
			nice: intPtr(0),
			want: []string{"gt", "slot", "run", "--nice", "0", "--", "env", "GT_TEST_DOCKER=1"},
		},
		{
			name: "10 is passed through",
			nice: intPtr(10),
			want: []string{"gt", "slot", "run", "--nice", "10", "--", "env", "GT_TEST_DOCKER=1"},
		},
		{
			name: "19, the highest niceness, is passed through",
			nice: intPtr(19),
			want: []string{"gt", "slot", "run", "--nice", "19", "--", "env", "GT_TEST_DOCKER=1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := slotPrefix(tc.nice, false); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("slotPrefix(%v) = %q, want %q", tc.nice, got, tc.want)
			}
		})
	}
}

// TestParseOptionsNiceAccepts: TIER_SWEEP_NICE unset parses as no option at all,
// and each value the range allows parses as that number.
func TestParseOptionsNiceAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  *int
	}{
		{value: "", want: nil},
		{value: "0", want: intPtr(0)},
		{value: "10", want: intPtr(10)},
		{value: "19", want: intPtr(19)},
	}
	for _, tc := range cases {
		t.Run("value "+tc.value, func(t *testing.T) {
			t.Parallel()
			f := &fakeHost{env: map[string]string{EnvNice: tc.value}, tmpDir: madeDir}
			opts, err := ParseOptions(nil, f.host())
			if err != nil {
				t.Fatalf("ParseOptions(EnvNice=%q): %v", tc.value, err)
			}
			if !reflect.DeepEqual(opts.Nice, tc.want) {
				t.Errorf("Nice = %v, want %v", opts.Nice, tc.want)
			}
		})
	}
}

// TestParseOptionsNiceRefusals: a value outside 0 to 19, or one that is not a
// whole number, is a usage error that names the variable and the value, so a
// typo never reaches a package as a changed niceness.
func TestParseOptionsNiceRefusals(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"-1", "20", "abc", "1.5", "1 0"} {
		t.Run("value "+value, func(t *testing.T) {
			t.Parallel()
			f := &fakeHost{env: map[string]string{EnvNice: value}, tmpDir: madeDir}
			opts, err := ParseOptions(nil, f.host())
			if err == nil {
				t.Fatalf("ParseOptions(EnvNice=%q): no error, options %+v", value, opts)
			}
			if opts.Nice != nil {
				t.Errorf("ParseOptions returned Nice %v beside %v, want none", *opts.Nice, err)
			}
			for _, want := range []string{EnvNice, value} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to name %q", err, want)
				}
			}
			// The same refusal through the command: the usage exit, and no
			// log directory made on the way.
			var stderr bytes.Buffer
			code := Run(nil, io.Discard, &stderr, Deps{CgoEnv: testCgoEnv, Host: f.host()})
			if code != UsageExit {
				t.Errorf("Run exit = %d, want the usage exit %d", code, UsageExit)
			}
			if len(f.mkdirs) != 0 || len(f.temps) != 0 {
				t.Errorf("a refused value made directories: %v %v", f.temps, f.mkdirs)
			}
		})
	}
}

// tierSlotRuns runs the integration and race tiers over a repository with a
// test in every tier, with env as the host's environment, and returns the argv
// of each gt slot run they started.
func tierSlotRuns(t *testing.T, env map[string]string) [][]string {
	t.Helper()
	var runs [][]string
	for _, argv := range tierRuns(t, env) {
		if argv[0] == "gt" {
			runs = append(runs, argv)
		}
	}
	if len(runs) != 4 {
		t.Fatalf("gt slot run calls = %d, want 4 (root, docker, module, race): %q", len(runs), runs)
	}
	return runs
}

// tierRuns is every command the integration and race tiers started.
func tierRuns(t *testing.T, env map[string]string) [][]string {
	t.Helper()
	logDir := t.TempDir()
	env[EnvLogDir] = logDir
	host := &fakeHost{env: env, tmpDir: logDir}
	opts, err := ParseOptions(nil, host.host())
	if err != nil {
		t.Fatalf("ParseOptions: %v", err)
	}
	repo := baseRepo()
	repo.lists[". *_test.go"] = []string{"r/r_test.go", "d/d_test.go"}
	repo.files["r/r_test.go"] = taggedTest
	repo.files["d/d_test.go"] = plainTest
	repo.files[dockerList] = "d\n"
	repo.lists[". */go.mod"] = []string{"tracker/go.mod"}
	repo.lists["tracker *_test.go"] = []string{"store/pgtest/x_test.go"}
	repo.files["tracker/store/pgtest/x_test.go"] = taggedTest
	fake := &fakeTier{head: "abc123"}
	at := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	s := &Sweep{
		Opts:    opts,
		Exec:    fake.exec,
		Src:     repo,
		Environ: []string{"PATH=/fake/bin"},
		CgoEnv:  testCgoEnv,
		Stderr:  io.Discard,
		Now:     func() time.Time { return at },
	}
	s.Integration()
	s.Race()
	var runs [][]string
	for _, c := range fake.calls {
		runs = append(runs, c.argv)
	}
	return runs
}

// TestIntegrationAndRaceUseTheNiceOption: a parsed TIER_SWEEP_NICE reaches every
// gt slot run the tiers start, whichever package and directory it runs.
func TestIntegrationAndRaceUseTheNiceOption(t *testing.T) {
	t.Parallel()
	want := []string{"gt", "slot", "run", "--nice", "0", "--", "env", "GT_TEST_DOCKER=1"}
	for _, argv := range tierSlotRuns(t, map[string]string{EnvNice: "0"}) {
		if len(argv) < len(want) || !reflect.DeepEqual(argv[:len(want)], want) {
			t.Errorf("go test invocation %q does not begin with %q", argv, want)
		}
	}
}

// TestIntegrationAndRaceUnsetNiceKeepsTheDefaultArgv: with TIER_SWEEP_NICE unset,
// every gt slot run the tiers start has today's argv, with no --nice at all.
func TestIntegrationAndRaceUnsetNiceKeepsTheDefaultArgv(t *testing.T) {
	t.Parallel()
	want := []string{"gt", "slot", "run", "--", "env", "GT_TEST_DOCKER=1"}
	for _, argv := range tierSlotRuns(t, map[string]string{}) {
		if len(argv) < len(want) || !reflect.DeepEqual(argv[:len(want)], want) {
			t.Errorf("go test invocation %q does not begin with %q", argv, want)
		}
	}
}

// TestNoSlotDropsTheSlotWrapperFromEveryPackage: with TIER_SWEEP_NOSLOT=1 the
// four package commands (root, docker, module, race) start with the container
// environment and no gt slot run, and none of the sweep's commands is gt at all.
// The runner container has no town for gt slot run to find.
func TestNoSlotDropsTheSlotWrapperFromEveryPackage(t *testing.T) {
	t.Parallel()
	want := []string{"env", "GT_TEST_DOCKER=1"}
	packages := 0
	for _, argv := range tierRuns(t, map[string]string{EnvNoSlot: "1"}) {
		if slices.Contains(argv, "slot") {
			t.Errorf("command %q still runs gt slot run", argv)
		}
		if argv[0] == "env" {
			packages++
			if !reflect.DeepEqual(argv[:len(want)], want) {
				t.Errorf("package command %q does not begin with %q", argv, want)
			}
		}
	}
	if packages != 4 {
		t.Errorf("package commands = %d, want 4 (root, docker, module, race)", packages)
	}
}

// TestParseOptionsNoSlot: unset and 0 keep the wrapper, 1 drops it, and anything
// else is a usage error that names the variable and the value.
func TestParseOptionsNoSlot(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{"": false, "0": false, "1": true} {
		f := &fakeHost{env: map[string]string{EnvNoSlot: value}, tmpDir: madeDir}
		opts, err := ParseOptions(nil, f.host())
		if err != nil || opts.NoSlot != want {
			t.Errorf("ParseOptions(EnvNoSlot=%q) = NoSlot %v, %v; want %v", value, opts.NoSlot, err, want)
		}
	}
	f := &fakeHost{env: map[string]string{EnvNoSlot: "yes"}, tmpDir: madeDir}
	if _, err := ParseOptions(nil, f.host()); err == nil || !strings.Contains(err.Error(), EnvNoSlot) || !strings.Contains(err.Error(), "yes") {
		t.Errorf("ParseOptions(EnvNoSlot=yes) error = %v, want one naming the variable and the value", err)
	}
}
