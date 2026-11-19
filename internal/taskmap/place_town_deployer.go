package taskmap

// The entries for gt town deployer. The credential is made on the host by the
// deployer user, so the command sits in host with the other town commands.
func init() {
	Register(
		Entry{Path: "town deployer", Group: "host", Use: "Use this to find the host-side steps that make the deployer's cluster credential as the deployer user, the break-glass way into the control cluster."},
		Entry{Path: "town deployer csr", Group: "host", Use: "Use this when the deployer user needs a client key and a CertificateSigningRequest for the break-glass operator to sign, to write the key into the verified credential directory and print the request.", Check: "the key lands 0600 at <dir>/<name>.key and the printed YAML names the identity; the command refuses inside a pod or as a user other than --as-user"},
	)
}
