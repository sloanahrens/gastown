// Package a holds one thing.
package a

// One returns one.
func One() int { return 1 }

type Thing struct{}

func (Thing) Do() {}

func hidden() {}
