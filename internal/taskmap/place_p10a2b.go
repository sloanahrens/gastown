package taskmap

// Placements for batch P10, half A2 part 2 (gt-8k2qlf): the default-agent list
// and the config get, migrate and secrets commands. Each reads or changes a
// town setting or the token store on this machine, so each sits in host beside
// the rest of gt config (place_p10a1.go, place_p10a2.go).
//
// Group judgement: config migrate and config secrets stay in host, not dolt or
// roll. They move the town's own settings files and the agent token store; dolt
// is the beads database and roll undoes or replays a landing.
func init() {
	Register(
		Entry{Path: "config default-agent list", Group: "host", Use: "Use this to see every agent the town can start, and which one a rig gets when it names none, before you change the default.", Check: "the printed list names each agent and ends with Default: <name>"},
		Entry{Path: "config get", Group: "host", Use: "Use this when one town setting must be read by its dot-notation key before you change it or rely on its value.", Check: "the command prints the value alone on one line, and an unknown key is an error that lists the supported keys"},
		Entry{Path: "config migrate", Group: "host", Use: "Use this when the town still keeps its config in five files: run it once to move the config onto mayor/town.json and settings/config.json before you edit either file by hand.", Check: "the command prints Migrated the town config to two files, then a Verify line naming gt config validate; with --dry-run it prints Would migrate"},
		Entry{Path: "config secrets", Group: "host", Use: "Use this to see how agent tokens are kept out of settings/config.json, with each token held in settings/daemon.env and named in the config, before you add a token to an agent's env block.", Check: "gt config secrets --help lists migrate among its subcommands"},
		Entry{Path: "config secrets migrate", Group: "host", Use: "Use this when settings/config.json still holds a literal agent token, to move each token out of the config after a dry run shows what would move.", Check: "the command prints Moved N literal token(s) from settings/config.json, or No literal tokens in settings/config.json when there are none"},
	)
}
