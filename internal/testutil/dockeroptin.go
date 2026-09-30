package testutil

// The opt-in lives in a file with no build constraint: hermetic.go (every
// platform) reads it, and it used to sit in doltserver.go (!windows), which
// broke `go build ./...` on Windows.

// DockerTestsEnv opts the container-backed tests in. Every entry point that
// would start a Dolt/testcontainers container skips unless it is "1", so an
// ordinary `go test ./internal/cmd -run TestFoo` compiles and runs in seconds
// without touching Docker or the container-gate slot. `make test` (the
// refinery gate and the daemon's main-branch patrol) sets it; a polecat that
// wants the container tests sets it explicitly and runs under gt slot run.
// Measured 2026-09-18 (gt-pxlg): five polecats spent ~81 of 185 agent-minutes
// queued on the slot for filtered runs Go finished in 4-17s, because any run
// of a container-bearing package had to take the slot whether or not the
// filter selected a container test.
const DockerTestsEnv = "GT_TEST_DOCKER"

// DockerTestsEnabled reports whether container-backed tests may run in this
// process.
func DockerTestsEnabled() bool {
	return dockerTestsEnabled(procEnv{})
}

func dockerTestsEnabled(env environment) bool {
	return getenv(env, DockerTestsEnv) == "1"
}
