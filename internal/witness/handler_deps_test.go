package witness

// newTestHandlers returns a handlers value a single test owns. Every
// collaborator starts real — and the destructive ones (session restart, tmux
// kill, `gt polecat nuke`) panic under a test binary (gt-5itbt) — so a test
// fakes exactly the fields its path reaches, on its own instance. Nothing is
// swapped process-wide, which is what lets these tests run in parallel.
func newTestHandlers() *handlers { return &handlers{} }
