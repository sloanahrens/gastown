# Stray worktree cleanup

The build's worktree is a detached one on the rig repo's shared `.repo.git`. No
cleanup path treats it as a stray: the reaper and `heldByOtherWorktree` only
`git worktree prune` registrations whose directory is gone (the directory exists
for the whole build), they match worktrees by branch name (a detached HEAD has
none), and the local-branch prune reads the list only to protect branches.
