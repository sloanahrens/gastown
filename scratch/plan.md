# gt-m52sj5 plan

Finding (om 66fbf1b010b3, minor): `warnings` slice in
internal/doctor/global_state_check.go now holds at most one entry, after
0653758b removed the second warning branch.

## Acceptance boxes

- [ ] `warnings` slice holds at most one entry
  - Wire: replace `var warnings []string` + `append` + `len(warnings) > 0` +
    `warnings[0]` with a single `warning string` set in the disabled branch.
  - Test: the existing `TestGlobalStateCheck_Run` row "disabled state is a
    warning" asserts Status/Message/FixHint for that path; deleting the
    assignment turns it red. The slice type itself is not observable from
    outside `Run`, and a test asserting it would be an implementation-detail
    test, which the rig's test norms forbid.

## Refusal branches

None added. The check's existing refusal paths (missing state -> warning,
unreadable state -> error) are unchanged and already have table rows.

## How this most likely fails in production

`warning != ""` is the sentinel; it is exact only because the sole assigned
value is a non-empty constant. A future second warning must reintroduce a
slice, not an empty string.
