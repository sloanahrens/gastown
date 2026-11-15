package taskmap

// Placements for batch P10, half B, part 3 (gt-5fgs9b): five tap guard
// commands that refuse a tool call before it runs. They go in internal, the
// same as the other tap handlers (place_p10bb.go), because a PreToolUse or
// PermissionRequest hook calls them and an agent never runs them by hand.
//
// Group judgement: none. Every one is a hook handler, so all five sit in
// internal even where the policy is about a task (a question, a PR, a write
// outside the worktree) rather than plumbing.
func init() {
	Register(
		Entry{Path: "tap guard permission-request", Group: "internal", Use: "Use this when an unattended polecat or dog session hits a permission prompt that no one can answer, so the prompt is denied and the session fails instead of parking."},
		Entry{Path: "tap guard polecat-paths", Group: "internal", Use: "Use this when a polecat's edit or shell command would write outside its own worktree, so the write is stopped before it can corrupt a sibling's branch."},
		Entry{Path: "tap guard pr-workflow", Group: "internal", Use: "Use this when an agent tries to open a pull request or start a feature branch, since Gas Town workers push straight to main."},
		Entry{Path: "tap guard question-tool", Group: "internal", Use: "Use this when an unattended session tries to ask the user a question, which is stopped so the session decides or escalates instead of parking on a dialog no one can answer."},
		Entry{Path: "tap guard sandbox-secrets", Group: "internal", Use: "Use this when a command in a sandbox session would print the environment or a secret value, so the command is stopped before it runs."},
	)
}
