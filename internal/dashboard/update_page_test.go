package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The header's update chip draws the feed's update section: the gt update job
// while it runs and what a run left behind, so the operator can read at a
// glance that an update is in flight and why nothing is being dispatched.

// updateState wraps one update section the way the feed sends it; no section
// at all is the state a town that never ran an update sends.
func updateState(u map[string]any) map[string]any {
	if u == nil {
		return map[string]any{}
	}
	return map[string]any{"update": u}
}

// updateStep is one step of a run's plan as the feed hands it over. A step
// with no detail leaves the key out, as the reader does.
func updateStep(name, result, detail string) map[string]any {
	st := map[string]any{"name": name, "result": result}
	if detail != "" {
		st["detail"] = detail
	}
	return st
}

// updateRunning is a running run at one step, with the steps the tooltip
// draws.
func updateRunning(current string, elapsed, heartbeat int) map[string]any {
	return map[string]any{
		"running": true, "run_id": "run-7", "source": "daemon",
		"current": current, "outcome": "running",
		"elapsed_sec": elapsed, "heartbeat_age_sec": heartbeat,
		"steps": []any{
			updateStep("drain", "ok", "2 polecats left"),
			updateStep("install", "running", ""),
		},
	}
}

// No update section, and an update that ended ok, both leave the header with
// no chip: there is nothing the operator has to read. The ok draw follows a
// running one, so the chip must clear what the earlier draw left as well as
// hide itself.
func TestUpdateChipHiddenWithoutAnUpdate(t *testing.T) {
	t.Parallel()

	t.Run("the feed carries no update section", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderUpdate", updateState(nil))
		if chip := dom.element(t, "update"); !chip.Hidden {
			t.Errorf("the chip is drawn with no update section (text %q), want it hidden", chip.Text)
		}
	})

	t.Run("the last run ended ok", func(t *testing.T) {
		t.Parallel()

		dom := newPageDOM(t)
		dom.call(t, "renderUpdate", updateState(updateRunning("install", 480, 12)))
		if chip := dom.element(t, "update"); chip.Hidden {
			t.Fatal("the running update drew no chip, so this case would not prove the ok draw hides one")
		}
		dom.call(t, "renderUpdate", updateState(map[string]any{
			"running": false, "outcome": "ok", "run_id": "run-7", "source": "daemon", "elapsed_sec": 600,
		}))
		chip := dom.element(t, "update")
		if !chip.Hidden {
			t.Errorf("an ok run draws the chip (text %q), want it hidden", chip.Text)
		}
		if chip.Text != "" || chip.Title != "" {
			t.Errorf("the hidden chip keeps %q / %q from the running draw, want it cleared", chip.Text, chip.Title)
		}
	})
}

// A running update is amber, names its step and how long it has run, and puts
// the run id and one line per step in its tooltip. A run that has not named
// its step yet reads "starting" rather than ending in a bare duration.
func TestUpdateChipShowsARunningUpdate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		update map[string]any
		text   string
		title  []string
	}{
		{
			name:   "installing after eight minutes",
			update: updateRunning("install", 480, 12),
			text:   "update: install 8m",
			title:  []string{"daemon", "drain: ok - 2 polecats left", "install: running"},
		},
		{
			name: "a run that has not named its step yet",
			update: map[string]any{
				"running": true, "run_id": "run-8", "source": "daemon",
				"current": "", "outcome": "running", "elapsed_sec": 5, "heartbeat_age_sec": 1,
			},
			text:  "update: starting 5s",
			title: []string{"daemon"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderUpdate", updateState(tc.update))
			chip := dom.element(t, "update")
			if chip.Hidden {
				t.Fatalf("a running update draws no chip, so the operator cannot see the job is in flight")
			}
			if chip.Class != "warn" {
				t.Errorf("the chip wears %q, want the amber warn class", chip.Class)
			}
			if chip.Text != tc.text {
				t.Errorf("the chip reads %q, want %q", chip.Text, tc.text)
			}
			if !strings.HasPrefix(chip.Title, tc.update["run_id"].(string)) {
				t.Errorf("the chip's title reads %q, want it to start with the run id %q", chip.Title, tc.update["run_id"])
			}
			for _, want := range tc.title {
				if !strings.Contains(chip.Title, want) {
					t.Errorf("the chip's title reads %q, with no %q", chip.Title, want)
				}
			}
		})
	}
}

// A running run whose heartbeat is stale reads "update stalled" with the time
// since the last write, because its record says what it was doing, not what it
// is doing. At the three-minute mark it is still current: the bound is above
// 180 seconds, not at it.
func TestUpdateChipShowsAStalledHeartbeat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		update    map[string]any
		text      string
		titleLead string
	}{
		{
			name:      "four minutes without a heartbeat",
			update:    updateRunning("install", 600, 240),
			text:      "update stalled",
			titleLead: "no heartbeat for 4m",
		},
		{
			name:   "a heartbeat exactly at the three-minute limit is still current",
			update: updateRunning("install", 300, 180),
			text:   "update: install 5m",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderUpdate", updateState(tc.update))
			chip := dom.element(t, "update")
			if chip.Hidden {
				t.Fatal("a running update draws no chip, so the operator cannot see the job is in flight")
			}
			if chip.Class != "warn" {
				t.Errorf("the chip wears %q, want the amber warn class", chip.Class)
			}
			if chip.Text != tc.text {
				t.Errorf("the chip reads %q, want %q", chip.Text, tc.text)
			}
			if tc.titleLead == "" {
				return
			}
			if !strings.HasPrefix(chip.Title, tc.titleLead) {
				t.Errorf("the chip's title reads %q, want it to start with %q", chip.Title, tc.titleLead)
			}
			if !strings.Contains(chip.Title, "install: running") {
				t.Errorf("the chip's title reads %q, with no step line under the stalled heartbeat", chip.Title)
			}
		})
	}
}

// A finished run that did not end cleanly is amber and names its outcome; one
// the operator stopped is quiet. An outcome the page does not know is drawn
// rather than hidden: hiding it would report an unknown state as an ok run.
func TestUpdateChipShowsABadFinishedRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		outcome string
		text    string
		class   string
	}{
		{"failed", "failed", "update: failed", "warn"},
		{"partial", "partial", "update: partial", "warn"},
		{"aborted", "aborted", "update: aborted", "warn"},
		{"interrupted", "interrupted", "update: interrupted", "quiet"},
		{"an outcome this page does not know", "vanished", "update: vanished", "warn"},
		{"an outcome the feed left empty", "", "update: unknown", "warn"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderUpdate", updateState(map[string]any{
				"running": false, "outcome": tc.outcome, "run_id": "run-9", "source": "daemon",
				"elapsed_sec": 300,
				"steps":       []any{updateStep("install", "failed", "the install marker stayed")},
			}))
			chip := dom.element(t, "update")
			if chip.Hidden {
				t.Fatalf("a finished %q run draws no chip, so the operator reads it as clean", tc.outcome)
			}
			if chip.Class != tc.class {
				t.Errorf("the chip wears %q, want %q", chip.Class, tc.class)
			}
			if chip.Text != tc.text {
				t.Errorf("the chip reads %q, want %q", chip.Text, tc.text)
			}
			if !strings.HasPrefix(chip.Title, "run-9") {
				t.Errorf("the chip's title reads %q, want it to start with the run id", chip.Title)
			}
			if !strings.Contains(chip.Title, "install: failed - the install marker stayed") {
				t.Errorf("the chip's title reads %q, with no step's result and detail", chip.Title)
			}
		})
	}
}

// The chip is the page's own: a state delivered on its stream draws it, the
// section table carries the entry a push's guard reads, and the markup places
// it in the header right after the hold chip.
func TestUpdateChipIsRegisteredAndPlacedBesideTheHoldChip(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	pushState(t, dom, updateState(map[string]any{
		"running": false, "outcome": "failed", "run_id": "run-9", "source": "daemon",
	}))
	if chip := dom.element(t, "update"); chip.Hidden {
		t.Error("a push carrying an update section left the chip hidden, so the page does not draw it")
	}

	// The table's own draw, the entry a section open would call.
	raw, err := json.Marshal(updateState(updateRunning("drain", 60, 1)))
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	if _, err := dom.vm.RunString("PANES.update.draw(" + string(raw) + ")"); err != nil {
		t.Fatalf("the page's section table has no update draw: %v", err)
	}
	if chip := dom.element(t, "update"); chip.Hidden || !strings.Contains(chip.Text, "drain") {
		t.Errorf("the section table's update draw reads %q (hidden=%v), want the running update", chip.Text, chip.Hidden)
	}

	page := string(indexHTML)
	if n := strings.Count(page, `id="update"`); n != 1 {
		t.Errorf("the page has %d update chips, want 1", n)
	}
	header := page[strings.Index(page, "<header>"):strings.Index(page, "</header>")]
	hold, chip := strings.Index(header, `id="hold"`), strings.Index(header, `id="update"`)
	if hold < 0 || chip < 0 {
		t.Fatal("the header carries no hold chip or no update chip")
	}
	if chip < hold {
		t.Error("the update chip sits before the hold chip it belongs after")
	}
	if all := pageFunc(t, "renderAll"); !strings.Contains(all, "renderUpdate(state)") {
		t.Error("the page's redraw does not draw the update chip, so it never moves")
	}
}
