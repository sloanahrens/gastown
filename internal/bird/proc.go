package bird

import "errors"

// ErrUnsupportedPlatform is what a liveness read answers with where the platform
// has no process start token. It is not a death: List, Taken and Pick fail with
// it and keep every record, rather than prune a bird that may be running or hand
// out a name that is not free.
var ErrUnsupportedPlatform = errors.New("bird: process start tokens are unsupported on this platform")
