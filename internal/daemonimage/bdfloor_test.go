package daemonimage

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckBDFloor(t *testing.T) {
	t.Parallel()
	const tip = "0123456789abcdef0123456789abcdef01234567"
	compareErr := errors.New("compare failed")
	tests := []struct {
		name        string
		onlyInFloor int
		err         error
		wantErr     bool
		wantWrapped error
	}{
		{name: "the floor is an ancestor of the tip", onlyInFloor: 0},
		{name: "the tip predates the floor", onlyInFloor: 3, wantErr: true},
		{name: "a negative count is not a compare answer", onlyInFloor: -1, wantErr: true},
		{name: "a compare error is refused", err: compareErr, wantErr: true, wantWrapped: compareErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CheckBDFloor(tip, tc.onlyInFloor, tc.err)
			if !tc.wantErr {
				if got != nil {
					t.Fatalf("CheckBDFloor(%q, %d, nil) = %v, want nil", tip, tc.onlyInFloor, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("CheckBDFloor(%q, %d, %v) = nil, want an error", tip, tc.onlyInFloor, tc.err)
			}
			for _, want := range []string{BDSamplesFloor, tip} {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("CheckBDFloor(%q, %d, %v) error = %q, want it to name %q", tip, tc.onlyInFloor, tc.err, got, want)
				}
			}
			if tc.wantWrapped != nil && !errors.Is(got, tc.wantWrapped) {
				t.Errorf("CheckBDFloor(%q, %d, %v) error = %v, want it to wrap %v", tip, tc.onlyInFloor, tc.err, got, tc.wantWrapped)
			}
		})
	}
}

func TestBDSamplesFloorIsAFullSHA(t *testing.T) {
	t.Parallel()
	t.Run("40 characters", func(t *testing.T) {
		t.Parallel()
		if len(BDSamplesFloor) != 40 {
			t.Errorf("BDSamplesFloor = %q has %d characters, want 40", BDSamplesFloor, len(BDSamplesFloor))
		}
	})
	t.Run("lowercase hex", func(t *testing.T) {
		t.Parallel()
		for _, r := range BDSamplesFloor {
			if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
				t.Errorf("BDSamplesFloor = %q holds %q, want only lowercase hex", BDSamplesFloor, r)
			}
		}
	})
}
