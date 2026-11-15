package taskmap

// Placements for batch P11, half B part 1 (gt-j669b4.22): the commands that
// plan the Dolt server as a container, snapshot its diagnostics, and repair
// the rig workspaces that name a database it should serve.
//
// Group judgement: all five sit in dolt. host covers the machine as a whole,
// but each of these acts on the beads database's server, and the agent asking
// why bd reads the wrong database starts in dolt. The container pair is the
// closest call: gt only prints the docker command for a person to review and
// run, which keeps it in dolt beside the server it describes.
func init() {
	Register(
		Entry{Path: "dolt container", Group: "dolt", Use: "Use this when the town's Dolt server is to run in a Docker container rather than on the host, to reach the command that plans it."},
		Entry{Path: "dolt container plan", Group: "dolt", Use: "Use this when a person must start the town's Dolt server as a Docker container and needs the exact docker create command to read and approve first, with nothing run and no file written.", Check: "the printed line starts docker create"},
		Entry{Path: "dolt dump", Group: "dolt", Use: "Use this when the Dolt server has misbehaved and an incident needs its PID, port, state file and recent log lines in one snapshot, taken without signalling the server.", Check: "the snapshot names the live PID, port and data directory"},
		Entry{Path: "dolt fix-metadata", Group: "dolt", Use: "Use this when bd has fallen back to a local embedded database because a rig's metadata.json does not name the shared Dolt server, and every rig's file should be brought into line.", Check: "the run prints Updated metadata.json for N rig(s), or that there was nothing to update"},
		Entry{Path: "dolt init", Group: "dolt", Use: "Use this when a rig's metadata names a Dolt database that is missing from the data directory, so the workspace is repaired from local data or given a fresh database.", Check: "the run ends All workspaces healthy or Repaired N/M workspace(s)"},
	)
}
