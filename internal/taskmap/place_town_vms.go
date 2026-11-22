package taskmap

// Placement for gt town vms, the reboot recovery (gt-czoguq). Group
// judgement: the command starts the Mac's Colima VMs, so both entries take
// host, the group for the machine the town runs on, as place_p7c.go records
// for town and town cluster; sandboxes covers running work in an isolated
// environment, not the VMs themselves.
func init() {
	Register(
		Entry{Path: "town vms", Group: "host", Use: "Use this when the Mac has rebooted and the Colima VMs the town runs on must come back, to find the step that starts them."},
		Entry{Path: "town vms plist", Group: "host", Use: "Use this when the Mac's colima-vms launchd job has to be written or replaced, to print the plist that runs gt town vms start at load and hand it to the apply step.", Check: "the printed plist is well-formed XML naming the gt binary and the home you gave"},
		Entry{Path: "town vms start", Group: "host", Use: "Use this when the Mac has rebooted and both Colima VMs, the sandbox containers, the VMs' DNS and the registry forward have to come back, to run the recovery and read the step lines it prints.", Check: "every printed step line ends ok and the command exits 0"},
	)
}
