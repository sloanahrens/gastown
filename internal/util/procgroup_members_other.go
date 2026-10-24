//go:build !linux && !windows

package util

import "errors"

// procGroupMembers has no /proc to read here: the caller keeps the original
// refusal.
func procGroupMembers(int) ([]int, error) {
	return nil, errors.New("process group members cannot be listed on this platform")
}
