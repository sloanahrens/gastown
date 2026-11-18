package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/imagevalues"
)

// bdPair is newPair with the bd gateway image a values file written by
// `gt update images --bd` names.
var bdPair = ImagePair{
	Daemon:  newPair.Daemon,
	Polecat: newPair.Polecat,
	Bd:      imgRef("bd-gateway", "7"),
	Commit:  newPair.Commit,
}

// writeBdValues writes f's values file with the three digests of bdPair, the
// shape a pod install reads.
func writeBdValues(t *testing.T, f *imageFixture) {
	t.Helper()
	values := imagevalues.Values{Daemon: bdPair.Daemon, Polecat: bdPair.Polecat, Bd: bdPair.Bd, Commit: bdPair.Commit}
	if err := imagevalues.Write(f.path, values); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
}

// TestImagePairBdRecordsAllThree: an images file with three digests yields a pair
// with all three, and the record the installer writes - the run state a rollback
// reads - stores them.
func TestImagePairBdRecordsAllThree(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, ImageRecord{Current: oldPair})
	writeBdValues(t, f)

	res, err := f.inst.Install(context.Background(), bdPair.Commit)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ClassifyInstall(res) != ResultOK {
		t.Fatalf("result = %+v, want a successful install", res)
	}
	if want := (ImageRecord{Current: bdPair, Previous: oldPair}); f.swap.seenAtApply[0] != want {
		t.Errorf("the record at the swap = %+v, want the pair with all three images, %+v", f.swap.seenAtApply[0], want)
	}
	if got := f.ledger.rec.Current; got != bdPair {
		t.Errorf("the recorded current pair = %+v, want all three images %+v", got, bdPair)
	}
}

// TestImagePairBdHoldsOnTheGatewayImage: pairHolds compares the bd gateway image
// against the pair the cluster runs, the way it compares the other two. A cluster
// with no bd-gateway Deployment yet names no gateway image and holds the pair on
// its other two, so a roll before the cutover still works.
func TestImagePairBdHoldsOnTheGatewayImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		running ImagePair
		swap    bool
	}{
		{
			name:    "all three match, so the pair holds",
			running: ImagePair{Daemon: bdPair.Daemon, Polecat: bdPair.Polecat, Bd: bdPair.Bd},
		},
		{
			name:    "only the gateway image differs, so the pair does not hold",
			running: ImagePair{Daemon: bdPair.Daemon, Polecat: bdPair.Polecat, Bd: imgRef("bd-gateway", "9")},
			swap:    true,
		},
		{
			name:    "a cluster with no bd-gateway Deployment holds on the other two",
			running: ImagePair{Daemon: bdPair.Daemon, Polecat: bdPair.Polecat},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageFixture(t, ImageRecord{Current: bdPair, Previous: oldPair})
			writeBdValues(t, f)
			f.inst.Images.Running = &fakeRunningPair{pair: tt.running}

			res, err := f.inst.Install(context.Background(), bdPair.Commit)
			if err != nil {
				t.Fatalf("Install: %v", err)
			}
			if ClassifyInstall(res) != ResultOK {
				t.Fatalf("result = %+v, want a successful install", res)
			}
			if !tt.swap {
				if got := f.swap.applied; len(got) != 0 {
					t.Errorf("applied = %+v, want no swap: the pair holds", got)
				}
				if f.ledger.rec != (ImageRecord{Current: bdPair, Previous: oldPair}) {
					t.Errorf("record = %+v, want it left as it was", f.ledger.rec)
				}
				return
			}
			if got := f.swap.applied; len(got) != 1 || got[0] != bdPair {
				t.Errorf("applied = %+v, want one swap to the pair %+v", got, bdPair)
			}
			if want := (ImageRecord{Current: bdPair, Previous: oldPair}); f.swap.seenAtApply[0] != want {
				t.Errorf("the record at the swap = %+v, want %+v", f.swap.seenAtApply[0], want)
			}
		})
	}
}

// TestImagePairBdRefusesADamagedRecord: a record whose bd gateway image is not a
// digest stops the install before anything changes, as a damaged daemon or
// polecat image does: a rollback puts the recorded images back, so a damaged one
// must not reach the swap.
func TestImagePairBdRefusesADamagedRecord(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, ImageRecord{})
	writeBdValues(t, f)
	badBd := ImagePair{Daemon: oldPair.Daemon, Polecat: oldPair.Polecat, Bd: "registry.internal:5000/gastown/bd-gateway@sha256:zz"}
	f.inst.Images.Ledger = &fakeImageLedger{rec: ImageRecord{Current: oldPair, Previous: badBd}}

	res, err := f.inst.Install(context.Background(), bdPair.Commit)
	if err == nil || !strings.Contains(err.Error(), "damaged pair; nothing changed") {
		t.Fatalf("Install = %+v, %v; want the damaged bd gateway image refused with nothing changed", res, err)
	}
	if f.host.calls != 0 {
		t.Errorf("the host install ran %d times, want none", f.host.calls)
	}
	if len(f.swap.applied) != 0 {
		t.Errorf("the daemon was swapped to %+v, want no swap", f.swap.applied)
	}
}

// TestImagePairBdStateFile holds the record's file format: a pair with a gateway
// image round-trips, a pair without one writes no bd key so a file is shaped as
// an older one was, and an older file that names no bd key reads with no gateway
// image.
func TestImagePairBdStateFile(t *testing.T) {
	t.Parallel()

	t.Run("a pair with a gateway image round-trips", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ledger := FileImageLedger{Dir: dir}
		want := ImageRecord{Current: bdPair, Previous: oldPair}
		if err := ledger.Write(want); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got, err := ledger.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got.Current != bdPair {
			t.Errorf("the read current pair = %+v, want %+v with the gateway image", got.Current, bdPair)
		}
	})

	t.Run("a pair with no gateway image writes no bd key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := (FileImageLedger{Dir: dir}).Write(ImageRecord{Current: oldPair, Previous: oldPair}); err != nil {
			t.Fatalf("Write: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, imageRecordFileName))
		if err != nil {
			t.Fatalf("read the record file: %v", err)
		}
		var raw struct {
			Current  map[string]any `json:"current"`
			Previous map[string]any `json:"previous"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("parse the record file: %v", err)
		}
		for _, pair := range []map[string]any{raw.Current, raw.Previous} {
			if _, ok := pair["bd"]; ok {
				t.Errorf("the record %s names a bd key for a pair with no gateway image: %s", pair, data)
			}
		}
	})

	t.Run("an older file with no bd key reads with no gateway image", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		older := `{"v":1,"current":{"daemon":"` + oldPair.Daemon + `","polecat":"` + oldPair.Polecat + `","commit":"` + oldPair.Commit + `"},"previous":{"daemon":"` + oldPair.Daemon + `","polecat":"` + oldPair.Polecat + `","commit":"` + oldPair.Commit + `"}}`
		if err := os.WriteFile(filepath.Join(dir, imageRecordFileName), []byte(older), 0o600); err != nil {
			t.Fatalf("write the older record: %v", err)
		}
		got, err := (FileImageLedger{Dir: dir}).Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got.Current != oldPair {
			t.Errorf("the read current pair = %+v, want %+v with no gateway image", got.Current, oldPair)
		}
		if err := validRecordedPair(got.Current); err != nil {
			t.Errorf("validRecordedPair(older pair) = %v, want a record written before the gateway accepted", err)
		}
	})
}
