package land

import "reflect"

// FirstSight records a submission the first time the landing worker sees it,
// before the worker lands anything.
type FirstSight interface {
	Record(w Work) (bool, error)
}

// FirstSightComposite runs several first-sight recorders on one submission.
// The landing worker has a single hook, and more than one record is owed: the
// submission battery's record and the done event a sandbox seat could not
// write (gt-1m9gr). Each recorder runs even when an earlier one fails, so one
// unreadable log never costs the other its record; the first error is what the
// worker logs, once per run of failures.
type FirstSightComposite struct {
	Recorders []FirstSight
}

// Record runs every recorder and reports whether any wrote, with the first
// error. A nil or typed-nil recorder is skipped rather than panicking on a
// hook that was never wired.
func (c *FirstSightComposite) Record(w Work) (bool, error) {
	wrote := false
	var firstErr error
	for _, rec := range c.Recorders {
		if nilled(rec) {
			continue
		}
		ok, err := rec.Record(w)
		if ok {
			wrote = true
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return wrote, firstErr
}

// nilled reports whether rec is nil, including an interface holding a nil
// pointer (a *DoneEventRecorder field never set is exactly that).
func nilled(rec FirstSight) bool {
	if rec == nil {
		return true
	}
	v := reflect.ValueOf(rec)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
