package taskmap

// The entries for gt town deployer. The credential is made on the host by the
// deployer user, so the command sits in host with the other town commands.
func init() {
	Register(
		Entry{Path: "town deployer", Group: "host", Use: "Use this to find the host-side steps that make the deployer's cluster credential as the deployer user, the break-glass way into the control cluster."},
		Entry{Path: "town deployer csr", Group: "host", Use: "Use this when the deployer user needs a client key and a CertificateSigningRequest for the break-glass operator to sign, to write the key into the verified credential directory and print the request.", Check: "the key lands 0600 at <dir>/<name>.key and the printed YAML names the identity; the command refuses inside a pod or as a user other than --as-user"},
		Entry{Path: "town deployer request", Group: "host", Use: "Use this to ask the deployer for a plan, an apply or a rollback: it writes one request file into the deployer's inbox and prints the id and the answers path.", Check: "the file is <inbox>/<id>.json and parses with the inbox schema; a kind other than plan, apply or rollback, a plan name that is not a plan directory name, or a request id already in the inbox is refused, and the command refuses inside a pod"},
		Entry{Path: "town deployer admin-check", Group: "host", Use: "Use this to find a kubeconfig context that reaches the control cluster with an embedded credential, once the deployer works and the admin entry is to come out.", Check: "each result line names only a context and a user; the command exits 1 on a finding or a kubeconfig it could not read, and refuses inside a pod"},
	)
}
