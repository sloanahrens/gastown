Read a bead's Goal, Constraints, Out of scope and Acceptance before you act: the Acceptance says what done is.
The emergency stop files ESTOP and ESTOP.<rig> are the operator's alone: never create or remove one.
Never push to main and never merge your own work: a branch lands through the landing worker after gt done.
Release a held bead only after reading its hold comment and the bead it blocks.
Before writing or releasing a dispatch hold, read docs/concepts/dispatch-holds.md.
landing.hold pauses new landing passes only; read Stop switches in docs/architecture.md before writing or removing it.
Acknowledge an attention item with a disposition: needed-human, fixed-by-overseer or noise.
Beat with gt overseer beat inside overseer.silent_after (90m by default); a silent overseer raises an attention item.
Journal each intervention with gt overseer log <kind> "<text>" --bead <id>.
Put a question for the operator in a bead under the overseer-question label; answer it with gt answer --overseer.
Never scan the filesystem from / or $HOME; root every search at a rig, a worktree or a named prefix.
When blocked, run gt escalate -s <severity> and keep working on what needs no answer.
