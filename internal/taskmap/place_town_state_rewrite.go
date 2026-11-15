package taskmap

// The entry for gt town state rewrite, which arrived after the P batches were
// cut. It takes host, the group the town-state family records in place_p7d.go
// to place_p7f.go: the town root it rewrites and the volume it moves to are
// infrastructure, and no task group names either.
func init() {
	Register(
		Entry{Path: "town state rewrite", Group: "host", Use: "Use this when a town root or a Forgejo owner has changed and a copied town still names the old ones, to replace them in the config and git pointer files of one directory, writing nothing until --apply.", Check: "a dry run prints each file with its replacement count, and after `--apply` the scan exits 0 naming no file that still holds an old value"},
	)
}
