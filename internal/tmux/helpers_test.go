package tmux

import "github.com/jonboulle/clockwork"

// newTmuxForTest builds a Tmux whose processes and clock are supplied by the
// test. A nil ex or clk falls back to the real one.
func newTmuxForTest(socket string, ex execFunc, clk clockwork.Clock) *Tmux {
	return &Tmux{socketName: socket, exec: ex, clock: clk}
}
