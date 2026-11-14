//go:build !linux

package hoststat

import (
	"context"
	"errors"
)

// topOutput reports that this platform has no top probe, so the package
// compiles wherever the town is built rather than only on the platform whose
// top it drives. A sample there carries the load averages and the process
// table and no idle share (gt-auiji). Darwin is build-only since the gt-wrap
// cutover (gt-qq5kv4): nothing runs hoststat there, so this error is not a
// supported runtime value.
func topOutput(context.Context) ([]byte, error) {
	return nil, errors.New("hoststat: no top probe for this platform")
}
