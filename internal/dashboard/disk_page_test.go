package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Disk block is read where an operator reads the machine: inside the
// Machine section, under the machine body, the way Deploys sits under the
// Cloud body. No new section, so the column layout is the one the page had.
func TestDiskBlockSitsInsideTheMachineSectionAfterTheBody(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	body := strings.Index(page, `id="machine"`)
	head := strings.Index(page, `id="disknote"`)
	disk := strings.Index(page, `id="disk"`)
	// The Cloud section is the next one over; nothing of the block may spill
	// past it.
	after := strings.Index(page, `<section data-fold="cloud"><h2>Cloud`)
	if body < 0 || head < 0 || disk < 0 || after < 0 {
		t.Fatalf("the Machine section is missing one of #machine, #disknote, #disk or Cloud (body=%d head=%d disk=%d cloud=%d)",
			body, head, disk, after)
	}
	if !(body < head && head < disk) {
		t.Errorf("the block is not under the machine body in the order #machine, #disknote, #disk (%d, %d, %d)", body, head, disk)
	}
	if disk > after {
		t.Error("the block is not inside the Machine section")
	}

	// The section is one element of the same shape as its neighbours, with the
	// block's own heading and body inside it.
	if !strings.Contains(page, `<section data-fold="machine"><h2>Machine <span class="r" id="machnote"></span></h2><div class="body" id="machine"></div><h3 class="sub">Disk <span class="r" id="disknote"></span></h3><div class="body" id="disk"></div></section>`) {
		t.Error("the Machine section is not the markup its neighbours are")
	}
}

// The block's four states are worded on the page and none of them is a blank:
// a push skips the pane when its reading has not moved, a snapshot with no disk
// reading says it is still reading, and a row says which of the two ways it has
// no share — a failed read is never drawn as an empty disk.
func TestDiskBlockDrawsEveryStateItCanRead(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderDisk")
	if !strings.Contains(draw, "function renderDisk(s) {\n  if (pushSkip(\"disk\", s)) return;") {
		t.Error("renderDisk does not start with the pushSkip call that leaves an unmoved pane where it is")
	}
	for _, want := range []string{
		`box.append(el("div", "empty", "reading the disk samples…"))`,
		`if (r.err) { box.append(el("div", "empty", r.name + " disk: could not read its samples")); continue; }`,
		`if (r.no_samples) { box.append(el("div", "empty", r.name + " disk: no samples yet")); continue; }`,
		`r.name + " disk " + r.used_pct + "% used (" + diskGiB(r.used_bytes || 0) + " of " + diskGiB(r.total_bytes) + " GiB), " + diskGrowthText(r)`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("renderDisk has no %q", want)
		}
	}
}

// A disk the reader got a sample for draws the share the reader rounded and the
// bytes it came from. This runs the page's own function over the reading the
// server would send: 410 of 500 GiB is 82%, and the byte counts are drawn as
// GiB to one decimal.
func TestDiskBlockDrawsTheShareLine(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{
		"rows": []any{map[string]any{
			"name": "control", "used_bytes": int64(410) << 30, "total_bytes": int64(500) << 30, "used_pct": 82,
		}},
	}})

	line := dom.descend(t, "disk", 0)
	// The payload carries no growth flags, so the line words the growth as the
	// fit it has not got rather than inventing one beside the share.
	if want := "control disk 82% used (410.0 of 500.0 GiB), growth: not enough samples yet"; line.Text != want {
		t.Errorf("the row draws %q, want %q", line.Text, want)
	}
	if line.Class != "" {
		t.Errorf("a share under 90 wears %q, want the line's own plain class", line.Class)
	}
}

// A share at or past 90 is the hot share the patrol raises on, so the line
// takes the page's warning colour; the row below the threshold stays plain.
func TestDiskBlockColoursAHotShare(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{
		"rows": []any{
			map[string]any{"name": "sandbox", "used_bytes": int64(450) << 30, "total_bytes": int64(500) << 30, "used_pct": 90},
			map[string]any{"name": "control", "used_bytes": int64(445) << 30, "total_bytes": int64(500) << 30, "used_pct": 89},
		},
	}})

	if got := dom.descend(t, "disk", 0).Class; got != "warnc" {
		t.Errorf("a share of 90 wears %q, want the warning colour", got)
	}
	if got := dom.descend(t, "disk", 1).Class; got != "" {
		t.Errorf("a share of 89 wears %q, want the line's own plain class", got)
	}
}

// A ring with no sample yet is a disk with nothing to draw, and the row says
// that rather than a zero share.
func TestDiskBlockDrawsARowWithNoSamples(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{
		"rows": []any{map[string]any{"name": "sandbox", "used_pct": 0, "no_samples": true}},
	}})

	if got := dom.descend(t, "disk", 0).Text; got != "sandbox disk: no samples yet" {
		t.Errorf("the row draws %q, want the ring said to hold no sample yet", got)
	}
}

// A ring the reader could not read is never drawn as an empty disk: the row
// says the read failed.
func TestDiskBlockDrawsARowItCouldNotRead(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{
		"rows": []any{map[string]any{"name": "control", "used_pct": 0, "err": "permission denied"}},
	}})

	if got := dom.descend(t, "disk", 0).Text; got != "control disk: could not read its samples" {
		t.Errorf("the row draws %q, want the failed read said out loud", got)
	}
}

// A snapshot that carries no disk reading at all is not a clear disk: the block
// says it is still reading rather than holding a blank.
func TestDiskBlockDrawsTheReadingLineWithNoDisk(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{})
	box := dom.element(t, "disk")
	if box.Kids != 1 {
		t.Fatalf("the block drew %d lines with no disk reading, want one", box.Kids)
	}
	if got := dom.descend(t, "disk", 0).Text; got != "reading the disk samples…" {
		t.Errorf("the block draws %q with no disk reading, want the reading said out loud", got)
	}
}

// The pane is registered beside Deploys with the reading it draws from, so a
// push that moves s.disk redraws it, and opening the Machine section draws it
// through the same table every other pane hangs from.
func TestDiskBlockRegistersItsPaneBesideDeploys(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	entry := `disk: {sec: "machine", body: ["disk"], read: s => s.disk, draw: s => renderDisk(s)},`
	if !strings.Contains(page, entry) {
		t.Errorf("index.html's PANES has no %s", entry)
	}
	if strings.Index(page, entry) < strings.Index(page, `deploys: {sec: "cloud"`) {
		t.Error("the Disk pane is not registered beside Deploys")
	}
}

// The block is drawn wherever Deploys is, the full redraw and the 15 s tick
// included: a pane left out of either freezes while the rest of the page moves.
func TestDiskBlockIsDrawnWhereverDeploysIs(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	calls := strings.Count(page, "renderDeploys(state)")
	if calls < 2 {
		t.Fatalf("the page calls renderDeploys(state) %d times, want at least the full redraw and the tick", calls)
	}
	if got := strings.Count(page, "renderDisk(state)"); got != calls {
		t.Errorf("the page draws the Disk block at %d of the %d places it draws Deploys", got, calls)
	}
	if !strings.Contains(pageFunc(t, "renderAll"), "renderDisk(state)") {
		t.Error("the page's full redraw does not draw the Disk block")
	}
	tick := page[strings.Index(page, "setInterval("):]
	if !strings.Contains(tick, "renderDisk(state)") {
		t.Error("the 15 s tick does not redraw the Disk block")
	}
}

// diskRow builds the row the server would send for name, with the growth the
// reader fitted beside the share.
func diskRow(name string, used, total int64, pct int, growth map[string]any) map[string]any {
	row := map[string]any{"name": name, "used_bytes": used, "total_bytes": total, "used_pct": pct}
	for k, v := range growth {
		row[k] = v
	}
	return row
}

// The line a reader makes of a steady ring is golden end to end: the ring grows
// a GiB an hour all week, which is 24 GiB a day in both windows, and 268 of 500
// GiB used at 24 a day leaves 9. The row reaches the page through the payload's
// JSON, the way the hub hands it over.
func TestDiskGrowthLineOfASteadyRing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	diskHourlyRing(t, dir, "control", func(k int) int64 { return (int64(100) + int64(k)) << 30 })
	raw, err := json.Marshal(ReadDisk(dir, diskNow))
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}

	dom := newPageDOM(t)
	dom.call(t, "renderDisk", map[string]any{"disk": disk})
	want := "control disk 54% used (268.0 of 500.0 GiB), +24.0 GiB/day (24h), +24.0 GiB/day (7d), about 9 days left"
	if got := dom.descend(t, "disk", 0).Text; got != want {
		t.Errorf("the row draws %q, want %q", got, want)
	}
}

// The growth the reader fitted is the line's tail, goldened over every state
// the reader can send: a steady ring shows both rates and the days left, a ring
// with too few samples says so, a missing ring has no share to grow, a flat
// disk says it has no growth, a disk more than 90 days away stops counting, and
// a window that lost space is drawn with its own sign.
func TestDiskGrowthLineDrawsEveryState(t *testing.T) {
	t.Parallel()

	const gib = int64(1) << 30
	cases := []struct {
		name string
		row  map[string]any
		want string
	}{
		{
			name: "steady growth",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": 3.0, "gib_per_day_7d": 2.9, "days_left": 31.03,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), +3.0 GiB/day (24h), +2.9 GiB/day (7d), about 31 days left",
		},
		{
			name: "too few samples",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"growth_ok": false, "days_left_ok": false,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), growth: not enough samples yet",
		},
		{
			name: "missing ring",
			row:  map[string]any{"name": "control", "used_pct": 0, "no_samples": true},
			want: "control disk: no samples yet",
		},
		{
			name: "flat disk",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": 0.0, "gib_per_day_7d": 0.0, "days_left": 0.0,
				"growth_ok": true, "days_left_ok": false,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), no growth",
		},
		{
			name: "over ninety days",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": 1.2, "gib_per_day_7d": 1.0, "days_left": 120.5,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), +1.2 GiB/day (24h), +1.0 GiB/day (7d), about 90+ days left",
		},
		{
			name: "exactly ninety days",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": 1.2, "gib_per_day_7d": 1.0, "days_left": 90.0,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), +1.2 GiB/day (24h), +1.0 GiB/day (7d), about 90 days left",
		},
		{
			name: "a day short of ten",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": 1.2, "gib_per_day_7d": 1.0, "days_left": 9.8,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), +1.2 GiB/day (24h), +1.0 GiB/day (7d), about 9 days left",
		},
		{
			name: "the day lost space",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": -0.4, "gib_per_day_7d": 1.5, "days_left": 40.0,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), -0.4 GiB/day (24h), +1.5 GiB/day (7d), about 40 days left",
		},
		{
			name: "a day that rounds to zero",
			row: diskRow("control", 410*gib, 500*gib, 82, map[string]any{
				"gib_per_day_24h": -0.04, "gib_per_day_7d": 1.5, "days_left": 40.0,
				"growth_ok": true, "days_left_ok": true,
			}),
			want: "control disk 82% used (410.0 of 500.0 GiB), +0.0 GiB/day (24h), +1.5 GiB/day (7d), about 40 days left",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{"rows": []any{tc.row}}})
			if got := dom.descend(t, "disk", 0).Text; got != tc.want {
				t.Errorf("the row draws %q, want %q", got, tc.want)
			}
		})
	}
}

// A disk with under a week left is warned on its days left even below the hot
// share, and one with a week or more is not; a row with no projection has no
// days left to warn on, so its zero value must stay plain rather than read as
// "0 days left".
func TestDiskGrowthWarnsUnderSevenDaysLeft(t *testing.T) {
	t.Parallel()

	const gib = int64(1) << 30
	cases := []struct {
		name    string
		growth  map[string]any
		wantCls string
	}{
		{
			name:    "under seven days",
			growth:  map[string]any{"gib_per_day_24h": 3.0, "gib_per_day_7d": 2.9, "days_left": 6.9, "growth_ok": true, "days_left_ok": true},
			wantCls: "warnc",
		},
		{
			name:    "exactly seven days",
			growth:  map[string]any{"gib_per_day_24h": 3.0, "gib_per_day_7d": 2.9, "days_left": 7.0, "growth_ok": true, "days_left_ok": true},
			wantCls: "",
		},
		{
			name:    "longer than seven days",
			growth:  map[string]any{"gib_per_day_24h": 3.0, "gib_per_day_7d": 2.9, "days_left": 40.0, "growth_ok": true, "days_left_ok": true},
			wantCls: "",
		},
		{
			name:    "no projection",
			growth:  map[string]any{"gib_per_day_24h": 0.0, "gib_per_day_7d": 0.0, "days_left": 0.0, "growth_ok": true, "days_left_ok": false},
			wantCls: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dom := newPageDOM(t)
			dom.call(t, "renderDisk", map[string]any{"disk": map[string]any{"rows": []any{
				diskRow("control", 410*gib, 500*gib, 82, tc.growth),
			}}})
			if got := dom.descend(t, "disk", 0).Class; got != tc.wantCls {
				t.Errorf("the row wears %q, want %q", got, tc.wantCls)
			}
		})
	}
}
