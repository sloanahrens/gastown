package land

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// TestReadGateRunsDecodesARowWithoutPackageCount: a row written before the packages cap has no
// package_count key and every package. ReadGateRuns reads it beside a post-cap row from the same
// file, nothing unparsed (gt-kwpn3u).
func TestReadGateRunsDecodesARowWithoutPackageCount(t *testing.T) {
	t.Parallel()
	oldPkgs := make([]GateRunPackage, 30)
	for i := range oldPkgs {
		oldPkgs[i] = GateRunPackage{Pkg: fmt.Sprintf("example.test/old%d", i), S: float64(i)}
	}
	// The old shape: the packages list and no package_count key.
	oldLine, err := json.Marshal(struct {
		Bead     string           `json:"bead"`
		Packages []GateRunPackage `json:"packages"`
	}{"gt-old", oldPkgs})
	if err != nil {
		t.Fatalf("marshal the old-shape row: %v", err)
	}
	newPkgs := make([]GateRunPackage, gateRunPackagesKept)
	for i := range newPkgs {
		newPkgs[i] = GateRunPackage{Pkg: fmt.Sprintf("example.test/new%d", i), S: float64(100 - i)}
	}
	newRow := GateRun{Bead: "gt-new", Packages: newPkgs, PackageCount: 30}
	town := t.TempDir()
	writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", string(oldLine)+"\n"+mustLine(t, newRow)+"\n")

	got, unparsed, err := ReadGateRuns(town)
	if err != nil {
		t.Fatalf("ReadGateRuns: %v", err)
	}
	if unparsed != 0 {
		t.Errorf("unparsed = %d, want 0: both generations of row read", unparsed)
	}
	if len(got) != 2 {
		t.Fatalf("ReadGateRuns = %d rows, want the old row and the new one", len(got))
	}
	wantOld := GateRun{Bead: "gt-old", Packages: oldPkgs}
	if !reflect.DeepEqual(got[0], wantOld) {
		t.Errorf("old row = %+v, want %+v: its PackageCount is 0 and all %d packages are present", got[0], wantOld, len(oldPkgs))
	}
	if got[0].PackageCount != 0 {
		t.Errorf("old row PackageCount = %d, want 0: the row carries no count", got[0].PackageCount)
	}
	if !reflect.DeepEqual(got[1], newRow) {
		t.Errorf("new row = %+v, want it as written: %+v", got[1], newRow)
	}
}
