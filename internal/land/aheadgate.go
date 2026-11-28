package land

// AheadGate is one candidate the landing worker gated ahead of its turn: the
// bead, head and base it was built on, the merged commit, and the gate's
// verdict. It is held in memory only (gt-6ivh2m).
type AheadGate struct {
	Bead, Head, Base, Merged string
	Result                   CandidateResult
}
