package townconfig

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// legacyTown is the live town with the wisp files it carried before the
// registry field: four rigs parked through the wisp layer, one of them (hm)
// with another value beside the park, and gastown with no park at all.
func legacyTown(t *testing.T) string {
	t.Helper()
	root := copyLiveTown(t)
	write(t, root, LegacyParkedDir+"/beads.json", `{"rig":"beads","values":{"status":"parked"},"blocked":[]}`)
	write(t, root, LegacyParkedDir+"/gastown.json", `{"rig":"gastown","values":{"max_polecats":12},"blocked":[]}`)
	write(t, root, LegacyParkedDir+"/hm.json", `{"rig":"hm","values":{"max_polecats":2,"status":"parked"},"blocked":[]}`)
	write(t, root, LegacyParkedDir+"/mango.json", `{"rig":"mango","values":{"status":"parked"},"blocked":[]}`)
	write(t, root, LegacyParkedDir+"/om.json", `{"rig":"om","values":{"status":"parked"},"blocked":[]}`)
	return root
}

func parkedRigs(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, name := range []string{"beads", "gastown", "hm", "mango", "om"} {
		if parked, _ := IsParked(root, name); parked {
			out = append(out, name)
		}
	}
	return out
}

func TestParkedRegistryField(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	since := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rec, err := Park(root, "om", config.RigParked{Since: since, By: "sloan", Reason: "rubric work"})
	if err != nil {
		t.Fatalf("Park = %v", err)
	}
	if rec.By != "sloan" {
		t.Errorf("Park returned %+v", rec)
	}
	p, err := ParkState(root, "om")
	if err != nil || p == nil || !p.Since.Equal(since) || p.By != "sloan" || p.Reason != "rubric work" {
		t.Fatalf("ParkState(om) = %+v, %v", p, err)
	}
	if parked, why := IsParked(root, "om"); !parked || !strings.Contains(why, "rubric work") {
		t.Errorf("IsParked(om) = %v, %q", parked, why)
	}
	if p, err := ParkState(root, "gastown"); p != nil || err != nil {
		t.Errorf("ParkState(gastown) = %+v, %v; want not parked", p, err)
	}

	// A second park keeps the first record.
	again, err := Park(root, "om", config.RigParked{Since: since.Add(time.Hour), By: "mayor", Reason: "other"})
	if err != nil || again.By != "sloan" {
		t.Errorf("second Park = %+v, %v; want the first record kept", again, err)
	}

	was, err := Unpark(root, "om")
	if err != nil || !was {
		t.Fatalf("Unpark = %v, %v", was, err)
	}
	if p, err := ParkState(root, "om"); p != nil || err != nil {
		t.Errorf("after Unpark: %+v, %v", p, err)
	}
	if was, err := Unpark(root, "om"); err != nil || was {
		t.Errorf("second Unpark = %v, %v; want false, nil", was, err)
	}
	// The other rigs' registry entries survive the writes.
	town, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := town.RigPrefix("mango"); p != "ma" {
		t.Errorf("mango prefix = %q after park writes", p)
	}
}

func TestParkUnknownRigIsAnError(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	if _, err := Park(root, "nope", config.RigParked{}); !errors.Is(err, ErrUnknownRig) {
		t.Errorf("Park(unknown) = %v, want ErrUnknownRig", err)
	}
	if _, err := Unpark(root, "nope"); !errors.Is(err, ErrUnknownRig) {
		t.Errorf("Unpark(unknown) = %v, want ErrUnknownRig", err)
	}
}

func TestParkedFailsClosed(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, root string){
		"unparseable rigs.json": func(t *testing.T, root string) { write(t, root, FileRigs, "{not json") },
		"unknown key in rigs.json": func(t *testing.T, root string) {
			write(t, root, FileRigs, `{"version":1,"rigs":{"om":{"git_url":"x","added_at":"2026-09-07T00:00:00Z","parkd":{}}}}`)
		},
		"unreadable rigs.json": func(t *testing.T, root string) {
			if err := os.Chmod(filepath.Join(root, FileRigs), 0); err != nil {
				t.Fatal(err)
			}
		},
		"broken settings":    func(t *testing.T, root string) { write(t, root, FileSettings, "{") },
		"no town.json":       func(t *testing.T, root string) { _ = os.Remove(filepath.Join(root, FileTown)) },
		"unparseable legacy": func(t *testing.T, root string) { write(t, root, LegacyParkedDir+"/om.json", "{") },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := copyLiveTown(t)
			breakIt(t, root)
			parked, why := IsParked(root, "om")
			if !parked || !strings.Contains(why, "treated as parked") {
				t.Errorf("IsParked = %v, %q; want parked (fail closed)", parked, why)
			}
		})
	}
	t.Run("unregistered rig", func(t *testing.T) {
		t.Parallel()
		if parked, _ := IsParked(copyLiveTown(t), "nope"); !parked {
			t.Error("an unregistered rig read as unparked")
		}
	})
}

// TestLegacyParkedReadsParkedUntilMigrated is the no-window guarantee: a rig
// parked only through its wisp file reads as parked before any migration.
func TestLegacyParkedReadsParkedUntilMigrated(t *testing.T) {
	t.Parallel()
	root := legacyTown(t)
	if got := strings.Join(parkedRigs(t, root), ","); got != "beads,hm,mango,om" {
		t.Errorf("parked before migration = %s, want beads,hm,mango,om", got)
	}
	if _, err := ParkState(root, "hm"); !errors.Is(err, ErrLegacyParked) {
		t.Errorf("ParkState(hm) = %v, want ErrLegacyParked", err)
	}
	if p, err := ParkState(root, "gastown"); p != nil || err != nil {
		t.Errorf("ParkState(gastown) = %+v, %v; a wisp file without status is not a park", p, err)
	}
}

func TestMigrateLegacyParked(t *testing.T) {
	t.Parallel()
	root := legacyTown(t)
	mtime := time.Date(2026, 9, 21, 18, 22, 0, 0, time.UTC)
	if err := os.Chtimes(legacyParkedPath(root, "beads"), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateLegacyParked(root, "gt rig park --migrate")
	if err != nil {
		t.Fatalf("MigrateLegacyParked = %v", err)
	}
	if want := []string{"beads", "hm", "mango", "om"}; !reflect.DeepEqual(migrated, want) {
		t.Errorf("migrated = %v, want %v", migrated, want)
	}
	if got := strings.Join(parkedRigs(t, root), ","); got != "beads,hm,mango,om" {
		t.Errorf("parked after migration = %s", got)
	}
	p, err := ParkState(root, "beads")
	if err != nil || p == nil {
		t.Fatalf("ParkState(beads) = %+v, %v; want a registry record", p, err)
	}
	if !p.Since.Equal(mtime) || p.By != "gt rig park --migrate" || p.Reason != "migrated from .beads-wisp/config/beads.json" {
		t.Errorf("beads record = %+v", p)
	}

	// The status key is gone; files with nothing else in them are gone.
	for _, name := range []string{"beads", "mango", "om"} {
		if _, err := os.Stat(legacyParkedPath(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s wisp file still present (%v)", name, err)
		}
	}
	data, err := os.ReadFile(legacyParkedPath(root, "hm"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(data); strings.Contains(s, "status") || !strings.Contains(s, "max_polecats") {
		t.Errorf("hm wisp file = %s; want status removed, max_polecats kept", s)
	}
	if data, _ := os.ReadFile(legacyParkedPath(root, "gastown")); !strings.Contains(string(data), "max_polecats") {
		t.Errorf("gastown wisp file changed: %s", data)
	}

	again, err := MigrateLegacyParked(root, "gt rig park --migrate")
	if err != nil || len(again) != 0 {
		t.Errorf("second migration = %v, %v; want nothing to do", again, err)
	}
}

func TestMigrateLeavesUnknownStatusParked(t *testing.T) {
	t.Parallel()
	root := legacyTown(t)
	write(t, root, LegacyParkedDir+"/gastown.json", `{"rig":"gastown","values":{"status":"docked"},"blocked":[]}`)
	migrated, err := MigrateLegacyParked(root, "test")
	if err == nil || !strings.Contains(err.Error(), `"docked"`) {
		t.Errorf("err = %v, want the docked value reported", err)
	}
	if len(migrated) != 4 {
		t.Errorf("migrated = %v, want the four parked rigs anyway", migrated)
	}
	if parked, _ := IsParked(root, "gastown"); !parked {
		t.Error("gastown with an unmigrated status read as unparked")
	}
	// gt rig unpark resolves it.
	if was, err := Unpark(root, "gastown"); err != nil || !was {
		t.Fatalf("Unpark(gastown) = %v, %v", was, err)
	}
	if parked, why := IsParked(root, "gastown"); parked {
		t.Errorf("after Unpark: parked, %q", why)
	}
}

func TestParkRewritesLegacyRecord(t *testing.T) {
	t.Parallel()
	root := legacyTown(t)
	if _, err := Park(root, "hm", config.RigParked{Since: time.Now().UTC(), By: "sloan", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if p, err := ParkState(root, "hm"); err != nil || p == nil || p.By != "sloan" {
		t.Errorf("ParkState(hm) = %+v, %v", p, err)
	}
}
