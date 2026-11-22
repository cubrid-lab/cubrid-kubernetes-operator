#!/usr/bin/env python3
# Writes the test values into the directory given as argv[1], one file per case, each the value
# and one line feed, as a Kubernetes Secret key mounted as a file holds it. Each value mixes
# letters and digits with SQL comment or statement separator characters. Beside each value:
#   .changed  the last character replaced,
#   .prefix   the last character removed,
#   .cut      the value up to, not including, its first SQL special sequence, which is what the
#             engine would store if it ended the literal or the statement there,
#   .pattern  the value itself, one line, for grep -F -f.
# A login with .changed, .prefix or .cut must be refused. Prints only names and byte lengths.
import os, sys
out = sys.argv[1]
os.makedirs(out, exist_ok=True)
# (name, value, the special sequence where .cut ends)
cases = [
    ("semicolon",        "Semi7a;b9Kx2Lm4Np6Qr8St0Uv", ";"),
    ("double-dash",      "Dash3x--y5Hj7Kl9Mn1Pq3Rs5", "--"),
    ("double-slash",     "Slsh4p//q6Gh8Jk0Lm2No4Pq6", "//"),
    ("block-comment",    "Blk5/*c*/7Fg9Hj1Kl3Mn5Pq", "/*"),
    ("trailing-dashes",  "Tail6Rt8Yu0Io2Pa4Sd6Fg8--", "--"),
    ("quote-semi-dash",  "Qsd7Ab9';--Cd1Ef3Gh5Ij7Kl", "'"),
    ("comment-end-only", "Star8Mn0*/Op2Qr4St6Uv8Wx", "*/"),
]
for name, v, special in cases:
    raw = v.encode()
    assert 24 <= len(raw) <= 31, name
    assert all(0x20 <= x < 0x7f for x in raw), name
    assert special in v, name
    cut = v[:v.index(special)]
    near = {
        "": v,
        ".changed": v[:-1] + ("X" if v[-1] != "X" else "Y"),
        ".prefix": v[:-1],
        ".cut": cut,
        ".pattern": v,
    }
    for suffix, text in near.items():
        with open(os.path.join(out, name + suffix), "wb") as f:
            f.write(text.encode() + b"\n")
    print("%s bytes=%d cut_bytes=%d special=%s" % (name, len(raw), len(cut.encode()), special))
