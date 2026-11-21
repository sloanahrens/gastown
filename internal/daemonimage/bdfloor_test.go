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

// The floor's two judgments are refusals, so a hold that keeps a failed pin's last good state
// can tell them from a failed compare, which is a plain error (gt-4yarr6).
func TestCheckBDFloorJudgmentsAreRefusalsAndACompareErrorIsNot(t *testing.T) {
	t.Parallel()
	const tip = "0123456789abcdef0123456789abcdef01234567"
	compareErr := errors.New("compare failed")
	for _, tc := range []struct {
		name        string
		onlyInFloor int
		err         error
		refusal     bool
		text        string
	}{
		{"a count above zero", 3, nil, true, "bd-floor guard: bd tip " + tip + " does not contain the samples build " + BDSamplesFloor + ": 3 commit(s) reachable only from the floor"},
		{"a negative count", -1, nil, true, "bd-floor guard: bd tip " + tip + " against the samples build " + BDSamplesFloor + ": commit count -1 is negative"},
		{"a compare error", 0, compareErr, false, "bd-floor guard: cannot compare bd tip " + tip + " with the samples build " + BDSamplesFloor + ": compare failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CheckBDFloor(tip, tc.onlyInFloor, tc.err)
			if got == nil || got.Error() != tc.text {
				t.Fatalf("CheckBDFloor(%q, %d, %v) = %v, want the error %q", tip, tc.onlyInFloor, tc.err, got, tc.text)
			}
			if refused := errors.Is(got, ErrPinRefused); refused != tc.refusal {
				t.Errorf("errors.Is(%v, ErrPinRefused) = %v, want %v", got, refused, tc.refusal)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Errorf("%v does not wrap the compare error %v", got, tc.err)
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
