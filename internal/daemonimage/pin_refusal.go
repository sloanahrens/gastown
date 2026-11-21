package daemonimage

import (
	"errors"
	"fmt"
)

// ErrPinRefused marks a judgment that an image pin must not move: the bd tip
// lacks the samples build, say. A refusal is not a failed call, so callers test
// for it with errors.Is, however the error is wrapped.
var ErrPinRefused = errors.New("image pin refused")

// pinRefusal is the error Refusef returns. It prints only its formatted text and
// matches ErrPinRefused through errors.Is.
type pinRefusal struct{ msg string }

func (e *pinRefusal) Error() string { return e.msg }

func (e *pinRefusal) Is(target error) bool { return target == ErrPinRefused }

// Refusef returns a pin refusal whose text is the formatted message.
func Refusef(format string, args ...any) error {
	return &pinRefusal{msg: fmt.Sprintf(format, args...)}
}
