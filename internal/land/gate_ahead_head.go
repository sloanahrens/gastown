package land

// gateAheadHeadCheck is the head half of the read-only checks that say a
// submission may be gated ahead: it fetches the target and asserts the head
// is on the pushed branch, returning an error naming a failed fetch, a failed
// check or a rejection, and writing nothing. The target fetch takes gitMu as
// Land's does; checkHeadPushed takes it again for its branch fetch, because
// withGitMu is not reentrant.
func (l *Lander) gateAheadHeadCheck(g Repo, w Work) error {
	if err := l.withGitMu(func() error { return l.fetchTarget(g, w.Target) }); err != nil {
		return &InfraError{Stage: "fetch target", Err: err}
	}
	rej, err := l.checkHeadPushed(g, w)
	if err != nil {
		return err
	}
	if rej != nil {
		return rej
	}
	return nil
}
