> Status: historical (2026-10). Merged in: gt-tpg0p.70. Not maintained.

# Unlocking the signing keychain without the password in argv

**Bead:** gt-tpg0p.70. **Host:** macOS 26.6.2 (arm64), Go 1.27.1. **Author:** merlin.

## Question

`security unlock-keychain -p <password> <keychain>` puts the password in the process's argv, where `ps` shows it for as long as the unlock runs. `security help unlock-keychain` says so itself: "Use of the -p option is insecure". Which other way unlocks the unattended signing keychain with no password in any argv?

## Short answer

Pass the password to `security` on **stdin**, with no `-p` word. It unlocked a locked throwaway keychain with no tty and no controlling terminal, and `ps` showed only `security unlock-keychain <keychain>`. The prompt is written to stderr and the password is not echoed. A Go caller needs a stdin-capable exec seam. `security -i`, where the whole command line is read from stdin, also works. `internal/installer` already uses it (gt-4ucnp). The two callers of the shared `signing.Unlock` do not.

## What was run

Everything below ran against throwaway keychains inside this worktree's `.kc-scratch/`, created with a made-up password string. No real password was used or written down. Each probe locked the keychain, ran the candidate, then read a generic-password item with `security find-generic-password -s probe-svc -w <keychain>`. A locked keychain returns nothing from that read (no tty, so no dialog) and an unlocked one returns the item. That read is the pass/fail oracle in the table. A negative control (a wrong password) was run for stdin and for the Go call.

A signing end-to-end was not run: a self-signed code-signing certificate is not trusted, `codesign` answered "no identity found", and trusting it means `security add-trusted-cert`, a user-wide change outside the throwaway. The lock oracle judges only whether the keychain unlocked, not whether `codesign` then signs.

## Verdicts

| # | Candidate | Verdict | Argv exposure left |
|---|---|---|---|
| 1 | Password on **stdin**, no `-p` | **works unattended** | none |
| 1b | `security -i`, command line on stdin | **works unattended** | none (the line holds the password, on stdin only) |
| 2 | Prompt read from a **pty** (`expect`) | works, but adds nothing over 1 | none |
| 3 | **No-password keychain** | works unattended, but removes the protection | none |
| 4 | **Partition list** / key ACL | does not unlock; unsupported as an answer | `-k <password>` in argv |
| 5 | **Security framework** through a small cgo call | works unattended; deprecated API | none |
| 6 | `unlock-keychain -u` (GUI prompt) | needs a human | none |

### 1. Password on stdin

Primary source, `security help unlock-keychain` (macOS 26.6.2):

> Usage: unlock-keychain [-hu] [-p password] [keychain]
> -p  Use "password" as the password to unlock the keychain
> -u  Prompt for the password via GUI.
> Use of the -p option is insecure

Without `-p`, the tool prompts for a password on its own. Results from the throwaway keychain:

- Locked, then `echo <made-up> | security unlock-keychain <kc>`: exit 0, the item reads back.
- Wrong password the same way: exit 51, stderr `SecKeychainUnlock ...: The user name or passphrase you entered is not correct.`, the item stays unreadable. The error text does not contain the password.
- Stdin from a file (`< pw.txt`), the same result.
- A process started with `start_new_session=True` (no controlling terminal, as under launchd) and the password written to its stdin pipe: exit 0, unlocked.
- `ps -o args=` on the running process: `security unlock-keychain /…/probe.keychain-db`. The `-p` and the password are absent.
- Closed stdin (`</dev/null`) or an empty line: exit 51. An empty password is a wrong password on a keychain that has one.
- The prompt `password to unlock <keychain>:` goes to stderr, so a caller that logs stderr should discard it or expect that line.

This is the recommendation. The caller must pass the password to the child's stdin and must not log it.

### 1b. `security -i`

`security -i` reads command lines from stdin. Writing `unlock-keychain -p <pw> <kc>\n` to it unlocked the throwaway keychain (exit 0), and `ps` showed only `security -i`. A wrong password exited 51. This is the form `internal/installer/sign.go` uses (`unlockLine`, gt-4ucnp). Its caveat stands: the `-i` line parser is undocumented, so a password that needs quoting or holds a line break has to be handled by the caller. Stdin without `-i` (candidate 1) has no parser to get wrong, because the password is the whole line.

### 2. Prompt read from a pty

`expect` spawned `security unlock-keychain <kc>`, waited for `password to unlock*`, and sent the made-up password: exit 0, unlocked. A pty is not needed: candidate 1 works with a plain pipe and no controlling terminal. A pty only adds a dependency (`expect`, or `creack/pty` in Go) and a race on the prompt text. Not recommended.

### 3. A keychain with no password

A keychain created with an empty password (`security create-keychain -p "" <kc>`; the made-up probe used this only for the test) unlocked with `security unlock-keychain <kc> </dev/null` (exit 0), with `-p ""`, and with an empty stdin line. `security set-keychain-settings <kc>` (no `-t`) printed `no-timeout`; the man page says "omitting this option specifies 'no timeout'". After an explicit `lock-keychain` the keychain was locked again, so nothing unlocks it by itself.

Verdict: it works unattended with no password anywhere, which removes the thing to hide. It also removes the protection the password gives the private key at rest, leaving only file mode `0600`. That is a security decision for the operator, not an engineering one. Not recommended while candidate 1 is available.

### 4. Partition list

Primary source, `man security`:

> set-key-partition-list [-S partition-list] [-k password] [options...] [keychain] Sets the "partition list" for a key. The "partition list" is an extra parameter in the ACL which limits access to the key based on an application's code signature. You must present the keychain's password to change a partition list. If you'd like to run /usr/bin/codesign with the key, "apple:" must be an element of the partition list.
> -k password     Password for keychain (deprecated)

The partition list decides which applications may use a key. It does not unlock the keychain, and changing it needs the password: `-k` in argv, or the prompt with no `-k` (`(deprecated) password to unlock <kc>:`, the same stdin behaviour as candidate 1). Run once at provisioning, it stops `codesign` from showing a key-access dialog, which is a different problem from this one. It is not a way to unlock.

### 5. Security framework through a small Go call

Primary source, Apple documentation for `SecKeychainUnlock(_:_:_:_:)`:

> Unlocks a keychain. … If you pass a locked keychain, this function displays the Unlock Keychain dialog box if you have not provided a password. … A buffer containing the password for the keychain. Pass NULL if the user password is unknown. In this case, this function displays the Unlock Keychain dialog to prompt the user for the keychain password. … A Boolean value indicating whether the password parameter is used. You should pass TRUE if you are passing a password or FALSE if it is to be ignored.

The page marks it deprecated at macOS 10.10 ("SecKeychain is deprecated"). `security(1)` calls this same function: its error text reads `SecKeychainUnlock <kc>: The user name or passphrase you entered is not correct.`

A 20-line cgo program (`SecKeychainOpen` then `SecKeychainUnlock(kc, len, pw, TRUE)`, linked with `-framework Security -framework CoreFoundation`, password read from stdin) built under Go 1.27.1 and was run against the throwaway keychain: a wrong password returned OSStatus -25293 (`errSecAuthFailed`) and left it locked, the right one returned 0 and the item read back, and `ps` showed only `./gounlock-bin <kc> -`. It leaves no argv exposure, but it needs cgo in a build that is otherwise pure Go, calls a deprecated API, and gains nothing over candidate 1, which reaches the same function through the signed system tool. Not recommended.

### 6. `-u` (GUI prompt)

`-u  Prompt for the password via GUI.` It shows a dialog in the logged-in user's session, so it needs a human at the screen and does not work in an unattended install. It was not run. This is the documented behaviour of `SecKeychainUnlock` with no password (quote under candidate 5).

## Remaining exposure with candidate 1

- The password lives in the caller's memory and in the child's stdin pipe. `ps` shows neither. A process running as the same user can still read the caller's memory or the 0600 password file; this fixes `ps`, not same-user compromise.
- The password file path, read by the caller, is unchanged.
- `security`'s stderr carries the prompt text and, on failure, the keychain path. It never carried the password in any run.

## Where the argv exposure is today

`signing.Unlock` (`internal/signing/signing.go`) runs `security unlock-keychain -p <password> <keychain>` through `signing.Exec`, whose signature (`func(ctx, name string, args ...string) (stdout, stderr string, err error)`) has no stdin. It is called from `internal/ominstaller/sign.go` and `internal/binaryinstall/sign.go`. `internal/installer/sign.go` already avoids it with `security -i` on stdin. The tests that pin the old argv are in `internal/signing/signing_test.go`, `internal/binaryinstall/sign_test.go` and `internal/ominstaller/sign_test.go`.

## How the throwaway keychain was checked

Before the first keychain was created, `security default-keychain`, `security list-keychains -d user` and `security list-keychains` were saved. The throwaway keychains were under `crew/merlin-gt-tpg0p.70/.kc-scratch/`, created with `security create-keychain` and never passed to `security default-keychain -s` or `security list-keychains -s`. After each create the list was read again and did not contain them. At the end both were removed with `security delete-keychain`, the same three commands were run again and diffed against the saved output: **identical**. The default keychain stayed `login.keychain-db`. The real signing keychain under `~/gt/.runtime/signing` was never named in a command except in the search list, where it already was. The scratch directory was then removed and `git status` showed only this file.

### Before and after

Before the first keychain was created (`security default-keychain; security list-keychains -d user; security list-keychains`):

```
"/Users/sloan/Library/Keychains/login.keychain-db"
---
"/Users/sloan/Library/Keychains/login.keychain-db"
"/Users/sloan/gt/.runtime/signing/gastown-signing.keychain-db"
"/Library/Keychains/System.keychain"
---
"/Users/sloan/Library/Keychains/login.keychain-db"
"/Users/sloan/gt/.runtime/signing/gastown-signing.keychain-db"
"/Library/Keychains/System.keychain"
"/Library/Keychains/System.keychain"
```

After both throwaway keychains were deleted, the same three commands were run again and `diff` against the saved first output printed nothing:

```
"/Users/sloan/Library/Keychains/login.keychain-db"
---
"/Users/sloan/Library/Keychains/login.keychain-db"
"/Users/sloan/gt/.runtime/signing/gastown-signing.keychain-db"
"/Library/Keychains/System.keychain"
---
"/Users/sloan/Library/Keychains/login.keychain-db"
"/Users/sloan/gt/.runtime/signing/gastown-signing.keychain-db"
"/Library/Keychains/System.keychain"
"/Library/Keychains/System.keychain"
```

(`---` separates the three commands here; the saved files used `== default`, `== list user` and `== list all` headings. The real signing keychain was already in the search list before the run.)

### How to reproduce

Run from a scratch directory under a worktree; `KC` is the throwaway, the password is made up.

```
KC=$PWD/probe.keychain-db; PW=throwaway-not-a-secret
security create-keychain -p "$PW" "$KC"
security add-generic-password -a probe -s probe-svc -w data-made-up "$KC"
oracle() { security find-generic-password -s probe-svc -w "$KC" 2>&1 | head -1; }
security lock-keychain "$KC"; oracle                       # prints nothing: locked
echo "$PW" | security unlock-keychain "$KC"; oracle        # data-made-up: unlocked, exit 0
security lock-keychain "$KC"
echo wrong-made-up | security unlock-keychain "$KC"        # exit 51, still locked
(sleep 2; echo "$PW") | (security unlock-keychain "$KC" >/dev/null 2>&1 & p=$!; sleep 0.5; ps -o args= -p $p; wait $p)
                                                           # security unlock-keychain <KC>, no -p
security lock-keychain "$KC"
printf 'unlock-keychain -p %s %s\n' "$PW" "$KC" | security -i; oracle   # 1b
expect -c "spawn security unlock-keychain $KC; expect \"password to unlock*\"; send \"$PW\\r\"; expect eof"   # 2
security delete-keychain "$KC"
```

The no-password probe is the same with `security create-keychain -p "" "$NK"` and `security unlock-keychain "$NK" </dev/null`. The cgo probe is the 20-line program of candidate 5, run as `./gounlock-bin "$KC" -` with the password on stdin.

## Recommendation and the follow-up bead

Use candidate 1. Do not use the pty, the empty-password keychain, the partition list or cgo.

Follow-up bead the overseer would file (shaped, unassigned, `bird-filed`):

- **Title:** signing.Unlock: send the keychain password on stdin, not in argv.
- **Goal:** `signing.Unlock` runs `security unlock-keychain <keychain>` with the password on the child's stdin. `ps` shows no `-p` and no password.
- **Constraints:** `internal/signing` gets a stdin-capable exec seam beside `Exec` (a new type or an added parameter; callers in `internal/ominstaller` and `internal/binaryinstall` pass their own). The password never appears in argv, a log line or an error. A timeout still returns `ErrUnlockTimeout`. `internal/installer/sign.go`'s `security -i` path is separate and stays (or moves to the same call in its own bead).
- **Tests:** a fake seam asserts the argv is exactly `unlock-keychain <keychain>` (no `-p`) and the stdin bytes are the password plus a newline; the three existing argv-pinning tests are rewritten to this shape; an integration test under `//go:build darwin` unlocks a throwaway keychain by stdin, using the shape of `TestIntegrationSecurityReadsTheUnlockLineFromStdin`.
- **Out of scope:** the keychain password itself, how it is stored, and the partition list.
- **Owed live by the overseer after the install:** one real unlock through the installer with `ps` sampled during it.
