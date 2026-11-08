package cloudcheck

import (
	"strings"
	"testing"
)

func TestPrintShowsEveryLevelAndQuietShowsOnlyProblems(t *testing.T) {
	t.Parallel()
	r := Report{
		OK: 1, Warnings: 1, Failed: 1, Off: 1,
		Rows: []Row{
			{Level: LevelOK, Name: "web", Detail: "HTTP 200"},
			{Level: LevelWarn, Name: "docs", Detail: "HTTP 404"},
			{Level: LevelFail, Name: "api", Detail: "HTTP 500"},
			{Level: LevelOff, Section: "legacy", Name: "site", Detail: "off on purpose"},
		},
	}

	var full strings.Builder
	r.Print(&full, false)
	for _, want := range []string{"web", "docs", "api", "legacy/site", "1 ok, 1 warnings, 1 failed, 1 off"} {
		if !strings.Contains(full.String(), want) {
			t.Errorf("full report does not show %q:\n%s", want, full.String())
		}
	}

	var quiet strings.Builder
	r.Print(&quiet, true)
	for _, want := range []string{"docs", "api"} {
		if !strings.Contains(quiet.String(), want) {
			t.Errorf("quiet report does not show %q:\n%s", want, quiet.String())
		}
	}
	for _, unwanted := range []string{"web", "legacy/site", "1 ok,"} {
		if strings.Contains(quiet.String(), unwanted) {
			t.Errorf("quiet report shows %q:\n%s", unwanted, quiet.String())
		}
	}
}
