package testpolicy

import (
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
