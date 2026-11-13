package taskmap

// The P1 batch of task map entries: the commands that gt attention through
// gt version. Each Use names the situation an agent is in, not what the
// command does inside.
func init() {
	Register(
		Entry{Path: "attention", Group: "orient", Use: "Use this to see which conditions are waiting on a person's or the overseer's judgment, most pressing first."},
		Entry{Path: "attention ack", Group: "orient", Use: "Use this when you have dealt with an attention item, or judged it noise, and want it hidden until its condition clears.", Check: "gt attention no longer lists the acked key"},
		Entry{Path: "cloud-check", Group: "review", Use: "Use this to see, after a deploy, whether each app still answers its health URL.", Check: "gt cloud-check exits 0"},
		Entry{Path: "dashboard", Group: "orient", Use: "Use this when you want the whole town on one page in a browser, rather than in a terminal."},
		Entry{Path: "doctor", Group: "orient", Use: "Use this when something in the town seems wrong and you need to know which check is failing, before you change anything."},
		Entry{Path: "doctor fix", Group: "host", Use: "Use this when doctor has named one failing check and that one check needs repairing, not the whole town.", Check: "gt doctor shows the check passing"},
		Entry{Path: "escalate", Group: "orient", Use: "Use this when you are blocked on a decision or a fault that a person must handle before you can go on.", Check: "gt escalate list shows the new escalation"},
		Entry{Path: "escalate ack", Group: "orient", Use: "Use this when you have picked up an escalation and are working on it, so the stale-escalation warnings stop."},
		Entry{Path: "escalate clear", Group: "orient", Use: "Use this when the condition behind a keyed alert has gone, so the alert's escalation stops waiting for someone."},
		Entry{Path: "escalate close", Group: "orient", Use: "Use this when an escalation's issue is resolved and you want the resolution recorded against it.", Check: "gt escalate list no longer shows it"},
		Entry{Path: "escalate list", Group: "orient", Use: "Use this to see which escalations are still open and waiting for someone."},
		Entry{Path: "escalate show", Group: "orient", Use: "Use this to read one escalation's details before you ack, close or act on it."},
		Entry{Path: "escalate stale", Group: "orient", Use: "Use this to find escalations nobody has acknowledged in time, so they can be raised to a higher severity.", Check: "gt escalate stale --dry-run lists none"},
		Entry{Path: "gate", Group: "review", Use: "Use this when you are weighing a selective landing gate, before you rely on it to test the packages your landings touch."},
		Entry{Path: "gate model", Group: "review", Use: "Use this when you want the per-landing breakdown of what a selective gate would have run and how long it would have taken."},
		// heartbeat reports a session's own state, which no other group covers,
		// so it sits under seats with the sessions it describes; its Use names
		// the restart the report prevents, which is why an agent runs it
		// (gt-j669b4.2).
		Entry{Path: "heartbeat", Group: "seats", Use: "Use this to tell the daemon whether this session is working, idle, exiting or stuck, so a busy session is not restarted."},
		Entry{Path: "history", Group: "dolt", Use: "Use this when a bead's history shows a field that never changed, to check whether the record was cut off before it began."},
		Entry{Path: "intervention", Group: "review", Use: "Use this to see or record the times a person or the overseer had to step in where the loop could not."},
		Entry{Path: "intervention record", Group: "review", Use: "Use this to log why, right after you step in on a bead the loop could not handle, so the intervention is counted."},
		Entry{Path: "log", Group: "orient", Use: "Use this to see what the town's agents have recently done, such as spawns, handoffs, nudges and crashes, in order."},
		Entry{Path: "log crash", Group: "internal", Use: "Use this when an agent's tmux pane exits, to record how the session ended."},
		Entry{Path: "log prune-worktree", Group: "internal", Use: "Use this when you have removed an orphaned worktree, so the removal shows in the town's log."},
		Entry{Path: "overseer", Group: "seats", Use: "Use this when you need to report on or record the overseer session's own state and journal."},
		Entry{Path: "overseer beat", Group: "seats", Use: "Use this to show the overseer is alive and say whether it is working, idle or away."},
		Entry{Path: "overseer log", Group: "seats", Use: "Use this to journal one overseer intervention, such as a hold, a fix by hand or an answer, with the bead it concerned."},
		Entry{Path: "prime", Group: "orient", Use: "Use this when you start, resume or lose context in a session and need your role and the next step."},
		Entry{Path: "questions", Group: "orient", Use: "Use this to see which overseer questions are waiting for an answer from the operator, most urgent first."},
		Entry{Path: "ready", Group: "dispatch", Use: "Use this to find work that has no blockers and can be picked up now, most important first."},
		Entry{Path: "regime", Group: "review", Use: "Use this when a change to the install, a directive, a gate, the seat model or a pool needs marking in the metrics record."},
		Entry{Path: "regime backfill", Group: "review", Use: "Use this to record, once, the install history and the changes made before regime recording began.", Check: "gt regime check reads the backfilled changes"},
		Entry{Path: "regime check", Group: "review", Use: "Use this to confirm the town's regime-change record reads cleanly before you trust the metrics built on it."},
		Entry{Path: "regime record", Group: "review", Use: "Use this when you have just changed the install, a directive, a gate, the seat model or a pool, so the metrics mark it."},
		Entry{Path: "repoguard", Group: "review", Use: "Use this when a change touches a repo's scripts, to check that each script is allowlisted with a reason it cannot be Go."},
		Entry{Path: "report", Group: "orient", Use: "Use this to get a plain-text summary of what the town did in one hour, from its own ledgers."},
		Entry{Path: "report publish", Group: "birds", Use: "Use this when a bird agent has written its daily report and the page and sidecar must be published for the dashboard."},
		Entry{Path: "show", Group: "orient", Use: "Use this to read a bead's full details by its id, whatever rig or town it lives in."},
		Entry{Path: "stale", Group: "host", Use: "Use this to check whether the gt binary on this machine was built before the latest build ref, before you trust its behavior."},
		Entry{Path: "status", Group: "orient", Use: "Use this to see whether the town is healthy and what its rigs, polecats and crew are doing.", Check: "gt status --line exits 0 when the town is green"},
		Entry{Path: "tail", Group: "orient", Use: "Use this to watch what the town is doing as one time-ordered stream of landings, dispatches, escalations and daemon events."},
		Entry{Path: "usage", Group: "cost", Use: "Use this to see or record how much of the Claude subscription the town has used."},
		Entry{Path: "usage attrib", Group: "cost", Use: "Use this to see which beads the town's Claude usage was spent on, one record per bead."},
		Entry{Path: "usage attrib scan", Group: "cost", Use: "Use this to bring each bead's usage record up to date from the seat transcripts and the events, before you read its token totals."},
		Entry{Path: "usage attrib show", Group: "cost", Use: "Use this to read one bead's token totals, models and estimated cost from its usage record."},
		Entry{Path: "usage report", Group: "cost", Use: "Use this to see how much Claude and DeepSeek usage each role, model, bead or day spent over the last days, from the usage records."},
		Entry{Path: "usage sample", Group: "internal", Use: "Use this when the status line hook passes a rate-limit reading, to record a plan usage sample."},
		Entry{Path: "version", Group: "host", Use: "Use this to confirm which gt version and build this machine is running, before you report a bug.", Check: "gt version --short prints the version"},
	)
}
