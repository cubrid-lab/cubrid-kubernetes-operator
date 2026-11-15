#!/usr/bin/env python3
# Writes the test values into the directory given as argv[1], one file per case, each the value
# and one line feed, as a Kubernetes Secret key mounted as a file holds it. Prints only names and
# byte lengths, never a value.
import os, sys
out = sys.argv[1]
os.makedirs(out, exist_ok=True)
alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
def ascii_of(n):
    return ("Len%d-" % n + "".join(alnum[(i * 7) % len(alnum)] for i in range(n)))[:n]
cases = {
    # (a) ASCII at CUBRID's bound (31 bytes, docs/poc/poc64/out/05-length-probe.txt) and one over it.
    "a-len31": ascii_of(31),
    "a-len32": ascii_of(32),
    # (b) non-ASCII UTF-8: Latin-1 range, Hangul and CJK, 24 bytes.
    "b-utf8": "P\u00e4ssw\u00f6rd-\ud55c\uae00-\u5bc6\u7801",
    # (c) single quotes and backslashes, the bytes an SQL string literal treats specially.
    "c-quote-backslash": "Qu'o\\te\\\\d''x\\'y",
    # (d) UTF-8 at the bound: 10 three-byte characters and one ASCII (31 bytes), and 11 (33 bytes).
    "d-utf8-len31": "\ud55c" * 10 + "z",
    "d-utf8-len33": "\ud55c" * 11,
}
for name, v in cases.items():
    raw = v.encode()
    assert all(x >= 0x20 and x != 0x7f for x in raw), name
    with open(os.path.join(out, name), "wb") as f:
        f.write(raw + b"\n")
    # The value with its last character replaced, and the value without its last character:
    # a login with either must be refused, so a stored value that was cut or changed is seen.
    last = "X" if v[-1] != "X" else "Y"
    with open(os.path.join(out, name + ".changed"), "wb") as f:
        f.write((v[:-1] + last).encode() + b"\n")
    with open(os.path.join(out, name + ".prefix"), "wb") as f:
        f.write(v[:-1].encode() + b"\n")
    # Patterns for grep -F -f: the value itself, one line.
    with open(os.path.join(out, name + ".pattern"), "wb") as f:
        f.write(raw + b"\n")
    print("%s bytes=%d chars=%d has_quote=%s has_backslash=%s ascii=%s" % (
        name, len(raw), len(v), "'" in v, "\\" in v, all(x < 0x80 for x in raw)))
