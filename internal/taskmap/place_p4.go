package taskmap

// Placements for batch P4: the agent pause pair and the agents family, the
// directive commands, handoff, the hook family, the model table, the sandbox
// profile, and the capacity scheduler. Each Use names the situation an agent
// is in, not what the command does inside.
//
// Group judgement: the pause pair and the agents verbs sit in seats because
// each inspects or changes an agent's own session or bead. The scheduler and
// the hook verbs sit in dispatch because they decide and carry the work an
// agent is given. The directive reads sit in orient because a seat reads them
// to learn what it is told to do; directive edit sits in host because it
// changes the files the town holds, as rig settings do. Model sits in host for
// the same reason: it rewrites the town's config files. Handoff sits in seats
// beside cycle, because its main job is to replace this session with a fresh
// one.
func init() {
	Register(
		Entry{Path: "agent", Group: "seats", Use: "Use this when one polecat must be parked so no scanner restarts or kills it, or when its pause must be cleared."},
		Entry{Path: "agent pause", Group: "seats", Use: "Use this when a polecat misbehaves and must finish or idle untouched while you find out why.", Check: "gt status marks the agent PAUSED with the reason"},
		Entry{Path: "agent resume", Group: "seats", Use: "Use this when a polecat's pause is over, or an old pause marker or bead mirror is still keeping it down.", Check: "gt status no longer marks the agent PAUSED"},

		Entry{Path: "agents", Group: "seats", Use: "Use this when you need the town's live crew sessions listed, or want the verbs that check or switch them."},
		Entry{Path: "agents check", Group: "seats", Use: "Use this when two sessions may be claiming one identity or a lock may outlive the process that took it.", Check: "the command prints All agents healthy"},
		Entry{Path: "agents fix", Group: "seats", Use: "Use this when gt agents check has found stale locks and the dead ones should be cleared for you.", Check: "gt agents fix prints No stale locks found"},
		Entry{Path: "agents list", Group: "seats", Use: "Use this to get the name of each live agent session when you need one to pass to peek, nudge or hook."},
		Entry{Path: "agents menu", Group: "seats", Use: "Use this to jump to another agent's session through a tmux popup instead of typing its session name."},
		Entry{Path: "agents resolve", Group: "seats", Use: "Use this when you need the bead id of a role's current agent, not the id an old note or bead names."},
		Entry{Path: "agents state", Group: "seats", Use: "Use this when an agent bead's labels, such as its idle count or backoff, must be read or changed by hand."},

		Entry{Path: "directive", Group: "orient", Use: "Use this when you need to read, edit or list the standing instructions a role is given."},
		Entry{Path: "directive edit", Group: "host", Use: "Use this when a role's standing instructions are wrong or missing at the rig or central layer and must be changed.", Check: "gt directive show <role> prints the changed text"},
		Entry{Path: "directive list", Group: "orient", Use: "Use this to see every directive file and which one no role ever loads before you edit or trust the set."},
		Entry{Path: "directive show", Group: "orient", Use: "Use this to read the exact instructions your role runs under, with the files they came from."},

		Entry{Path: "handoff", Group: "seats", Use: "Use this when this session should end and a fresh one carry its hooked work, notes and context on.", Check: "the new session primes with the hooked bead"},

		Entry{Path: "hook", Group: "dispatch", Use: "Use this when you need to see what work is on your hook, or to put a bead on it so the work survives a restart."},
		Entry{Path: "hook attach", Group: "dispatch", Use: "Use this when a bead must follow this session through a handoff or a compaction before you start on it.", Check: "gt hook shows the bead"},
		Entry{Path: "hook clear", Group: "dispatch", Use: "Use this when this session should stop working on its hooked bead and leave it for another agent.", Check: "gt hook shows nothing hooked"},
		Entry{Path: "hook detach", Group: "dispatch", Use: "Use this when one named bead must come off a hook and you want the command to refuse if a different bead is there.", Check: "gt hook shows the named bead gone"},
		Entry{Path: "hook show", Group: "dispatch", Use: "Use this when you need to know at a glance what another agent, such as a polecat or the witness, is hooked to."},
		Entry{Path: "hook status", Group: "dispatch", Use: "Use this to see what work your own hook holds and how far along it is."},

		Entry{Path: "model", Group: "host", Use: "Use this to see which model profile each role runs, or to switch one role, the default or the whole table."},
		Entry{Path: "model apply", Group: "host", Use: "Use this when the model table has been edited by hand and every role it names must be written to the live config in one pass.", Check: "gt model show exits 0 with an empty DRIFT column"},
		Entry{Path: "model set-default", Group: "host", Use: "Use this when the profile every role without a row of its own should run must change.", Check: "gt model show exits 0 with an empty DRIFT column"},
		Entry{Path: "model set-role", Group: "host", Use: "Use this when one role's model is wrong and the roles beside it must keep theirs.", Check: "gt model show exits 0 with an empty DRIFT column"},
		Entry{Path: "model show", Group: "host", Use: "Use this to check whether each role runs the model the town's table names, before you blame the model for how a role behaves.", Check: "gt model show exits 0 with an empty DRIFT column"},

		Entry{Path: "sandbox", Group: "sandboxes", Use: "Use this when you need a detail of the profile a seat's sandbox is built from, such as the Forgejo endpoint it reaches."},
		Entry{Path: "sandbox endpoint", Group: "sandboxes", Use: "Use this when a gateway's forgejo-polecat profile must bind the Forgejo host, port and allowed IPs the sandboxes on it reach.", Check: "gt sandbox endpoint --gateway <name> --check <export> prints bound"},

		Entry{Path: "scheduler", Group: "dispatch", Use: "Use this to see and steer the queue that decides which ready bead a polecat is given next."},
		Entry{Path: "scheduler clear", Group: "dispatch", Use: "Use this when a bead must leave the scheduler without being worked, either one named bead or every one of them.", Check: "gt scheduler list no longer lists the cleared beads"},
		Entry{Path: "scheduler list", Group: "dispatch", Use: "Use this to see which beads wait in the scheduler and which of them are blocked before dispatch."},
		Entry{Path: "scheduler pause", Group: "dispatch", Use: "Use this when no new bead should be given to a polecat until you say so.", Check: "gt scheduler status prints State: PAUSED"},
		Entry{Path: "scheduler resume", Group: "dispatch", Use: "Use this when dispatch has been paused and ready work should flow again.", Check: "gt scheduler status prints State: active"},
		Entry{Path: "scheduler run", Group: "dispatch", Use: "Use this to dispatch scheduled beads now, rather than waiting for the daemon's next tick.", Check: "gt scheduler list no longer lists the dispatched beads"},
		Entry{Path: "scheduler status", Group: "dispatch", Use: "Use this to see whether the scheduler is paused and how much polecat room is left before it dispatches more."},
	)
}
