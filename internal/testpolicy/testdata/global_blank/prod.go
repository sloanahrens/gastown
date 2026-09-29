package globalblank

import "fmt"

// T is a type with a compile-time interface assertion, which declares a
// package-level var named "_".
type T struct{}

func (T) String() string { return "t" }

var _ fmt.Stringer = T{}

func f() error { return nil }
