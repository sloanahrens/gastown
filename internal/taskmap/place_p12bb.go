package taskmap

// Placements for batch P12, half B part 2 (gt-ijo6p4): gt plugin sync, and the
// three gt ring paths, the namespace itself and its forge and remove verbs.
//
// Group judgement: plugin sync sits in dispatch beside the plugin commands
// place_p12ba.go places and beside formula sync, because a plugin is periodic
// work the town hands out and a sync only brings the on-disk plugins up to the
// checkout that defines them. The ring paths take host: the Ring is a signed
// record in mayor/town.json, verified under a key settings/config.json names,
// so forging, removing and reading it change the machine-owned town record as
// config set and rig config do. dispatch was rejected because no verb hands out
// work, though a waned Ring holds the dispatcher; shutdown because no verb
// stops a session or the town; internal because the One runs these by hand.
func init() {
	Register(
		Entry{Path: "plugin sync", Group: "dispatch", Use: "Use this when a plugin change has landed and the runtime plugins the town's agents read must catch up, keeping a hand-edited runtime copy and naming it unless you pass --force.", Check: "gt plugin sync --dry-run prints All plugins up to date"},
		Entry{Path: "ring", Group: "host", Use: "Use this when the town's automatic work is held or its lease needs replacing, to reach the verbs that read, forge, renew and remove the signed Ring."},
		Entry{Path: "ring forge", Group: "host", Use: "Use this when the town has no valid Ring or the last one has waned and automatic work must start again, to sign a new lease valid for 36 hours.", Check: "gt ring status prints ring: valid and exits 0"},
		Entry{Path: "ring remove", Group: "host", Use: "Use this when the town should carry no signed lease, to delete the Ring from mayor/town.json.", Check: "gt ring status prints ring: absent and exits 3"},
	)
}
