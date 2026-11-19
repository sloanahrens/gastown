package update

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// pruneBase is the push time every test counts its tag ages from. The planner
// reads no clock, so the tests set every time.
var pruneBase = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// pruneTag is a tag pushed days after pruneBase.
func pruneTag(name, digest string, days int) PruneTag {
	return PruneTag{Name: name, Digest: digest, PushedAt: pruneBase.AddDate(0, 0, days)}
}

// pruneInstalled is a digest install's installed pair, with no digest a test
// tag reuses unless it means to.
var pruneInstalled = ImagePair{Daemon: "sha256:d-installed", Polecat: "sha256:p-installed", Bd: "sha256:b-installed"}

// wantPruneKeys fails the test unless the entries name exactly the wanted
// "image/tag" pairs, in any order.
func wantPruneKeys(t *testing.T, label string, got []PruneEntry, want ...string) {
	t.Helper()
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Image+"/"+e.Tag)
	}
	want = slices.Clone(want)
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("%s = %v, want %v", label, keys, want)
	}
}

// pruneReason returns the reason the plan gives for image/tag, failing the
// test when the tag is not in entries.
func pruneReason(t *testing.T, entries []PruneEntry, image, tag string) string {
	t.Helper()
	for _, e := range entries {
		if e.Image == image && e.Tag == tag {
			return e.Reason
		}
	}
	t.Fatalf("%s/%s is not in the plan", image, tag)
	return ""
}

func TestPruneKeepsInstalledAndPreviousPairs(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed: pruneInstalled,
		Previous:  ImagePair{Daemon: "sha256:d-previous", Polecat: "sha256:p-previous", Bd: "sha256:b-previous"},
		Images: map[string][]PruneTag{
			"gastown-daemon": {
				pruneTag("cur111111111", "sha256:d-installed", 3),
				pruneTag("alias2222222", "sha256:d-installed", 2),
				pruneTag("prev33333333", "sha256:d-previous", 1),
				pruneTag("stale4444444", "sha256:d-stale", 0),
			},
			"gastown-polecat": {
				pruneTag("pol555555555", "sha256:p-installed", 0),
				pruneTag("pol666666666", "sha256:p-previous", -1),
			},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep,
		"gastown-daemon/cur111111111", "gastown-daemon/alias2222222", "gastown-daemon/prev33333333",
		"gastown-polecat/pol555555555", "gastown-polecat/pol666666666")
	wantPruneKeys(t, "Delete", plan.Delete, "gastown-daemon/stale4444444")
	if got := pruneReason(t, plan.Keep, "gastown-daemon", "prev33333333"); got != "previous pair" {
		t.Errorf("previous-pair tag reason = %q, want %q", got, "previous pair")
	}
	if got := pruneReason(t, plan.Keep, "gastown-daemon", "alias2222222"); got != "installed pair" {
		t.Errorf("installed-pair alias reason = %q, want %q", got, "installed pair")
	}
}

func TestPruneKeepsTagsWithAGreenRef(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed:  pruneInstalled,
		Green:      map[string]bool{"ggggggggggggg": true},
		KeepNewest: 1,
		Images: map[string][]PruneTag{
			"gastown-daemon": {
				pruneTag("newer11111111", "sha256:d-new", 5),
				pruneTag("ggggggggggggg", "sha256:d-green", 1),
				pruneTag("oldxxxxxxxxx", "sha256:d-old", 0),
			},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep, "gastown-daemon/newer11111111", "gastown-daemon/ggggggggggggg")
	wantPruneKeys(t, "Delete", plan.Delete, "gastown-daemon/oldxxxxxxxxx")
	if got := pruneReason(t, plan.Keep, "gastown-daemon", "ggggggggggggg"); got != "green/ggggggggggggg ref" {
		t.Errorf("green tag reason = %q, want %q", got, "green/ggggggggggggg ref")
	}
	if got := pruneReason(t, plan.Delete, "gastown-daemon", "oldxxxxxxxxx"); !strings.Contains(got, "2026-11-01") {
		t.Errorf("delete reason = %q, want it to name the tag's push time", got)
	}
}

func TestPruneKeepsNewestNPerImage(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed:  pruneInstalled,
		KeepNewest: 2,
		Images: map[string][]PruneTag{
			"gastown-daemon": {
				pruneTag("d0000000000a", "sha256:d0", 0),
				pruneTag("d1111111111b", "sha256:d1", 1),
				pruneTag("d2222222222c", "sha256:d2", 2),
				pruneTag("d3333333333d", "sha256:d3", 3),
			},
			"gastown-polecat": {
				pruneTag("p0000000000a", "sha256:p0", 0),
				pruneTag("p1111111111b", "sha256:p1", 1),
				pruneTag("p2222222222c", "sha256:p2", 2),
			},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep,
		"gastown-daemon/d3333333333d", "gastown-daemon/d2222222222c",
		"gastown-polecat/p2222222222c", "gastown-polecat/p1111111111b")
	wantPruneKeys(t, "Delete", plan.Delete,
		"gastown-daemon/d1111111111b", "gastown-daemon/d0000000000a", "gastown-polecat/p0000000000a")
	for _, e := range plan.Delete {
		if e.Reason == "" {
			t.Errorf("delete candidate %s/%s has no reason", e.Image, e.Tag)
		}
	}
	// Each candidate's reason carries its own push time and rank, so two
	// candidates of one image never read the same.
	older := pruneReason(t, plan.Delete, "gastown-daemon", "d0000000000a")
	newer := pruneReason(t, plan.Delete, "gastown-daemon", "d1111111111b")
	if older == newer {
		t.Errorf("two delete candidates share the reason %q", older)
	}
	if !strings.Contains(newer, "2026-11-02") || !strings.Contains(newer, "rank 3 of 4") {
		t.Errorf("newer delete reason = %q, want its push time and rank", newer)
	}
	if !strings.Contains(older, "2026-11-01") || !strings.Contains(older, "rank 4 of 4") {
		t.Errorf("older delete reason = %q, want its push time and rank", older)
	}
}

func TestPruneNeverDeletesADigestSharedWithAKeptTag(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed:  pruneInstalled,
		KeepNewest: 1,
		Images: map[string][]PruneTag{
			"gastown-daemon": {
				pruneTag("dnew1111111a", "sha256:shared", 2),
				pruneTag("dold2222222b", "sha256:shared", 0),
				pruneTag("dgone333333c", "sha256:d-gone", -1),
			},
			"gastown-polecat": {
				pruneTag("pnew1111111a", "sha256:p-new", 3),
				pruneTag("pold2222222b", "sha256:shared", -2),
			},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep,
		"gastown-daemon/dnew1111111a", "gastown-daemon/dold2222222b",
		"gastown-polecat/pnew1111111a", "gastown-polecat/pold2222222b")
	wantPruneKeys(t, "Delete", plan.Delete, "gastown-daemon/dgone333333c")
	for _, e := range plan.Delete {
		if e.Digest == "sha256:shared" {
			t.Errorf("delete candidate %s/%s shares a kept digest", e.Image, e.Tag)
		}
	}
}

func TestPruneListsNoTagOfAnotherName(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed:  pruneInstalled,
		KeepNewest: 0,
		Images: map[string][]PruneTag{
			"gastown-daemon": {
				pruneTag("dinst0000000", "sha256:d-installed", 1),
				pruneTag("dstale1111111", "sha256:d-stale", 0),
			},
			"mango/engine": {pruneTag("engine222222", "sha256:engine", 0)},
			"mango/ui":     {pruneTag("ui3333333333", "sha256:ui", 0)},
			"mango":        {pruneTag("mango4444444", "sha256:mango", 0)},
			"devops/probe": {pruneTag("probe5555555", "sha256:probe", 0)},
			"gt.local/bd":  {{Name: "gobin6666666", Digest: "sha256:gobin"}},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune with other names: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep, "gastown-daemon/dinst0000000")
	wantPruneKeys(t, "Delete", plan.Delete, "gastown-daemon/dstale1111111")
}

func TestPruneKeepsBdTagsWhenTheInstalledPairNamesNoBd(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed:  ImagePair{Daemon: "sha256:d-installed", Polecat: "sha256:p-installed"},
		KeepNewest: 1,
		Images: map[string][]PruneTag{
			"gastown-daemon": {pruneTag("dnew1111111a", "sha256:d-new", 2)},
			"gastown-bd": {
				pruneTag("bnew1111111a", "sha256:b-new", 3),
				pruneTag("bold2222222b", "sha256:b-old", 0),
			},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune with no bd digest: %v", err)
	}
	wantPruneKeys(t, "Keep", plan.Keep,
		"gastown-daemon/dnew1111111a", "gastown-bd/bnew1111111a", "gastown-bd/bold2222222b")
	wantPruneKeys(t, "Delete", plan.Delete)
	for _, tag := range []string{"bnew1111111a", "bold2222222b"} {
		if got := pruneReason(t, plan.Keep, "gastown-bd", tag); !strings.Contains(got, "names no bd digest") {
			t.Errorf("bd tag %s reason = %q, want it to name the missing bd digest", tag, got)
		}
	}
}

func TestPruneFailsClosedWithAnEmptyDeleteList(t *testing.T) {
	t.Parallel()
	stale := pruneTag("dstale1111111", "sha256:d-stale", 0)
	tests := []struct {
		name string
		in   PruneInput
	}{
		{
			name: "tag with no push time",
			in: PruneInput{Installed: pruneInstalled, Images: map[string][]PruneTag{
				"gastown-daemon": {stale, {Name: "nopush222222", Digest: "sha256:d-x"}},
			}},
		},
		{
			name: "tag with no digest",
			in: PruneInput{Installed: pruneInstalled, Images: map[string][]PruneTag{
				"gastown-daemon": {stale, pruneTag("nodigest333333", "", 0)},
			}},
		},
		{
			name: "tag with no name",
			in: PruneInput{Installed: pruneInstalled, Images: map[string][]PruneTag{
				"gastown-daemon": {stale, pruneTag("", "sha256:d-y", 0)},
			}},
		},
		{
			name: "installed daemon digest empty",
			in: PruneInput{Installed: ImagePair{Polecat: "sha256:p", Bd: "sha256:b"}, Images: map[string][]PruneTag{
				"gastown-daemon": {stale},
			}},
		},
		{
			name: "installed polecat digest empty",
			in: PruneInput{Installed: ImagePair{Daemon: "sha256:d", Bd: "sha256:b"}, Images: map[string][]PruneTag{
				"gastown-daemon": {stale},
			}},
		},
		{
			name: "negative newest-tags count",
			in:   PruneInput{Installed: pruneInstalled, KeepNewest: -1, Images: map[string][]PruneTag{"gastown-daemon": {stale}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan, err := PlanPrune(tt.in)
			if !errors.Is(err, ErrPruneRefused) {
				t.Fatalf("PlanPrune error = %v, want ErrPruneRefused", err)
			}
			if len(plan.Delete) != 0 {
				t.Errorf("Delete = %v, want none after a refusal", plan.Delete)
			}
		})
	}
}

func TestPruneIgnoresAnUnreadableTagOfAnotherName(t *testing.T) {
	t.Parallel()
	in := PruneInput{
		Installed: pruneInstalled,
		Images: map[string][]PruneTag{
			"gastown-daemon": {pruneTag("dstale1111111", "sha256:d-stale", 0)},
			"mango/ui":       {{Name: "ui2222222222", Digest: "sha256:ui"}},
		},
	}
	plan, err := PlanPrune(in)
	if err != nil {
		t.Fatalf("PlanPrune: %v, a tag of an ignored name must not refuse the plan", err)
	}
	wantPruneKeys(t, "Delete", plan.Delete, "gastown-daemon/dstale1111111")
}
