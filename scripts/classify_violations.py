import re

files = open('/tmp/violation_files.txt').read().split()
for f in files:
    with open(f) as fh:
        lines = fh.readlines()
    for i, line in enumerate(lines):
        if re.search(r'exec\.Command(Context)?\(', line) and ('"bd"' in line or 'bdPath' in line):
            window = ''.join(lines[i:i+12])
            has_dir = bool(re.search(r'\.Dir\s*=', window))
            has_env = bool(re.search(r'\.Env\s*=', window))
            tag = []
            if has_dir:
                tag.append('DIR')
            if has_env:
                tag.append('ENV')
            print(f"{f}:{i+1}: {' '.join(tag) if tag else 'simple'}")
