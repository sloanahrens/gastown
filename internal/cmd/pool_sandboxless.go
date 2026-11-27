package cmd

// seatRunsNothing reports whether a seat's own read found nothing held and
// nobody working: a measured sessionless disposition (DeadRead), no live work
// in flight and an idle agent bead. A read that errored is a seat the pool
// cannot see, not one it may free, so it answers false (gt-n54w0f).
func seatRunsNothing(state poolSeatState, err error) bool {
	return err == nil && state.DeadRead && !state.HookHeld && !state.AgentWorking
}
