package taskmap

// Placements for batch P2: the bead verbs, the land verdict, the rig commands
// and spec lint. Each Use is written in the task's words, not the help's.
func init() {
	Register(
		Entry{
			Path:  "bead",
			Group: "orient",
			Use:   "Use this to see which bead write verb fits the job when you are not sure which one to run.",
		},
		Entry{
			Path:  "bead claim",
			Group: "dispatch",
			Use:   "Use this to take a bead for yourself before you start work on it, so no other agent picks it up.",
			Check: "gt hook",
		},
		Entry{
			Path:  "bead close",
			Group: "dispatch",
			Use:   "Use this to close a bead that needs no more work, with the reason it was closed recorded on it.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead comment",
			Group: "dispatch",
			Use:   "Use this to leave a remark on a bead that the next person to open it will read.",
		},
		Entry{
			Path:  "bead create",
			Group: "dispatch",
			Use:   "Use this to file a newly found task as its own bead when it needs a record of its own.",
		},
		Entry{
			Path:  "bead dep",
			Group: "dispatch",
			Use:   "Use this to see or change which beads a bead has to wait for before it can start.",
		},
		Entry{
			Path:  "bead dep add",
			Group: "dispatch",
			Use:   "Use this to make a bead wait for another bead, so it is not dispatched before that work lands.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead dep remove",
			Group: "dispatch",
			Use:   "Use this to drop a wait between two beads when the bead it waited for no longer blocks it.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead note",
			Group: "dispatch",
			Use:   "Use this to record findings on a bead so they survive your session if it ends before you finish.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead reopen",
			Group: "roll",
			Use:   "Use this to bring back a bead that was closed too early or whose fix has been undone.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead reset",
			Group: "dispatch",
			Use:   "Use this to free a bead whose holder is gone, so it can be dispatched to another agent.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead review",
			Group: "review",
			Use:   "Use this to record your overseer verdict on a landed bead, so the attention queue clears or keeps its risk item.",
		},
		Entry{
			Path:  "bead search",
			Group: "orient",
			Use:   "Use this to find out whether a bead already covers a topic in any rig before you file a new one.",
		},
		Entry{
			Path:  "bead tick",
			Group: "dispatch",
			Use:   "Use this to mark one acceptance box done once the work that satisfies it has been verified.",
			Check: "bd show <id>",
		},
		Entry{
			Path:  "bead update",
			Group: "dispatch",
			Use:   "Use this to change a bead's title, priority, labels or acceptance criteria when the work it describes has changed.",
		},
		Entry{
			Path:  "land verdict",
			Group: "review",
			Use:   "Use this to read the om verdict and its findings on a work bead before you decide whether it can land.",
		},
		Entry{
			Path:  "rig",
			Group: "orient",
			Use:   "Use this to find the rig command that fits the job when you are not sure which one applies.",
		},
		Entry{
			Path:  "rig add",
			Group: "host",
			Use:   "Use this to register a project as a new rig by cloning its repository onto this machine.",
		},
		Entry{
			Path:  "rig config",
			Group: "host",
			Use:   "Use this to see or set a rig's configuration values, such as its polecat cap or whether it restarts itself.",
		},
		Entry{
			Path:  "rig config set",
			Group: "host",
			Use:   "Use this to change one configuration value for a rig, such as capping how many polecats work in it at once.",
			Check: "gt rig config show <rig>",
		},
		Entry{
			Path:  "rig config show",
			Group: "host",
			Use:   "Use this to see what a rig's configuration resolves to and which layer each value comes from.",
		},
		Entry{
			Path:  "rig config unset",
			Group: "host",
			Use:   "Use this to clear a rig's local configuration value so the town default applies again.",
			Check: "gt rig config show <rig>",
		},
		Entry{
			Path:  "rig dock",
			Group: "shutdown",
			Use:   "Use this to stop a rig's agents and keep it disabled on every clone until it is undocked.",
		},
		Entry{
			Path:  "rig hold",
			Group: "dispatch",
			Use:   "Use this to stop new automatic dispatches to a rig while its running polecats finish, such as to drain a landing backlog.",
		},
		Entry{
			Path:  "rig list",
			Group: "orient",
			Use:   "Use this to see every rig and whether each one is running, parked or docked.",
		},
		Entry{
			Path:  "rig park",
			Group: "shutdown",
			Use:   "Use this to take a rig out of service, so dispatch and auto-start skip it until it is unparked.",
		},
		Entry{
			Path:  "rig release",
			Group: "dispatch",
			Use:   "Use this to let automatic dispatch reach a rig again once its dispatch hold is no longer needed.",
		},
		Entry{
			Path:  "rig remove",
			Group: "host",
			Use:   "Use this to unregister a rig from the town when it no longer belongs here; its files stay on disk.",
		},
		Entry{
			Path:  "rig reset",
			Group: "seats",
			Use:   "Use this to clear a rig's stale handoff content, stale mail or orphaned in-progress beads once its agents have gone.",
		},
		Entry{
			Path:  "rig settings",
			Group: "host",
			Use:   "Use this to see the settings file that controls a rig's agents, merge queue and crew startup.",
		},
		Entry{
			Path:  "rig settings set",
			Group: "host",
			Use:   "Use this to change one rig setting, such as the agent a rig runs, without editing its settings file by hand.",
			Check: "gt rig settings show <rig>",
		},
		Entry{
			Path:  "rig settings show",
			Group: "host",
			Use:   "Use this to read a rig's full settings file before you change anything in it.",
		},
		Entry{
			Path:  "rig settings unset",
			Group: "host",
			Use:   "Use this to remove one rig setting so its default applies again.",
			Check: "gt rig settings show <rig>",
		},
		Entry{
			Path:  "rig shutdown",
			Group: "shutdown",
			Use:   "Use this to stop all of a rig's polecat sessions after checking them for uncommitted work.",
		},
		Entry{
			Path:  "rig status",
			Group: "orient",
			Use:   "Use this to see one rig's polecats and crew, and the bead each one is on, when you need to know who is doing what.",
		},
		Entry{
			Path:  "rig stop",
			Group: "shutdown",
			Use:   "Use this to stop the polecat sessions of one or more rigs at once.",
		},
		Entry{
			Path:  "rig undock",
			Group: "shutdown",
			Use:   "Use this to let a docked rig's agents start again after it was disabled on every clone.",
		},
		Entry{
			Path:  "rig unpark",
			Group: "shutdown",
			Use:   "Use this to bring a parked rig back into service so dispatch can reach it again.",
		},
		Entry{
			Path:  "spec lint",
			Group: "dispatch",
			Use:   "Use this to check a work bead's shape against the spec template before you dispatch it to a seat.",
		},
	)
}
