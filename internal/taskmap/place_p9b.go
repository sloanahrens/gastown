package taskmap

// Placements for batch P9, half B (gt-j669b4.20): the rest of gt mail, from
// group delete through thread. Sending a message and attaching one to a hook
// give work to an agent, so they sit in dispatch beside nudge and hook attach,
// and so do the group and queue commands a sender uses to choose recipients
// and to hand work out. Releasing a claim hands the work back for another
// agent, as unsling does. Reading, searching, marking and previewing a
// message orient you to what was sent, which is where the first half put
// inbox, read and delete.
func init() {
	Register(
		Entry{Path: "mail group delete", Group: "dispatch", Use: "Use this when a mail group should stop existing and a message to its name should no longer reach anyone.", Check: "gt mail group list no longer shows the group"},
		Entry{Path: "mail group list", Group: "dispatch", Use: "Use this to see which mail groups exist before you address a message to one.", Check: "the printed list names each group"},
		Entry{Path: "mail group remove", Group: "dispatch", Use: "Use this when an address should stop receiving what is sent to a group.", Check: "gt mail group show <name> no longer lists the member"},
		Entry{Path: "mail group show", Group: "dispatch", Use: "Use this to see who a group's mail reaches before you rely on it reaching them.", Check: "the printed output lists each member"},
		Entry{Path: "mail hook", Group: "dispatch", Use: "Use this when a mail message should become the work this session resumes after a handoff or a restart.", Check: "the command prints Work attached to hook"},
		Entry{Path: "mail inbox", Group: "orient", Use: "Use this to see what mail is waiting for you and how much of it you have not read.", Check: "the printed header counts the messages and the unread ones"},
		Entry{Path: "mail mark-read", Group: "orient", Use: "Use this when a message has been handled and should stop counting as unread without leaving your inbox.", Check: "gt mail inbox shows the message as read"},
		Entry{Path: "mail mark-unread", Group: "orient", Use: "Use this when a message still needs attention and should be shown as unread again.", Check: "gt mail inbox shows the message as unread"},
		Entry{Path: "mail peek", Group: "orient", Use: "Use this to glance at the first unread message without opening it or marking it read.", Check: "the printed preview names the message's subject and sender"},
		Entry{Path: "mail queue", Group: "dispatch", Use: "Use this to hand work to whichever eligible agent claims it first, and to manage those queues."},
		Entry{Path: "mail queue create", Group: "dispatch", Use: "Use this when work should wait for the first eligible agent to claim it instead of going to a named one.", Check: "gt mail queue list shows the new queue"},
		Entry{Path: "mail queue delete", Group: "dispatch", Use: "Use this when a work queue is finished and should stop accepting claims.", Check: "gt mail queue list no longer shows the queue"},
		Entry{Path: "mail queue list", Group: "dispatch", Use: "Use this to see which work queues exist, who may claim from each, and whether each is active.", Check: "the printed list names each queue with its claimers"},
		Entry{Path: "mail queue show", Group: "dispatch", Use: "Use this to check how much work is waiting in a queue, claimed, or done.", Check: "the printed counts name the available and in-progress messages"},
		Entry{Path: "mail read", Group: "orient", Use: "Use this to open a message you were sent, which marks it read without removing it.", Check: "the printed header names the message's subject and sender"},
		Entry{Path: "mail release", Group: "dispatch", Use: "Use this when you cannot finish a claimed queue message and another agent should be able to take it.", Check: "the command prints Released message back to queue"},
		Entry{Path: "mail reply", Group: "dispatch", Use: "Use this when the sender of a message should get your answer in the same thread.", Check: "the command prints Reply sent to, naming the sender"},
		Entry{Path: "mail search", Group: "orient", Use: "Use this to find a message by its text when you no longer know which one holds it.", Check: "the printed count and matches follow the query"},
		Entry{Path: "mail send", Group: "dispatch", Use: "Use this when another agent, or a whole group of them, must receive a message you can address by name.", Check: "the command prints Message sent to, naming the address"},
		Entry{Path: "mail thread", Group: "orient", Use: "Use this to read a whole exchange in order when a message is one turn of a longer conversation.", Check: "the printed thread lists every message in order"},
	)
}
