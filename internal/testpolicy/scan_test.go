package testpolicy

import (
	"go/build/constraint"
	"path/filepath"
	"sort"
	"testing"
)

func TestScanDirFixtures(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{ // fixture dir -> sorted expected rules
		"clean":                 nil,
		"sleep":                 {RuleNoSleep},
		"env":                   {RuleNoEnv, RuleNoEnv},               // os.Setenv and t.Setenv
		"chdir":                 {RuleNoChdir, RuleNoChdir},           // os.Chdir and t.Chdir
		"skip":                  {RuleNoSkip, RuleNoSkip, RuleNoSkip}, // Skip, Skipf, SkipNow
		"exec_other":            {RuleNoSubprocess},
		"exec_go":               {RuleNoBuild},
		"exec_git":              nil,
		"exec_file":             {RuleNoExecFiles},
		"shebang":               {RuleNoExecFiles},
		"global_swap":           {RuleNoGlobalSwap},
		"global_shadow":         nil,
		"parallel_missing":      {RuleParallel},
		"parallel_subtest_only": {RuleParallel},
		"nontesting_receiver":   nil,
		"allow_ok":              nil,
		"allow_noreason":        {RuleAllowReason},
		"prod_setenv":           {RuleProdSetenv},
		"prod_sleep_clock":      {RuleProdSleep},
		"integration_skipped":   nil,
	}
	for dir, want := range cases {
		dir, want := dir, want
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			vs, err := ScanDir(filepath.Join("testdata", dir))
			if err != nil {
				t.Fatalf("ScanDir: %v", err)
			}
			var got []string
			for _, v := range vs {
				got = append(got, v.Rule)
			}
			sort.Strings(got)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("rules = %v, want %v\n%v", got, want, vs)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("rules = %v, want %v\n%v", got, want, vs)
				}
			}
		})
	}
}

func TestCheckContracts(t *testing.T) {
	t.Parallel()
	dirs := []string{
		filepath.Join("testdata", "contracts", "goodfake"),
		filepath.Join("testdata", "contracts", "real"),
		filepath.Join("testdata", "contracts", "badfake"),
	}
	vs, err := CheckContracts("testdata/contracts", dirs)
	if err != nil {
		t.Fatalf("CheckContracts: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("violations = %v, want exactly one", vs)
	}
	v := vs[0]
	if v.Rule != RuleContract {
		t.Fatalf("rule = %q, want %q", v.Rule, RuleContract)
	}
	if v.Pos.Filename != filepath.Join("testdata", "contracts", "badfake") {
		t.Fatalf("violation filename = %q, want badfake dir", v.Pos.Filename)
	}
}

func TestRequiresIntegration(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"integration":             true,
		"!integration":            false,
		"linux || integration":    false,
		"linux && integration":    true,
		"!windows":                false,
		"integration && !windows": true,
	}
	for src, want := range cases {
		src, want := src, want
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			expr, err := constraint.Parse("//go:build " + src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", src, err)
			}
			if got := requiresIntegration(expr); got != want {
				t.Errorf("requiresIntegration(%q) = %v, want %v", src, got, want)
			}
		})
	}
}
