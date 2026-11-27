package taskmap

// Placement for gt town cluster credentials (gt-tfdewi): the command that has
// sbxcreds issue a deployer's kubeconfig for one role on the gt-sbx cluster. It
// sits in host, beside the other commands that inspect or change the machine.
func init() {
	Register(
		Entry{Path: "town cluster credentials", Group: "host", Use: "Use this when a deployer needs a kubeconfig for one role on the gt-sbx cluster, to have sbxcreds issue it and write it to --out.", Check: "gt town cluster credentials --help lists the --cluster, --role and --out flags"},
	)
}
