package editorial

import "fmt"

// ClassifiedError pairs a FailureClass with the error that produced it, so a
// review step's fail-closed classification travels with the error instead of
// being re-derived from its text at a higher layer. Routing is always on the
// Class field, never on log text (see the design's fail-closed table).
type ClassifiedError struct {
	Class FailureClass
	Err   error
}

func (e *ClassifiedError) Error() string {
	if e.Err == nil {
		return string(e.Class)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

func (e *ClassifiedError) Unwrap() error { return e.Err }
