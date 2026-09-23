package guard

import (
	"errors"
	"testing"
)

func TestZeroValueIsUnknown(t *testing.T) {
	var r Result
	if !r.IsUnknown() {
		t.Fatalf("zero Result = %v, want Unknown", r)
	}
	if r.IsPass() || r.IsFail() {
		t.Fatalf("zero Result reports IsPass=%v IsFail=%v, want both false", r.IsPass(), r.IsFail())
	}
}

// TestZeroValueErrAndStringDoNotPanic pins gt-udrrw finding 341b4ccd6f8c: a
// zero-value Result has no err attached, so String() (which formats r.err
// directly) and Err() (which returned nil) must not panic or silently lose
// the fact that this is an unset, not a passing, result.
func TestZeroValueErrAndStringDoNotPanic(t *testing.T) {
	var r Result

	if err := r.Err(); err == nil {
		t.Fatalf("zero Result.Err() = nil, want non-nil (zero value is Unknown, not Pass)")
	}

	s := r.String() // must not panic
	if s == "" || s == "pass" {
		t.Fatalf("zero Result.String() = %q, want a non-empty unknown: ... message", s)
	}
}

func TestPass(t *testing.T) {
	r := Pass()
	if !r.IsPass() {
		t.Fatalf("Pass() = %v, want IsPass", r)
	}
	if r.IsFail() || r.IsUnknown() {
		t.Fatalf("Pass() reports IsFail=%v IsUnknown=%v, want both false", r.IsFail(), r.IsUnknown())
	}
	if r.Err() != nil {
		t.Fatalf("Pass().Err() = %v, want nil", r.Err())
	}
}

func TestFail(t *testing.T) {
	r := Fail("bead not found")
	if !r.IsFail() {
		t.Fatalf("Fail() = %v, want IsFail", r)
	}
	if r.IsPass() || r.IsUnknown() {
		t.Fatalf("Fail() reports IsPass=%v IsUnknown=%v, want both false", r.IsPass(), r.IsUnknown())
	}
	if r.Err() == nil || r.Err().Error() != "bead not found" {
		t.Fatalf("Fail().Err() = %v, want %q", r.Err(), "bead not found")
	}
}

func TestUnknown(t *testing.T) {
	underlying := errors.New("connection refused")
	r := Unknown(underlying)
	if !r.IsUnknown() {
		t.Fatalf("Unknown() = %v, want IsUnknown", r)
	}
	if r.IsPass() || r.IsFail() {
		t.Fatalf("Unknown() reports IsPass=%v IsFail=%v, want both false", r.IsPass(), r.IsFail())
	}
	if !errors.Is(r.Err(), underlying) {
		t.Fatalf("Unknown().Err() = %v, want to wrap %v", r.Err(), underlying)
	}
}

// TestUnknownNeverPass is the property this package exists to hold: nothing
// in this file can turn an Unknown Result into a passing one. There is no
// method under test here — the test is that IsPass and IsUnknown never both
// report true for the same Result, across every constructor.
func TestUnknownNeverPass(t *testing.T) {
	results := []Result{
		{},
		Pass(),
		Fail("problem"),
		Unknown(errors.New("boom")),
	}
	for _, r := range results {
		if r.IsUnknown() && r.IsPass() {
			t.Fatalf("Result %v reports both IsUnknown and IsPass", r)
		}
	}
}

func TestFailPanicsOnEmptyReason(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Fail(\"\") did not panic")
		}
	}()
	Fail("")
}

func TestFailErrPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("FailErr(nil) did not panic")
		}
	}()
	FailErr(nil)
}

func TestUnknownPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Unknown(nil) did not panic")
		}
	}()
	Unknown(nil)
}

func TestSwitchCallsMatchingBranch(t *testing.T) {
	cases := []struct {
		name string
		r    Result
		want string
	}{
		{"pass", Pass(), "pass"},
		{"fail", Fail("nope"), "fail"},
		{"unknown", Unknown(errors.New("boom")), "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			c.r.Switch(
				func() { got = "pass" },
				func(err error) { got = "fail" },
				func(err error) { got = "unknown" },
			)
			if got != c.want {
				t.Fatalf("Switch on %v called %q branch, want %q", c.r, got, c.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	if got := Pass().String(); got != "pass" {
		t.Fatalf("Pass().String() = %q, want %q", got, "pass")
	}
	if got := Fail("bad shape").String(); got != "fail: bad shape" {
		t.Fatalf("Fail().String() = %q, want %q", got, "fail: bad shape")
	}
	if got := Unknown(errors.New("timeout")).String(); got != "unknown: timeout" {
		t.Fatalf("Unknown().String() = %q, want %q", got, "unknown: timeout")
	}
}
