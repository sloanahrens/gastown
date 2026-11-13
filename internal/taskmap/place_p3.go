package taskmap

// Placements for batch P3: the Jev batteries and their readers, the metrics
// pipeline, the polecat seat caps, and the sling, nudge, peek and spec commands.
func init() {
	Register(
		Entry{Path: "jev", Group: "review", Use: "Use this to read what the Jev batteries answered and what they cost, from the logs they write."},
		Entry{Path: "jev attention", Group: "orient", Use: "Use this to have Jev triage each newly raised item: whether a person must act on it or it will clear on its own."},
		Entry{Path: "jev conflict-report", Group: "review", Use: "Use this to see how many bead pairs Jev's same-files answers would have warned about, per day and cut."},
		Entry{Path: "jev dataset", Group: "review", Use: "Use this when you need Jev's labeled answers as a file for the mango analysis, with no prompt or passage text in it."},
		Entry{Path: "jev dedupe", Group: "review", Use: "Use this to check whether a newly filed finding repeats a bead that already exists."},
		Entry{Path: "jev dispatch", Group: "review", Use: "Use this to see how Jev predicts each bead placed on a seat will land, and read the latest predictions."},
		Entry{Path: "jev health", Group: "orient", Use: "Use this when you need to know whether the Jev batteries are running and still writing their logs."},
		Entry{Path: "jev landing", Group: "review", Use: "Use this to see Jev's second opinion on each landing that touched risk paths."},
		Entry{Path: "jev noise", Group: "review", Use: "Use this to measure how much a battery's answers change when the same state is asked again, so you know its noise floor."},
		Entry{Path: "jev requests", Group: "review", Use: "Use this when a Jev call failed or you need proof of what a battery sent, without its text."},
		Entry{Path: "jev review-targets", Group: "review", Use: "Use this to have Jev score the files in a deep-review rotation slice so the riskiest are reviewed first."},
		Entry{Path: "jev scorecard", Group: "review", Use: "Use this to see how well a battery's answers agree with its labels, with precision, recall and a verdict."},
		Entry{Path: "jev scrubcheck", Group: "review", Use: "Use this to see how much scrubbing the state changes Jev's answers, to judge whether the scrub costs signal."},
		Entry{Path: "jev spend", Group: "cost", Use: "Use this when you need to know what Jev calls cost over a recent window, by caller."},
		Entry{Path: "jev submission", Group: "review", Use: "Use this to get Jev's answers on each branch submitted for landing, before it lands."},

		Entry{Path: "metrics", Group: "host", Use: "Use this to find which gt commands are used, how often and by whom, before pruning dead ones."},
		Entry{Path: "metrics all", Group: "host", Use: "Use this to refresh the whole metrics digest in one run: extract, mango, backtest and report."},
		Entry{Path: "metrics analyse", Group: "host", Use: "Use this when the town-metrics panel's last analysis is more than a day old."},
		Entry{Path: "metrics backtest", Group: "orient", Use: "Use this to check how the weekly metrics line up against the regime events."},
		Entry{Path: "metrics extract", Group: "host", Use: "Use this when today's landing, weekly and bead tables must be rewritten before a digest or analysis reads them."},
		Entry{Path: "metrics import-versions", Group: "host", Use: "Use this when the archive's first-filed bead texts must be loaded into the version store."},
		Entry{Path: "metrics mango", Group: "orient", Use: "Use this when you want today's mango findings written beside the metrics tables."},
		Entry{Path: "metrics questions", Group: "orient", Use: "Use this to see where the town stands on the ten questions about how it behaves, from its own files."},
		Entry{Path: "metrics repeats", Group: "review", Use: "Use this to find om findings that restate a finding from an earlier review round."},
		Entry{Path: "metrics report", Group: "orient", Use: "Use this when you need the dated town metrics digest written into the docs directory."},
		Entry{Path: "metrics run", Group: "host", Use: "Use this to run one town-metrics hour by hand, from snapshot through analysis."},
		Entry{Path: "metrics shape-outcome", Group: "review", Use: "Use this to see whether a bead's first-filed shape predicts its review rounds and whether it is approved."},
		Entry{Path: "metrics snapshot", Group: "host", Use: "Use this to add one town snapshot to the metrics log now."},
		Entry{Path: "metrics storypoints", Group: "orient", Use: "Use this to read today's story-points shadow report from the landings table."},
		Entry{Path: "metrics storypoints-train", Group: "host", Use: "Use this to refit the story-points weights from the landings table, writing a proposed file and a report for a person to adopt."},

		Entry{Path: "nudge", Group: "dispatch", Use: "Use this to send a polecat or crew session a message that it sees in its live session."},
		Entry{Path: "peek", Group: "seats", Use: "Use this to see what a polecat or crew session is doing now, from its recent terminal output."},
		Entry{Path: "pool", Group: "seats", Use: "Use this to see or change how many polecats each agent seat may run."},
		Entry{Path: "pool migrate", Group: "seats", Use: "Use this when the town config still holds the two-seat keys and the daemon will not start."},
		Entry{Path: "pool mix", Group: "seats", Use: "Use this only when an old note says to mix seats: the command is retired and prints the gt pool set line that replaces it."},
		Entry{Path: "pool set", Group: "seats", Use: "Use this to set the cap of one or more named seats and leave the other seats as they are.", Check: "gt pool show"},
		Entry{Path: "pool set-total", Group: "seats", Use: "Use this to cap how many polecats run across every seat at once, beside the per-agent caps."},
		Entry{Path: "pool show", Group: "seats", Use: "Use this to see each seat's cap, how many polecats it holds, and whether it is open or closed."},
		Entry{Path: "sling", Group: "dispatch", Use: "Use this to give a bead or formula to an agent, a crew member or a polecat, so it starts working on it at once."},
		Entry{Path: "sling respawn-reset", Group: "dispatch", Use: "Use this when a bead has hit the respawn limit and you have found the cause, so it can be slung again."},
		Entry{Path: "unsling", Group: "dispatch", Use: "Use this to take a bead off an agent's hook, so the agent stops working on it.", Check: "gt hook"},
		Entry{Path: "spec", Group: "dispatch", Use: "Use this to lint a spec bead's shape against the template or to run the spec dispatcher by hand."},
		Entry{Path: "spec dispatch", Group: "dispatch", Use: "Use this to run one spec-dispatch tick now, rather than waiting for the daemon's next pass."},
		Entry{Path: "spec estimate", Group: "dispatch", Use: "Use this to estimate a spec bead's size before you dispatch it, so its story points are on the bead."},
	)
}
