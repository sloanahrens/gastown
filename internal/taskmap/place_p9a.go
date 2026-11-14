package taskmap

// Placements for batch P9, half A (gt-j669b4.13): gt mail through gt mail group
// create. Sending goes with nudge in dispatch, and a mail group and a queue
// claim are both ways of giving work to an agent. Reading mail, a channel and
// the address directory is orient, as is closing out a handled message, where
// attention ack and escalate close already sit. Inbox and channel
// administration that no task group covers, and the hook helper, sit in
// internal.
func init() {
	Register(
		Entry{Path: "mail", Group: "dispatch", Use: "Use this when you need to reach another agent by message, or to read what other agents have sent you."},
		Entry{Path: "mail announces", Group: "orient", Use: "Use this to read the town's announce channels, where broadcasts addressed to a role are kept.", Check: "the printed list names each channel and its readers"},
		Entry{Path: "mail archive", Group: "orient", Use: "Use this when a message you have handled should leave your inbox while staying readable in the store.", Check: "the command prints Message archived"},
		Entry{Path: "mail channel", Group: "orient", Use: "Use this to list the broadcast channels, or to read the messages waiting on one named channel."},
		Entry{Path: "mail channel create", Group: "internal", Use: "Use this when the town needs a new broadcast channel, with the retention policy its messages should have.", Check: "gt mail channel list shows the new channel"},
		Entry{Path: "mail channel delete", Group: "internal", Use: "Use this when a broadcast channel is done and its configuration should be removed.", Check: "gt mail channel list no longer shows the channel"},
		Entry{Path: "mail channel list", Group: "orient", Use: "Use this to see which broadcast channels exist and how long each keeps its messages.", Check: "the printed table lists each channel"},
		Entry{Path: "mail channel show", Group: "orient", Use: "Use this to read one broadcast channel's retention policy and its messages.", Check: "the printed output names the channel and lists its messages"},
		Entry{Path: "mail channel subscribe", Group: "orient", Use: "Use this when you want to receive the messages broadcast on a channel.", Check: "gt mail channel subscribers <name> lists you"},
		Entry{Path: "mail channel subscribers", Group: "orient", Use: "Use this to see who receives a channel's broadcasts before you rely on it reaching them.", Check: "the printed list names each subscriber"},
		Entry{Path: "mail channel unsubscribe", Group: "orient", Use: "Use this when a channel's broadcasts are no longer yours to read.", Check: "gt mail channel subscribers <name> no longer lists you"},
		Entry{Path: "mail check", Group: "internal", Use: "Use this in a Claude Code hook to learn whether new mail is waiting without blocking the session.", Check: "the exit code is 0 when mail is waiting and 1 when it is not"},
		Entry{Path: "mail claim", Group: "dispatch", Use: "Use this when work is waiting in a mail queue and you want the oldest message nobody else has claimed.", Check: "the command prints Claimed message, with the message's id and sender"},
		Entry{Path: "mail clear", Group: "internal", Use: "Use this when a worker's inbox must be emptied outright, as when resetting the town to quiescence.", Check: "the command prints Cleared and the number of messages"},
		Entry{Path: "mail delete", Group: "orient", Use: "Use this when a message is answered and should be acknowledged out of your inbox.", Check: "the command prints Message deleted"},
		Entry{Path: "mail directory", Group: "orient", Use: "Use this to find the address to write to: agents, groups, queues, channels and the special addresses.", Check: "the printed table lists each address with its type"},
		Entry{Path: "mail drain", Group: "internal", Use: "Use this when routine protocol messages have piled up in an inbox and can be archived in bulk once stale.", Check: "gt mail drain --dry-run lists no drainable messages"},
		Entry{Path: "mail group", Group: "dispatch", Use: "Use this to keep named sets of mail recipients so one message can reach all of them."},
		Entry{Path: "mail group add", Group: "dispatch", Use: "Use this when another address or group should join an existing mail group.", Check: "gt mail group show <name> lists the new member"},
		Entry{Path: "mail group create", Group: "dispatch", Use: "Use this when several agents should be reachable by one group name.", Check: "gt mail group list shows the new group"},
	)
}
