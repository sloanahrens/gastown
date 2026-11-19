package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// gt-nsbeg9's gt pool show half: the page names the running sandboxes the count
// does not hold and the counted seats with none, in the same notes the tick line
// appends.

// poolReconcileShowTown is a town with one seat and a rig registry, and the
// seats a test reconciles. The dead-hooked seat names no sandbox of the
// gateway's, so it is the counted seat with none.
func poolReconcileShowTown(t *testing.T) (string, []poolSession) {
	t.Helper()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"agents":{"deepseek-flash":{"command":"claude"}},`+
		`"polecat_pool":{"seats":{"deepseek-flash":{"cap":3}}}}`, "")
	writeTestRigsConfig(t, root, "gastown")
	return root, []poolSession{
		{name: "gt-capable", agent: "deepseek-flash", sandbox: "gt-capable", rig: "gastown", polecat: "capable"},
		{name: "dead/gastown/ember", agent: "deepseek-flash", sandbox: "gt-ember", rig: "gastown", polecat: "ember", deadHooked: true},
	}
}

// poolReconcileShowReads is a fake read set whose every seat is reported with
// one disposition word, so a page's formatting is what the test measures.
func poolReconcileShowReads(running func() ([]string, error)) poolReconcileReads {
	return poolReconcileReads{
		Running: running,
		SeatOf: func(sandbox string) (string, string, bool) {
			return "gastown", strings.TrimPrefix(sandbox, "gt-"), true
		},
		Dispose: func(rig, name string) (polecat.WorkstateDisposition, error) {
			return polecat.WorkstateDisposition{ReuseStatus: "idle-pr-open"}, nil
		},
	}
}

// TestPoolShowNamesUncountedSandboxesAndSeatsWithNone is the page's half: a
// stray running sandbox and a counted seat with none print under the total,
// through the same seat formatter, each with its disposition.
func TestPoolShowNamesUncountedSandboxesAndSeatsWithNone(t *testing.T) {
	t.Parallel()
	root, sessions := poolReconcileShowTown(t)
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	live := poolLiveForWithReconcile(root,
		func(*config.PolecatPool) ([]poolSession, error) { return sessions, nil },
		poolReconcileShowReads(func() ([]string, error) { return []string{"gt-capable", "gt-stray"}, nil }))

	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, poolTestNow, live); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	for _, want := range []string{
		"  uncounted sandboxes: gt-stray:idle-pr-open\n",
		"  counted without a sandbox: gt-ember:idle-pr-open\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not print %q:\n%s", want, page)
		}
	}
}

// TestPoolShowNotesAnUnreadableSandboxListing is the page's fail-closed half: a
// listing that cannot be read says so, and never reads as no uncounted sandbox.
func TestPoolShowNotesAnUnreadableSandboxListing(t *testing.T) {
	t.Parallel()
	root, sessions := poolReconcileShowTown(t)
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	live := poolLiveForWithReconcile(root,
		func(*config.PolecatPool) ([]poolSession, error) { return sessions, nil },
		poolReconcileShowReads(func() ([]string, error) { return nil, errors.New("gateway down") }))

	var out bytes.Buffer
	if err := runPoolSeatMapShow(&out, root, ts, poolTestNow, live); err != nil {
		t.Fatal(err)
	}
	page := out.String()
	if !strings.Contains(page, "  sandbox listing unavailable\n") {
		t.Fatalf("page does not print the unavailable note:\n%s", page)
	}
	if strings.Contains(page, "uncounted sandboxes") || strings.Contains(page, "counted without a sandbox") {
		t.Fatalf("an unreadable listing printed a seat list:\n%s", page)
	}
}

// TestPoolShowCleanReconcileKeepsItsPage is the byte-identical half: when every
// counted seat has the sandbox the gateway runs, the page is exactly the page
// without the reconciliation.
func TestPoolShowCleanReconcileKeepsItsPage(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, `{"type":"town-settings","version":1,"agents":{"deepseek-flash":{"command":"claude"}},`+
		`"polecat_pool":{"seats":{"deepseek-flash":{"cap":3}}}}`, "")
	writeTestRigsConfig(t, root, "gastown")
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	read := func(*config.PolecatPool) ([]poolSession, error) {
		return []poolSession{
			{name: "gt-capable", agent: "deepseek-flash", sandbox: "gt-capable", rig: "gastown", polecat: "capable"},
		}, nil
	}
	show := func(live poolShowLive) string {
		t.Helper()
		var out bytes.Buffer
		if err := runPoolSeatMapShow(&out, root, ts, poolTestNow, live); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	// The gateway runs exactly the sandbox the counted seat has, so the clean
	// reading has no note to print.
	clean := show(poolLiveForWithReconcile(root, read,
		poolReconcileShowReads(func() ([]string, error) { return []string{"gt-capable"}, nil })))
	without := show(poolLiveForWith(root, read))

	if clean != without {
		t.Fatalf("a clean reconciliation changed the page:\n%s\nwant:\n%s", clean, without)
	}
}
