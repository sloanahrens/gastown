//go:build !linux

package hoststat

import (
	"context"
	"errors"
)

// topOutput reports that this platform has no top probe, so the package
// compiles wherever the town is built rather than only on the platform whose
// top it drives. A sample there carries the load averages and the process
// table and no idle share (gt-auiji).
func topOutput(context.Context) ([]byte, error) {
	return nil, errors.New("hoststat: no top probe for this platform")
}
