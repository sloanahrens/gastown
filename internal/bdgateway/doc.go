// Package bdgateway drives bd through the gateway that runs it as its own
// container. Before editing it, read docs/design/bd-container.md: Phase 0 is
// the integration proof that a real bd accepts a gateway-owned workspace, and
// Phase 1 builds the gateway on that proof.
//
// Running the Phase 0 proof. It needs a real bd and a Dolt endpoint that holds
// a bead; elsewhere it skips, unless BD_PROOF_REQUIRE says a missing
// prerequisite must fail instead. The overseer sets:
//
//	BD_PROOF_BEAD                a bead id in the endpoint's database, e.g. gt-p02maf
//	BEADS_DOLT_SERVER_HOST       the Dolt endpoint bd reads
//	BEADS_DOLT_SERVER_PORT       the endpoint's port
//	BD_REAL_BIN                  optional: the bd to run; PATH's bd otherwise
//	BD_PROOF_PROJECT_ID          optional: a uuid written into metadata.json, which
//	                             exercises bd's project-id check
//	BD_PROOF_REQUIRE=1           a missing prerequisite fails instead of skipping
//
// and runs, from the repository root:
//
//	go vet -tags integration ./internal/bdgateway
//	BD_PROOF_BEAD=gt-p02maf BEADS_DOLT_SERVER_HOST=<host> \
//	  BEADS_DOLT_SERVER_PORT=<port> BD_PROOF_REQUIRE=1 \
//	  go test -tags integration ./internal/bdgateway -count=1 -v
//
// The integration run takes no -run filter — the build tag selects the tier and
// that tags every test in this package (docs/testing.md) — so a filter that
// matches no name cannot turn the proof into a vacuous pass.
package bdgateway
