import re, subprocess

d = subprocess.run(
    ["git", "diff", "origin/main...HEAD", "--", "internal/"],
    capture_output=True, text=True,
).stdout

cur = None
hunks = []
for line in d.splitlines():
    if line.startswith("+++ b/"):
        cur = line[6:]
    elif line.startswith("@@"):
        hunks.append({"file": cur, "body": []})
    elif hunks and (line.startswith("+") or line.startswith("-")):
        hunks[-1]["body"].append(line)

with_env, dir_only, neither = 0, [], []
for h in hunks:
    body = h["body"]
    removed = [x for x in body if x.startswith("-")]
    for l in body:
        if not l.startswith("+"):
            continue
        if not re.search(r'(?:CommandWithEnv|CommandContextWithEnv|CommandWithPath|CommandContextWithPath)\(', l):
            continue
        if "os.Environ()" not in l:
            continue
        had_env = any(re.search(r'\.Env\s*=', x) for x in removed)
        had_dir = any(re.search(r'\.Dir\s*=', x) for x in removed)
        if had_env:
            with_env += 1
        elif had_dir:
            dir_only.append((h["file"], l.strip()))
        else:
            neither.append((h["file"], l.strip()))

print("added call sites passing os.Environ():")
print("  removed a '.Env ='  (env was explicit -> PRESERVE):", with_env)
print("  removed only '.Dir =' (env was nil -> MUST BE nil):", len(dir_only))
print("  removed neither (no Dir/env -> nil fine):", len(neither))
print("\n--- DIR-ONLY (regressions) ---")
for f, l in dir_only:
    print(f + "  ::  " + l[:110])
print("\n--- NEITHER ---")
for f, l in neither:
    print(f + "  ::  " + l[:110])
