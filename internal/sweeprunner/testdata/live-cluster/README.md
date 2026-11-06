# Live-cluster golden listings

Captured 2026-11-05 from the control cluster (k3s on the gt-control VM): they are what the API
printed, stripped of env, volumes, images, annotations and tokens, with every other field kept
as the API prints it. They exist because the guards' tests once used fabricated shapes and so
passed against listings the real API never prints.

- `capacity-listing.json`: what `get nodes,pods -A -o json` prints: the node (allocatable
  `memory` is `24556112Ki`, `cpu` is `12`) and every pod with its requests, `phase`, owners
  and `priority`. It holds two `Succeeded` pods (`restore-*`) at priority 0, which hold no
  resources and must not count.
- `priority-listing.json`: what `get pods,priorityclasses -A -o json` prints: the same pods
  and the town's PriorityClasses. Owner references sit in `metadata.ownerReferences`, as the
  API puts them, not under `spec`.

To refresh: capture the two listings again with the same stripping, and keep the shapes (the Ki
suffix, the metadata owners, the phases).
