# redact.py <secrets-dir>: copies standard input to standard output with every test password
# (each regular file of <secrets-dir>, without its trailing line feed) replaced by <redacted:NAME>.
import os, sys
d = sys.argv[1]
data = sys.stdin.buffer.read()
for name in sorted(os.listdir(d)):
    p = os.path.join(d, name)
    if os.path.isfile(p):
        v = open(p, "rb").read().rstrip(b"\n")
        if v:
            data = data.replace(v, ("<redacted:%s>" % name).encode())
sys.stdout.buffer.write(data)
