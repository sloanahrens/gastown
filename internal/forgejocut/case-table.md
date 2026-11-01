# Cutover case table: the shell test's assertions and their Go tests

`scripts/forgejo-cutover_test.sh` is the shell test this port replaces; it
stays until the deletion bead (gt-tpg0p.52). Every assertion it executes is
mapped below to the Go test that holds the same behaviour, so deleting an
assertion's Go coverage is visible here first.

Counts. The shell test executes 157 assertions today: 138 check lines plus
19 exit-status assertions in the case headers (the bead says 158; the count
was taken at filing). Every one is accounted for below. The --mirror case is
the one block with no Go behaviour to test: the port dropped --mirror,
--mirror-target and --provision as flags (bead gt-tpg0p.44), so its
assertions map to the test that pins the refusal.

Scope note (recorded, not hidden). Bead gt-tpg0p.47 (6/10, step 4, the
promote key) was still open with no EnsurePromoteKey in the tree when
gt-tpg0p.51 was dispatched, so the run could not wire step 4 and the
promote-key assertions below had no Go test. gt-tpg0p.51 carries step 4 in
internal/forgejocut/promotekey.go at that bead's own API (EnsurePromoteKey,
GhPromoteKeyCommands), with key files through the run's Files seam and
ssh-keygen through the exec seam, so the whole run is wired and every
assertion has a test. gt-tpg0p.47 should be closed as superseded rather than
landing a second implementation.

## a landing in flight refuses the cutover

- `473` a queued landing exits non-zero — TestRefuseIfLandingInFlightMessage; TestRunStopsAtTheFirstFailingStepAndSaysWhich (row step 1 (refusing a landing in flight))
- `474` the refusal names the queued landings — TestRefuseIfLandingInFlightMessage
- `475` the probe never runs — TestRunStopsAtTheFirstFailingStepAndSaysWhich (that row asserts the probe line is absent)
- `476` nothing is written — TestRunStopsAtTheFirstFailingStepAndSaysWhich (the run stops before steps 5 and 6)
## a probe that is not green refuses the cutover

- `482` a red probe exits non-zero — TestRunStopsAtTheFirstFailingStepAndSaysWhich (row step 1 (the probe))
- `483` the refusal says the probe is not green — TestProbeArgsAndOutcome
- `484` the probe was run — TestProbeArgsAndOutcome
- `485` no mirror is created — TestMainUsageExitsTwoAndHelpExitsZero (the port refuses --mirror)
## an unreadable landing queue refuses the cutover

- `491` an unreadable queue exits non-zero — TestRefuseIfLandingInFlightMessage (queue read fails)
- `492` the refusal says the queue could not be read — TestRefuseIfLandingInFlightMessage
- `493` the probe never runs — TestRunStopsAtTheFirstFailingStepAndSaysWhich (an unreadable queue is the same step-1 error as the queued row)
## --dry-run prints every command and writes nothing

- `502` a dry run exits 0 — TestRunDryRunWritesNothingOutsideItsTempDir
- `503` it prints the probe command — TestRunDryRunWritesNothingOutsideItsTempDir
- `504` the printed probe carries the cutover town root — TestRunDryRunWritesNothingOutsideItsTempDir; TestProbeArgsAndOutcome
- `505` it prints the ref import — TestRunDryRunWritesNothingOutsideItsTempDir; TestImportDryRunPushesNothingAndPrintsThePlan
- `506` it prints the key it would mint — TestRunDryRunWritesNothingOutsideItsTempDir
- `507` it says it would mint, not that it did — TestRunDryRunWritesNothingOutsideItsTempDir
- `508` it prints the gh deploy-key command — TestRunDryRunWritesNothingOutsideItsTempDir; TestRunPromoteKey
- `509` it prints the daemon restart — TestRunDryRunWritesNothingOutsideItsTempDir; TestRestartDryRunRunsNothing
- `510` it names the github Actions reminder — TestOperatorStepsText
- `511` it does not print the string form that 422s — TestOperatorStepsText
- `512` it says it would back up, not that it did — TestRunDryRunWritesNothingOutsideItsTempDir; TestSettingsDryRun; TestRepointDryRunChangesNothing
- `513` it never claims a backup it did not make — TestRunDryRunWritesNothingOutsideItsTempDir
- `514` the settings file is byte-identical — TestRunDryRunWritesNothingOutsideItsTempDir
- `515` town.json is byte-identical — TestRunDryRunWritesNothingOutsideItsTempDir
- `516` no config backup is left — TestRunDryRunWritesNothingOutsideItsTempDir
- `517` no town.json backup exists — TestRunDryRunWritesNothingOutsideItsTempDir
- `518` no settings backup exists — TestRunDryRunWritesNothingOutsideItsTempDir
- `519` the ref import changed nothing — TestRunDryRunWritesNothingOutsideItsTempDir; TestImportDryRunPushesNothingAndPrintsThePlan
- `520` no promote key is created — TestRunDryRunWritesNothingOutsideItsTempDir
- `521` ssh-keygen was not run — TestRunDryRunWritesNothingOutsideItsTempDir
- `522` no remote was set — TestRunDryRunWritesNothingOutsideItsTempDir; TestRepointDryRunChangesNothing
- `523` no write call was sent — TestProvisionDryRunSendsNoWrite; TestRunDryRunWritesNothingOutsideItsTempDir
- `524` the daemon was not restarted — TestRunDryRunWritesNothingOutsideItsTempDir; TestRestartDryRunRunsNothing
## a real cutover converges the rig

- `531` the cutover exits 0 — TestRunSecondRunChangesNothing; TestRunPromoteKey
- `532` the probe ran for the rig — TestProbeArgsAndOutcome
- `533` the probe ran with the cutover town root — TestProbeArgsAndOutcome
- `536` the repo is provisioned through the shared provisioner — TestProvisionInProcessAgainstTheFake; TestProvisionCallsTheProvisionerOnce
- `537` every GitHub ref was imported — TestImportFinalComparison; TestRunSecondRunChangesNothing
- `538` a default run creates no push mirror — TestMainUsageExitsTwoAndHelpExitsZero (--mirror refused)
- `539` no mirror body was sent — TestMainUsageExitsTwoAndHelpExitsZero
- `540` the promote key was minted at the default path — TestRunPromoteKey
- `541` the key is ed25519 — TestRunPromoteKey
- `542` the private key is mode 600 — TestRunPromoteKey
- `543` the gh command names the public key — TestRunPromoteKey
- `544` the gh command grants write — TestRunPromoteKey
- `545` the gh command titles the key for the rig — TestRunPromoteKey
- `546` the private key never reaches stdout — TestRunPromoteKey
- `547` the import listed the admin base — TestImportNeverForces
- `548` the import pushed to the admin base — TestImportNeverForces
- `549` the probe pushed through the admin base — TestProbeArgsAndOutcome
- `550` the bare repo is repointed — TestRepointVisitsBareMayorAndEveryCrew
- `551` the mayor clone is repointed — TestRepointVisitsBareMayorAndEveryCrew
- `552` the crew clone is repointed — TestRepointVisitsBareMayorAndEveryCrew
- `553` a non-repo crew dir is skipped — TestRepointVisitsBareMayorAndEveryCrew
- `554` settings names the Forgejo remote — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `555` settings names the gate workflow — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `556` settings names the three bots — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `557` settings names the promote target — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `558` settings names the promote key — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `559` settings names no mirror target — TestSettingsBlockDecodesThroughConfigForgejoConfig (the block builder writes no mirror_target)
- `560` town.json git_url is repointed — TestTownGitURLEditsOnlyThatField
- `561` the settings file is backed up — TestSettingsBacksUpBeforeEditing
- `562` town.json is backed up — TestTownGitURLEditsOnlyThatField; TestBackupFileNameAndContent
- `563` the daemon was restarted — TestRestartOnlyWhenNoLanding
- `564` no token reached argv — TestRunBrokerToken (no exec or git argv carries it); TestProbeKeepsAdminTokenOutOfArgvLogsAndErrors
## a second cutover over converged state changes nothing

- `572` the second cutover exits 0 — TestRunSecondRunChangesNothing
- `573` the promote key is reused — TestRunSecondRunChangesNothing; TestRunPromoteKey (reuse row)
- `574` the key is not regenerated — TestRunSecondRunChangesNothing; TestRunPromoteKey (reuse row)
- `575` the remotes are reported already right — TestRepointSkipsAlreadyCorrect; TestRunSecondRunChangesNothing
- `576` no remote is set again — TestRunSecondRunChangesNothing
- `577` town.json is unchanged — TestTownGitURLAlreadyMatchingWritesNothing; TestRunSecondRunChangesNothing
- `578` no second backup is written — TestRunSecondRunChangesNothing
- `579` the block is reported already matching — TestSettingsAlreadyMatchingIsUntouched; TestRunSecondRunChangesNothing
## --mirror reproduces the push mirror

- `586` the mirror cutover exits 0 — TestMainUsageExitsTwoAndHelpExitsZero (--mirror dropped by the port)
- `587` the push mirror was created without a branch filter — TestMainUsageExitsTwoAndHelpExitsZero
- `588` the mirror enables ssh — TestMainUsageExitsTwoAndHelpExitsZero
- `589` the mirror deploy key is printed — TestMainUsageExitsTwoAndHelpExitsZero
- `590` the block names the mirror target — TestMainUsageExitsTwoAndHelpExitsZero
- `591` the block names no promote fields — TestMainUsageExitsTwoAndHelpExitsZero
- `592` no promote key is minted — TestMainUsageExitsTwoAndHelpExitsZero
- `593` no promote key file is left — TestMainUsageExitsTwoAndHelpExitsZero
## a hostname-form --forgejo-url does the script own git work as admin

- `604` the cutover exits 0 — TestImportNeverForces; TestRunSecondRunChangesNothing
- `605` the ref listing and import succeeded — TestImportFinalComparison
- `606` the import listed the admin base — TestImportNeverForces
- `607` the import pushed to the admin base — TestImportNeverForces
- `608` the unreachable hostname never carried a ref listing — TestImportNeverForces
- `609` the unreachable hostname never carried a push — TestImportNeverForces
- `610` the probe pushed through the admin base — TestProbeArgsAndOutcome
- `611` the hostname form is written to the bare remote — TestRepointVisitsBareMayorAndEveryCrew
- `612` the hostname form is written to the mayor clone — TestRepointVisitsBareMayorAndEveryCrew
- `613` the hostname form is written to the crew clone — TestRepointVisitsBareMayorAndEveryCrew
- `614` the hostname form is written to town.json — TestTownGitURLEditsOnlyThatField
- `615` the hostname form is written to the rig block — TestSettingsBlockDecodesThroughConfigForgejoConfig
- `616` the promote target is the ssh form of the GitHub URL — TestSettingsBlockDecodesThroughConfigForgejoConfig; TestSSHFormTable
## a landing queued at the restart is left to the operator

- `623` the cutover still exits 0 — TestRunRestartLeftToTheOperator
- `624` it says the landing queue is not empty — TestRunRestartLeftToTheOperator; TestRestartOnlyWhenNoLanding
- `625` the daemon was not restarted — TestRunRestartLeftToTheOperator; TestRestartOnlyWhenNoLanding
- `626` it prints the restart command for later — TestRunRestartLeftToTheOperator; TestRestartOnlyWhenNoLanding
- `627` the rig is still cut over — TestRunRestartLeftToTheOperator
## Forgejo ahead of GitHub keeps its refs, never rewinding them

- `638` a second cutover of an ahead rig exits 0 — TestClassifyTable (ActionKeep is not an error); TestImportNeverForces
- `639` it says Forgejo is ahead, with the count — TestRefPlanSortedAndLogged; TestCommitsAheadUsesTheGitSeam
- `640` it names the Forgejo commit — TestRefPlanSortedAndLogged
- `641` it names the GitHub commit — TestRefPlanSortedAndLogged
- `642` Forgejo main is unchanged — TestImportNeverForces
- `643` nothing was pushed — TestImportNeverForces
- `644` no force was used — TestImportNeverForces
- `645` it continued to the promote step — TestClassifyTable (a keep is not an error); TestImportNeverForces
- `646` the daemon was restarted — TestRestartOnlyWhenNoLanding
## Forgejo behind GitHub is fast-forwarded

- `654` the cutover exits 0 — TestClassifyTable; TestImportNeverForces
- `655` it plans the fast-forward — TestRefPlanSortedAndLogged
- `656` it plans the ref Forgejo lacks — TestRefPlanSortedAndLogged; TestClassifyTable
- `657` Forgejo main moved to the GitHub commit — TestImportNeverForces
- `658` the new ref arrived — TestImportNeverForces
- `659` the push did not force — TestImportNeverForces
- `660` no push refspec carried a force marker — TestImportNeverForces
## a diverged ref stops the run before anything is pushed

- `669` a diverged ref exits non-zero — TestRunDivergedRef; TestImportDivergedStopsOrSkips
- `670` it names the diverged ref — TestRunDivergedRef; TestRefPlanSortedAndLogged; TestImportDivergedStopsOrSkips
- `671` it names both commits — TestRefPlanSortedAndLogged
- `672` it names the override — TestImportDivergedStopsOrSkips
- `673` nothing reached Forgejo — TestRunDivergedRef; TestImportDivergedStopsOrSkips
- `674` no push was attempted — TestRunDivergedRef; TestImportDivergedStopsOrSkips
- `675` the cutover stopped before the promote key — TestRunDivergedRef (nothing is written or minted)
## --accept-diverged skips that ref and imports the rest

- `679` an accepted divergence exits 0 — TestImportDivergedStopsOrSkips
- `680` it still reports the divergence — TestImportDivergedStopsOrSkips
- `681` the diverged ref is left as Forgejo has it — TestImportDivergedStopsOrSkips
- `682` the other ref was imported — TestImportDivergedStopsOrSkips
- `683` no force was used — TestImportNeverForces
## --dry-run shows a rewind before it can happen

- `692` the dry run exits 0 — TestImportDryRunPushesNothingAndPrintsThePlan
- `693` the dry run prints the kept line — TestRefPlanSortedAndLogged; TestImportDryRunPushesNothingAndPrintsThePlan
- `694` the dry run leaves Forgejo alone — TestImportDryRunPushesNothingAndPrintsThePlan
- `695` the dry run pushed nothing — TestImportDryRunPushesNothingAndPrintsThePlan
## --dry-run shows the fast-forward it would make

- `704` the dry run exits 0 — TestImportDryRunPushesNothingAndPrintsThePlan
- `705` the dry run prints the fast-forward — TestImportDryRunPushesNothingAndPrintsThePlan
- `706` the dry run leaves Forgejo alone — TestImportDryRunPushesNothingAndPrintsThePlan
- `707` the dry run pushed nothing — TestImportDryRunPushesNothingAndPrintsThePlan
## --dry-run reports a divergence as the stop it would be

- `715` the dry run stops on the divergence — TestRunDivergedRef (dry-run row)
- `716` it prints the diverged line — TestRunDivergedRef (dry-run row)
- `717` nothing was pushed — TestRunDivergedRef (dry-run row)
## no admin token anywhere is a loud failure

- `736` a run with no admin token exits non-zero — TestRunExitCodes (no admin token)
- `737` the failure names the broker command — TestRunExitCodes (no admin token)
- `738` the file token never reaches the output — TestRunExitCodes (the decoy read assertion)
- `739` no request is sent without a token — TestRunExitCodes (no admin token)
- `740` no probe is run without a token — TestRunExitCodes (no admin token)
## the auth broker run act supplies the token

- `757` a run under the broker token exits 0 — TestRunBrokerToken (broker row)
- `758` every call presents the broker token — TestRunBrokerToken (no Forgejo violation)
- `759` the broker token never reaches a stub argv — TestRunBrokerToken (no exec or git argv holds it)
- `760` the broker token never reaches the output — TestRunBrokerToken
- `761` the broker token reaches no file the run writes — TestRunBrokerToken (files, keys and token files checked)
- `763` the probe is run with no token file to read — TestRunBrokerToken
- `764` the probe is run with no token in its own argv — TestRunBrokerToken
- `765` the provisioner is run with no token file to read — TestRunBrokerToken (the captured provisioner options name none)
- `766` the provisioner is run with no token in its own argv — TestRunBrokerToken
- `767` the run still converges the rig — TestRunBrokerToken
- `768` the run still creates the push mirror — TestMainUsageExitsTwoAndHelpExitsZero (--mirror dropped by the port)
## the environment override stands in for the broker

- `784` a run with the override in the environment exits 0 — TestRunBrokerToken (override row)
- `785` the environment override is the one presented — TestRunBrokerToken (the fake accepts it on every call)
