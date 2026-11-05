// Package buildstamp is the one rule for the commit stamp gt records: the
// first nine characters of the full sha. The Go producers call it, so the stamp
// the daemon image bakes into its gt and the one deploysource checks an install
// at cannot drift when git's abbreviation length changes with a checkout's
// object count (gt-x5kgt).
package buildstamp

import (
	"errors"
	"fmt"
	"strings"
)

// Len is the number of characters of a full commit sha that gt records.
const Len = 9

// Short returns the first Len characters of full, which must be a full
// 40-character lowercase hex commit sha. Any other input is an error: a stamp
// is never padded, guessed or cut from something that is not a full sha.
func Short(full string) (string, error) {
	if len(full) != 40 {
		return "", fmt.Errorf("buildstamp: commit is %d characters, want a full 40-character sha", len(full))
	}
	if strings.Trim(full, "0123456789abcdef") != "" {
		return "", errors.New("buildstamp: commit is not lowercase hex")
	}
	return full[:Len], nil
}
