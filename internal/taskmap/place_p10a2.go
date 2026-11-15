package taskmap

// Placements for batch P10, half A2 (gt-0pmzkx): the rest of gt config. These
// read or change the town's settings, so they sit in host beside the rest of
// gt config (place_p10a1.go) and the gt daemon commands (place_p8.go).
//
// Group judgement: config cost-tier sets the town's cost-optimization tier,
// which picks a model for each role, so it sits in cost, where an agent looking
// to see or limit what the town spends finds it beside the usage commands. The
// gt model commands that edit one role's model table belong to host
// (place_p4.go); cost-tier is the spending preset, not that per-role table.
func init() {
	Register(
		Entry{Path: "config agent set", Group: "host", Use: "Use this when the town must offer an agent it does not have built in, or a built-in agent's command must be overridden, by giving the agent a name, a command and its arguments.", Check: "the command prints Agent '<name>' set to: <command line>"},
		Entry{Path: "config agent-email-domain", Group: "host", Use: "Use this to see or change the domain gt puts in the commit email it builds from an agent's address, so a commit is attributed to the place the agent runs.", Check: "the command prints Agent email domain set to '<domain>' when it changes the setting"},
		Entry{Path: "config cost-tier", Group: "cost", Use: "Use this when the town's spend must be cut or restored to full quality by changing the model tier every role runs, rather than switching one role at a time.", Check: "the command prints Cost tier set to <tier> above the role assignments it applied"},
		Entry{Path: "config daemon", Group: "host", Use: "Use this when settings/config.json must be read or changed the way the daemon loads it, so a change the daemon would refuse is refused before it is saved.", Check: "a write prints a unified diff of the change and leaves a config.json.bak-<timestamp>-<label> backup"},
	)
}
