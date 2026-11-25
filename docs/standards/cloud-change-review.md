# Reviewing a change that touches the cloud

> Canonical copy since 2026-11-24 (moved from the operator's ~/.claude/docs/standards so a sandbox seat can read it); edit it here.

For the overseer, bird reviews and hand reviews. Applies to any bead or diff that touches GCP, Firebase, Pulumi, `gcloud`, a manifest, a role, a service account or a console step. The rules om applies live elsewhere (central directive gt-o87rv; devops `.om.json` via dv-m75m); this page is what a person asks before PASS. Draft by owl, accuracy-checked against devops `origin/main` by merlin, 2026-10-30.

## Why

Most cloud controls are detective: the patrol finds an unexpected API, binding or key after the fact. On 2026-10-30 the console "add Firebase" step enabled 28 APIs and created `firebase-adminsdk-fbsvc` with `roles/iam.serviceAccountTokenCreator`. No diff existed for any review to read. The cloud-check plugin raised an urgent patrol finding (escalation hq-wisp-s5jax) and the cleanup beads dv-j61t, dv-e1fk and dv-racb followed. The default Compute account with Editor, created when Cloud Run is enabled (dv-9ez, dv-7y3), had the same shape. Ask, before the step runs, what it will create outside the diff.

## Checklist

Answer every line on the bead or in the review. "n/a" needs a reason.

1. **New API.** Who approved the list (bead comment, a named agent or the operator)? What does Google enable beside it? Every extra API must be in the manifest or in the patrol's exempt list `autoEnabledAPIs` (`pkg/patrol/checks.go`, five entries; `checkAPIs` reports the rest as `api_unexpected`). Widening that list also means widening `dependencyAPIs` (`internal/gcp/platformbootstrap/component.go`, two entries), in the same change.
2. **IAM role or permission.** Is the golden diff there (`internal/gcp/testdata/deployer_{,cluster_,static_,union_}ceiling.golden`) with a bead comment saying why? Is any permission in `forbiddenDeployerPermissions` (`internal/gcp/iam.go`: `iam.serviceAccountKeys.create`, `iam.serviceAccounts.create`, `resourcemanager.projects.delete`, `resourcemanager.projects.setIamPolicy`, `serviceusage.services.enable`)? Reject. `iam.serviceAccounts.actAs` never goes on the project; it is a binding on the one account. Custom roles only (`gtDeployerRun`, `gtAuditorRead`); `.om.json` least-privilege makes project-level `serviceAccountUser` / `serviceAccountTokenCreator`, wildcard or `allUsers` members and any role broader than the bead names a major finding.
3. **Service account or key.** Who owns it, what uses it, what is its deletion path? The design allows exactly two long-lived keys, base and auditor (`docs/design.md`, "GCP identities"). Is the creation in code the patrol can reconcile, or in the console?
4. **Destroy, disable, delete.** Preview output on the bead first. Never forced: no `disableDependentServices`, no retry with force after a dependents error (dv-e1fk), no `--skip-preview` outside the release path that already carries it. Typed confirmation at the step; rollback or re-enable path stated; read-only evidence of what depends on the target comes first (dv-j61t).
5. **Console-only step.** What does the console create beyond what the bead names? Is the result listed afterwards by a read-only command and compared with the patrol? Who runs it, and is the operator's go on the bead?
6. **Cost.** Billing plan (Spark or Blaze), budget with alert, anything that bills while idle (VM, disk, address, SQL, load balancer, registry growth). No cited rule yet.
7. **Go, not shell.** Cloud logic is Go with an injected Exec seam and tests. A new script holding testable logic is a major; a new wizard is always a major (`.om.json` scripts-are-debt).
8. **Credentials.** Nothing in argv, logs, fixtures or echoed config; no polecat-run test needs a cloud credential (`.om.json` no-credentials).

## Evidence to demand before PASS

- **Code change:** the golden diff; the test going red when the rule is removed (mutate it; this is the one place the rule is stated, checklist items 2 to 8 rely on it); `go test` output for the package, not a summary.
- **Apply or destroy bead:** the preview pasted, the exact command line, the patrol result before and after (finding count by kind), a read-only listing of what now exists.
- **Console step:** before and after listings, names only (`gcloud services list --enabled`, `gcloud iam service-accounts list`, the project policy).
- **"Nothing else changed":** a patrol run after the step, never the author's statement.

## Runbook

1. Read the bead's Goal and Gate. Find the verbs (enable, grant, create, delete, disable, destroy); each maps to a checklist line.
2. Diff the manifest, role lists and goldens. A widening with no bead comment is a stop.
3. For each console or provider call ask what else it creates. Read the vendor doc first and list the extras.
4. Mutate each guard (the forbidden entry, the deny check, the confirmation) as the evidence list says.
5. Anything run against a real project: preview first, then the operator's go on the bead, then the post-run patrol. The agent never runs it.
6. After landing, confirm the patrol allowlists and exemptions changed in the same landing as the manifest.
7. Record the answers on the bead with evidence, not "looks fine".

To be extended by dv-0w7l.9 (how a ceiling grows: the discovery protocol and golden review per Firebase product) and dv-0w7l.12 (goose's decisions as checklist inputs).
