package prodonly

import "example.com/testutil"

// A production file calling an entry point is not a test that starts one.
func Enabled() bool { return testutil.DockerTestsEnabled() }
