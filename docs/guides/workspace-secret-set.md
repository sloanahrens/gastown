> Status: current (2026-11). Runbook for plan row K6; the live creation is owed by the operator in a grant window and has not run.

# A workspace's Secret Manager secret set

Read this before creating, rotating or rolling back the Secret Manager secrets that hold a hosted workspace's model provider key. It is plan row K6 of the hosted-platform plan (the plan document in the claude repo); the store it feeds is `internal/keystore/gsm` (row K2b, gt-4m749). Customer zero's workspace id is `customer-zero` (`workspaceid.CustomerZero`).

Creating the set is a cloud change: it needs the answers under **Cloud-change review answers** below, a grant window and the operator's hands. Nothing in this repo creates a secret, and no test in it calls the cloud.

## Secret names

One secret per key name, with the workspace id first. `gsm.SecretID` joins the two with an underscore, and neither part may hold one, so two keys never share an id.

| Key name | Secret id for customer zero | Where the name comes from |
|---|---|---|
| `api-key` | `customer-zero_api-key` | `keycheck.KeyName` |

An id holds 1 to 100 characters of letters, digits and hyphen, plus the underscore the adapter keeps as its separator (`gsm.ErrBadName`).

## Creating the set

In the grant window, create one secret per key name and give it its first version, with the value on stdin so it never reaches a command line or shell history. `$PROJECT_ID` is the hosted project's id, set in the window and recorded in no file:

```bash
gcloud secrets create customer-zero_api-key --project "$PROJECT_ID" --replication-policy automatic
gcloud secrets versions add customer-zero_api-key --project "$PROJECT_ID" --data-file=-
```

Then bind the reader described below. Check the binding by reading the value as an identity that holds none — impersonate another workspace's gateway — and confirming the read fails. Discard standard output and judge the read by its exit status and its error, so a binding that is wrong cannot print the value:

```bash
# The read must fail. stdout is discarded, so a wrong binding that lets it
# through prints nothing; stderr stays visible, so a refused impersonation can
# be told apart from a refused read. $OTHER_GATEWAY is another workspace's
# gateway service account.
gcloud secrets versions access latest --secret customer-zero_api-key --project "$PROJECT_ID" \
  --impersonate-service-account="$OTHER_GATEWAY" >/dev/null
```

A refused read exits non-zero with `PERMISSION_DENIED` on `secretmanager.versions.access`. A read that succeeds exits zero with no output, and is a failure. Any other error — the impersonation refused, the API unreachable — is a check that could not be made, not a pass.

## Reader identity

One service account per workspace gateway, and one IAM binding per secret:

- Bind `roles/secretmanager.secretAccessor` on `customer-zero_api-key` to that workspace's gateway service account, and to no other principal.
- Bind on the secret, not on the project: a project-level binding lets every identity that holds the role read every workspace's secret.
- Give the gateway neither `roles/secretmanager.admin` nor `roles/secretmanager.viewer`: it needs only `secretAccessor`. The adapter reads a value only through `Get` for the key's exact name.
- The operator's identity writes the secret in the grant window, and its binding is removed when the window closes, so it holds no read on the value afterwards. The admin plane — the console, the dispatcher, beads — never reads a key value.

The property a set keeps is that a workspace's key is readable by that workspace's gateway alone: no shared secret, no cross-workspace read, no admin-plane read.

## Rotation

Rotation is the K5 package (`internal/keyrotation`, gt-fn9nz). It keeps no state of its own: the store answers what is enabled and what is safe to destroy, so a rotation started in one process finishes from another.

1. Write the replacement key as a new version of the same secret. `Rotate` calls `Store.Put`, so the version number rises by one and the new version becomes the one `Get` returns next.
2. Check it. `Rotate` asks its `Checker` whether the new version works — the same provider verification the pre-dispatch check makes (`keycheck`). A refusal disables the new version: the previous key stays live, `Rotate` returns `Rotated: false` with an `OwnerNotice`, and there is nothing to undo.
3. On a pass, `Rotate` disables every older enabled version, so new seats read only the replacement. A seat already started keeps the value it read; nothing can revoke that.
4. Retire the older versions. `SeatEnded` destroys a version once the store reports it disabled and no seat started on it is still running; a second call is safe. A destroyed version's number is never reused.

The check window: step 1 makes the new version the one `Get` returns before step 2 has run, so a seat that starts mid-rotation can read an unchecked key. The store cannot stage a write; closing the window is a separate bead. Rotate while the workspace is quiet.

Where a workspace has a wired rotate action (plan row O5), it runs steps 1 to 4. Where it has none, the operator does them by hand in a grant window, in the same order:

- Write the replacement; the value arrives on stdin, so it never reaches a command line:

  ```bash
  gcloud secrets versions add customer-zero_api-key --project "$PROJECT_ID" --data-file=-
  ```

- Check the new version with the provider, as `keycheck` does, before the old one is touched. Read the new version into the shell with `gcloud secrets versions access`, and send it with the provider request on stdin (`curl --config -`), never as a `-H` argument, so the value stays off the command line. Make HTTP errors non-zero (`--fail`) and discard the response (`-o /dev/null`): the exit status is the verdict.

- On a pass, disable the version it replaces, then destroy it once no seat started on it is still running:

  ```bash
  OLD=1   # the version being replaced; use the number versions list reports
  gcloud secrets versions disable customer-zero_api-key --project "$PROJECT_ID" --version "$OLD"
  gcloud secrets versions destroy customer-zero_api-key --project "$PROJECT_ID" --version "$OLD"
  ```

- On a refusal, or a check that could not be made, disable the new version and stop. The previous key never stopped serving, so there is nothing else to undo:

  ```bash
  NEW=2   # the version versions add wrote; use the number versions list reports
  gcloud secrets versions disable customer-zero_api-key --project "$PROJECT_ID" --version "$NEW"
  ```

## Rollback

- **A refused rotation.** Nothing to do: it disabled the replacement and left the previous key serving.
- **A rotation that passed but broke the workspace.** Write the previous key back as a new version. A destroyed version cannot be restored and a number is never reused, so the old value is re-added, never re-enabled; `Get` skips a disabled version and reads the one below it.
- **Undo of the creation.** Remove the gateway's binding first, so reads stop, then destroy each version, then delete the secret. A destroyed version is gone for good; a deleted secret can be undeleted within Secret Manager's 30-day window.
- **A set created under the wrong workspace id.** The adapter refuses any call naming a workspace other than its own (`ErrWrongWorkspace`) before it reaches Secret Manager, so the misnamed secret is dead weight that no gateway reads, never a cross-tenant read. Roll it back with *Undo of the creation* and recreate it under the right id.

## Cloud-change review answers

**Blast radius.** One secret, one workspace, one reader. A mistake leaves customer zero without the key its gateway needs; no other workspace's set is touched, and no other principal holds a binding on this one. The failure is confined to that workspace's gateway and key, not to the project.

**Cost line.** None beyond the secret versions themselves: no VM, bucket, network or replication beyond Secret Manager's default. Secret Manager bills per active version and per access, so this set is one secret with one or two non-destroyed versions plus the gateway's reads at start.

**Undo.** **Rollback** above: binding off, versions destroyed, secret deleted, all in the grant window.

**Who holds the keys.** The customer supplies the model provider key; the operator writes it into the secret in the grant window; that workspace's gateway identity reads it; nobody else reads it. No key value, project number or token appears in this file, in a bead, in a log or on a command line.

## The check this file passes

Run from the repo root. A pass is no output and exit 1, so the file holds no key-shaped string and no project number:

```bash
grep -nE 'sk-[A-Za-z0-9]{16,}|AIza[0-9A-Za-z_-]{30,}|[0-9]{12}' docs/guides/workspace-secret-set.md
```

Every section heading, the five the row names among them, is listed by:

```bash
grep -n '^## ' docs/guides/workspace-secret-set.md
```
