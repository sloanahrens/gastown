package taskmap

// Placement for gt town forward bd-gateway, which prints the launchd job that
// keeps svc/bd-gateway reachable on 127.0.0.1:7440 from the Mac, so a host tool
// that expects the gateway at that address still works once the host container is gone.
func init() {
	Register(
		Entry{Path: "town forward bd-gateway", Group: "host", Use: "Use this when a host tool or the rehearsal harness must reach the bd gateway at 127.0.0.1:7440 from the Mac, to print the launchd job that keeps the port-forward to the gateway Service up for the operator to load.", Check: "once the printed job is loaded, 127.0.0.1:7440 answers from the Mac"},
	)
}
