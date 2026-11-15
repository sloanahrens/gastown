package taskmap

// Placements for batch P11, half B part 2 (gt-fenzx8): the commands that
// create a rig's database on the Dolt server, list those databases, kill a
// stray server holding the town's port, measure round trips, and read the
// server's log.
//
// Group judgement: all five sit in dolt. Each one acts on the Dolt server or
// the databases it serves, and the agent asking why bd reads the wrong
// database starts in dolt. kill-imposters is the closest call: it signals a
// process, which could read as host or shutdown, but it only ever stops a
// stray server on the Dolt port, so it stays beside the server it repairs.
func init() {
	Register(
		Entry{Path: "dolt init-rig", Group: "dolt", Use: "Use this when a rig needs a database of its own on the town's Dolt server and none exists yet, so the rig's beads have somewhere to live.", Check: "the report gives the rig database's location"},
		Entry{Path: "dolt kill-imposters", Group: "dolt", Use: "Use this when the town's Dolt port answers but serves another directory's data, so a stray server is holding the port and bd reads the wrong database.", Check: "the run prints No imposters found on port N, or Imposter killed (PID N)"},
		Entry{Path: "dolt latency", Group: "dolt", Use: "Use this when bd feels slow against the Dolt server and you need the round-trip time measured before blaming the queries or the network.", Check: "the run prints samples, p50, p95 and max"},
		Entry{Path: "dolt list", Group: "dolt", Use: "Use this to see which rig databases the town's Dolt data directory holds and which rig owns each one.", Check: "the output names each database with its owner, or says none were found"},
		Entry{Path: "dolt logs", Group: "dolt", Use: "Use this when the Dolt server misbehaves and you need its most recent log lines before deciding what to restart.", Check: "the output ends with the server's latest log lines"},
	)
}
