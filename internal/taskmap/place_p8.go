package taskmap

// The P8 batch of task map entries: the gt daemon commands. Each Use names the
// situation an agent or an operator is in, not what the command does inside.
//
// The batch's bead listed seventeen daemon paths, four of them the gt daemon
// scratch commands deleted before this batch landed (gt-b2rxeg); the thirteen
// that remain are the entries below.
//
// Every entry here is in group host, because each one inspects or changes the
// daemon process on this machine: starting it, stopping it, reading its log,
// or rendering and checking the container it runs in. None of them is a step an
// agent takes to place itself (orient) or to hand work to someone (dispatch).
// daemon status is the one that reads like orient; it stays in host because it
// reports a process, not where the agent is or what to do next (gt-j669b4.12).
func init() {
	Register(
		Entry{Path: "daemon", Group: "host", Use: "Use this to manage the town's background daemon, which pokes agents, restarts their sessions and runs their lifecycle requests."},
		Entry{Path: "daemon clear-backoff", Group: "host", Use: "Use this when an agent has crashed repeatedly and the daemon has stopped restarting it, once you have fixed the cause.", Check: "gt daemon logs shows the agent restarting again"},
		Entry{Path: "daemon compose", Group: "host", Use: "Use this to reach the steps for the town's daemon container compose file; daemon compose write is the one that renders and writes it."},
		Entry{Path: "daemon compose write", Group: "host", Use: "Use this when the town's daemon settings have changed and the live compose file must be rewritten before the container is started again.", Check: "the command prints the Wrote line naming the compose file and the container it describes"},
		Entry{Path: "daemon enable-supervisor", Group: "host", Use: "Use this when the daemon must come back by itself after a crash or a reboot, on a Linux host with systemd.", Check: "gt daemon status exits 0"},
		Entry{Path: "daemon image", Group: "host", Use: "Use this when the daemon container needs an image for a gt revision, reading the dry-run plan first and building with --apply."},
		Entry{Path: "daemon logs", Group: "host", Use: "Use this when you need to know why the daemon restarted, skipped or refused an agent, from its own log."},
		Entry{Path: "daemon rehearse", Group: "host", Use: "Use this before a container daemon is started, to see whether a scratch copy of the live settings passes every check without touching the live town.", Check: "the printed table lists each check and the command exits 0"},
		Entry{Path: "daemon restart", Group: "host", Use: "Use this after replacing the gt binary, so the daemon runs the new program rather than the one it started from.", Check: "gt daemon status no longer reports a rebuilt binary"},
		Entry{Path: "daemon rotate-logs", Group: "host", Use: "Use this when the daemon's log files have grown past the size limit and you want them rotated in place."},
		Entry{Path: "daemon start", Group: "host", Use: "Use this when the daemon is not running, after a reboot or a stop, and the town's agents should be supervised again.", Check: "gt daemon status exits 0"},
		Entry{Path: "daemon status", Group: "host", Use: "Use this to check whether the daemon is running and whether it still runs the binary now on disk, before you blame a command on the daemon.", Check: "the exit code is 0 when the daemon runs and 3 when it does not"},
		Entry{Path: "daemon stop", Group: "host", Use: "Use this when the daemon must stay quiet, such as during maintenance, and you will start it again yourself.", Check: "gt daemon status exits 3"},
	)
}
