# Rework plan — gt-w9nv86 (attempt 2)

## Finding 1 (major): stopped-container age measured from creation
Acceptance: a container that ran longer than the unused age and stopped a moment ago
is kept; one stopped longer than the age goes.
- Wire: `ciEngineContainer.State.FinishedAt` (docker prints it; zero time for a
  container that never ran, i.e. status `created`). The planner measures a stopped
  container's age from its finish time, falling back to creation only for the zero
  time; a finish time that is unreadable or precedes creation refuses the listing.
- Test: `TestCIEnginePruneMeasuresAStoppedContainersAgeFromItsStop` — red if the
  planner reads creation again (mutate `stoppedAt` back to `created`).
- Refusal: row in `TestCIEnginePruneRefusesAListingItCannotRead` for an unreadable
  FinishedAt.

## Finding 2 (minor): guide still says 24 hours
- `docs/guides/disk-maintenance.md`: three keep-rule ages become `ci_engine_unused_age`.
  The setting's default/floor statement stays gt-fsquaf's (blocked on this bead).

## Finding 3 (minor): integration-tagged test not run
- `go test -tags integration ./internal/daemon -run CIEngine -count=1`, output to the
  bead notes.

## Most likely way this fails in production
Docker's `FinishedAt` for a restarted container names the last stop, so a container
that restarted recently is young; that is the intended conservative reading.
