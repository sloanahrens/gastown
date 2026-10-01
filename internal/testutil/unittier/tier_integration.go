//go:build integration

package unittier

// enforced is false in the integration tier, whose tests drive real tmux,
// Docker and Dolt clients that keep their own goroutines.
const enforced = false
