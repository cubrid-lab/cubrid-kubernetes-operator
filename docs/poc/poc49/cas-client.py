# A Broker client: connects through broker1 of the CUBRID container, inserts
# one row, commits and disconnects, again and again, and prints every error.
import os, sys, time
import pycubrid
host = os.environ.get("CUBRID_HOST", "poc49-hb")
n = ok = 0
end = time.time() + float(sys.argv[1])
while time.time() < end:
    n += 1
    try:
        c = pycubrid.connect(host=host, port=33000, database="appdb", user="dba", password="")
        cur = c.cursor()
        cur.execute("INSERT INTO t (v) VALUES (-1)")
        c.commit()
        c.close()
        ok += 1
    except Exception as exc:
        print(time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime()), "error:", exc, flush=True)
    time.sleep(0.05)
print("cas-client: attempts", n, "ok", ok, flush=True)
