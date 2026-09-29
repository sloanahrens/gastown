package beads

// Maintenance writes that used to be raw SQL against bd tables (gt-fcxe9.12,
// ADR 0001). Each is one bd verb in machine mode, so bd maintains is_blocked,
// the events journal and its own Dolt commit, and a refusal or partial
// failure comes back as bd's typed exit status. All run against this
// wrapper's own database: callers pin it (NewRigLocal) to the database the
// ids were read from.

// DeleteIssues permanently deletes ids with one "bd delete --force". bd
// removes their labels, comments, events and dependency links in both
// directions. The batch is all-or-nothing: a nil error means every id is
// gone.
func (b *Beads) DeleteIssues(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"delete"}, ids...)
	args = append(args, "--force")
	_, err := b.runMachine(args...)
	return err
}

// DemoteToWisp moves an issue that carries the ephemeral flag but sits in
// the issues table into the wisps table, with its labels, comments, events
// and dependencies ("bd update --ephemeral").
func (b *Beads) DemoteToWisp(id string) error {
	_, err := b.runMachine("update", id, "--ephemeral")
	return err
}

// ReopenUnassigned returns an issue to open with no assignee. It repairs an
// in_progress row whose assignee is NULL or empty, which no claim holds.
func (b *Beads) ReopenUnassigned(id string) error {
	_, err := b.runMachine("update", id, "--status=open", "--assignee=")
	return err
}
