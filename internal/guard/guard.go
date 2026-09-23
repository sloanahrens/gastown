// Package guard defines the tri-state result a guard function returns:
// Pass, Fail, or Unknown.
//
// A guard checks some input and reports whether it is fine. The recurring
// defect this package exists to close (gt-udrrw) is guards written so that an
// input the guard could not even read — an unreadable file, a failed
// subprocess, a broken connection — produces the same result as an input the
// guard read and found fine. The caller then cannot tell "I could not check
// this" from "this checks out", and a read failure silently becomes an
// all-clear. One instance shipped a false "clean" verdict over a staged
// revert (gt-wisp-10qu); another recurred one level inside its own fix
// (gt-wisp-rnzh, state_collapse.go).
//
// Unknown is the state that closes the gap: it is never Pass, and there is no
// conversion from Unknown to Pass anywhere in this package. A caller that
// wants to treat Unknown as if it were Pass has to write that decision out
// explicitly at the call site — it cannot happen by omission.
package guard

import (
	"errors"
	"fmt"
)

// errZeroValue is the error a zero-value Result reports: an unset Result is
// Unknown, and Unknown requires a reason, so a caller that never called Pass,
// Fail, or Unknown still gets a non-nil Err() and a String() that does not
// panic.
var errZeroValue = errors.New("guard: zero-value Result (Unknown, no reason set)")

// state is unexported so the only way to produce a Result is through Pass,
// Fail, or Unknown below — a caller cannot construct the zero value of a
// passing state by accident.
type state uint8

const (
	// stateUnknown is the zero value: an unset Result reads as Unknown, not as
	// Pass, so a Result left uninitialized by a caller who forgot to set it
	// fails closed rather than silently passing.
	stateUnknown state = iota
	statePass
	stateFail
)

// Result is a guard's verdict on the input it checked: Pass, Fail, or
// Unknown. The zero Result is Unknown with no error attached — use Pass,
// Fail, or Unknown to build one.
type Result struct {
	state state
	err   error
}

// Pass reports that the guard checked its input and found nothing wrong.
func Pass() Result {
	return Result{state: statePass}
}

// Fail reports that the guard checked its input and found a problem. reason
// says what, and is required: a Fail with no reason is the same information
// loss this package exists to prevent, one level up.
func Fail(reason string) Result {
	if reason == "" {
		panic("guard.Fail: empty reason")
	}
	return Result{state: stateFail, err: fmt.Errorf("%s", reason)}
}

// FailErr is Fail for a caller that already has an error describing the
// problem, rather than a string to wrap.
func FailErr(err error) Result {
	if err == nil {
		panic("guard.FailErr: nil error")
	}
	return Result{state: stateFail, err: err}
}

// Unknown reports that the guard could not check its input — the read that
// would have produced Pass or Fail failed instead. err is required and says
// why; a caller that cannot say why the check failed has not actually failed
// to check, and should ask whether it means Fail instead.
func Unknown(err error) Result {
	if err == nil {
		panic("guard.Unknown: nil error")
	}
	return Result{state: stateUnknown, err: err}
}

// IsPass reports whether the guard checked its input and found it fine.
func (r Result) IsPass() bool { return r.state == statePass }

// IsFail reports whether the guard checked its input and found a problem.
func (r Result) IsFail() bool { return r.state == stateFail }

// IsUnknown reports whether the guard could not check its input. A caller
// that branches on IsFail alone and treats everything else as passing has
// reintroduced the bug this type exists to prevent — IsUnknown must be
// checked on its own, not inferred from !IsFail.
func (r Result) IsUnknown() bool { return r.state == stateUnknown }

// Err returns why the guard did not pass: the problem found (Fail) or the
// reason the check could not run (Unknown). It is nil for Pass, and never nil
// for Fail or Unknown — including the zero-value Result, which reports
// errZeroValue rather than nil.
func (r Result) Err() error {
	if r.state != statePass && r.err == nil {
		return errZeroValue
	}
	return r.err
}

// String renders the result for logs: "pass", "fail: <reason>", or
// "unknown: <reason>".
func (r Result) String() string {
	switch r.state {
	case statePass:
		return "pass"
	case stateFail:
		return "fail: " + r.Err().Error()
	default:
		return "unknown: " + r.Err().Error()
	}
}

// Switch calls exactly one of onPass, onFail, or onUnknown, matching r's
// state. All three are required parameters, so a caller cannot compile code
// that handles Pass and Fail while leaving Unknown to fall through to
// whichever branch runs last.
func (r Result) Switch(onPass func(), onFail func(err error), onUnknown func(err error)) {
	switch r.state {
	case statePass:
		onPass()
	case stateFail:
		onFail(r.err)
	default:
		onUnknown(r.err)
	}
}
