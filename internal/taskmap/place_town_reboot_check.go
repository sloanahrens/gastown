package taskmap

// Placement for gt town reboot-check (gt-9lciim). Group judgement: the command checks the
// Mac the town runs on after a reboot, so it takes host, as gt town vms does.
func init() {
	Register(
		Entry{Path: "town reboot-check", Group: "host", Use: "Use this when the Mac has rebooted and you need to see what the reboot left broken, to read one line per check from the operations doc with the repair for each failing one.", Check: "every printed line starts ok; the command exits 0 whatever the rows say"},
	)
}
