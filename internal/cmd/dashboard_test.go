package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/dashboard"
)

func TestParseSeatPair(t *testing.T) {
	t.Parallel()
	got := parseSeatPair("gastown/agate:gt-4k3fj.14")
	want := dashboard.SeatRef{Rig: "gastown", Polecat: "agate", Bead: "gt-4k3fj.14"}
	if got != want {
		t.Errorf("parseSeatPair = %+v, want %+v", got, want)
	}
	if parseSeatPair("no-colon") != (dashboard.SeatRef{}) {
		t.Error("a pair with no bead must parse to the zero ref")
	}
}

func TestDashboardClassName(t *testing.T) {
	t.Parallel()
	for c, want := range map[tailClass]string{
		tailClassFailure: "failure", tailClassWarning: "warning", tailClassSuccess: "success",
		tailClassLanding: "landing", tailClassDispatch: "dispatch", tailClassRestart: "restart", tailClassPlain: "plain",
	} {
		if got := dashboardClassName(c); got != want {
			t.Errorf("class %d = %q, want %q", c, got, want)
		}
	}
}

func TestResolveSpendCmdPrefersFlag(t *testing.T) {
	t.Parallel()
	if got := resolveSpendCmd("/bin/echo {}"); len(got) != 2 || got[0] != "/bin/echo" {
		t.Errorf("flag not honored: %v", got)
	}
}

func TestDashboardEntryDropsClockAndKeepsTitle(t *testing.T) {
	t.Parallel()
	v := tailView{Trim: true}
	e := dashboardEntry(v, tailLine{Rig: "gastown", Kind: tailKindDaemon, Text: "landed gt-x on main", Title: "Fix the thing"})
	if e.Text != "landed gt-x on main · Fix the thing" || e.Class != "success" || e.Rig != "gastown" {
		t.Errorf("entry = %+v", e)
	}
}
