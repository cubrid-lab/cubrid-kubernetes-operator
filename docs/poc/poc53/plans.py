"""POC-53 trial plans. A trial's spec:
  stopped, master, remaining  members; h-0 has priority 1, h-1 2, h-2 3
  stop_slave  False for the control: the slave's heartbeat keeps running
  gap         seconds from the slave's `heartbeat stop` returning to the master's end, or
              "together": the master's end starts as the slave's stop starts
  end         graceful (`heartbeat stop`, `server stop`, exit) or kill (`docker kill`)
  holder      what stays connected to the master through its stop (holder.py), or none
  return_at   "after" (the slave returns after the watch) or seconds from its stop
  slave_return  restart (new container on the same volume) or start (`heartbeat start` in place)
  fresh       start from the saved data even if the last trial left a healthy cluster
  settle      seconds to wait between the healthy cluster and the fault
"""
FIELDS = ["stopped", "master", "remaining", "stop_slave", "gap", "end", "holder", "return_at", "slave_return", "fresh", "settle"]


def spec(s, m, r, end, gap=2, stop_slave=True, holder="none", return_at="after", slave_return="restart", fresh=False, settle=0):
    out = dict(stopped=f"h-{s}", master=f"h-{m}", remaining=f"h-{r}", stop_slave=stop_slave, gap=gap, end=end,
               holder=holder, return_at=return_at, slave_return=slave_return)
    if fresh:
        out.update(fresh=True, settle=settle)
    return out


def name(s, n):
    bits = ["control" if not s["stop_slave"] else f"stop{s['stopped'][-1]}", f"master{s['master'][-1]}",
            f"left{s['remaining'][-1]}", s["end"], f"gap{s['gap']}"]
    if s["holder"] != "none":
        bits.append(s["holder"])
    if s["return_at"] != "after":
        bits.append(f"back{s['return_at']}")
    if s["slave_return"] != "restart":
        bits.append("start-in-place")
    if s.get("fresh"):
        bits.append(f"fresh-settle{s['settle']}")
    return "-".join(bits) + f"-n{n}"


# Chained: each trial's master is the last trial's remaining member.
PERMUTATIONS = [(0, 1, 2), (0, 2, 1), (1, 0, 2), (1, 2, 0), (2, 0, 1), (2, 1, 0)]

PLANS = {
    "first": [(spec(0, 1, 2, "graceful"), 1), (spec(0, 1, 2, "kill"), 1)],
    "holders": [(spec(0, 1, 2, "graceful", holder="idle-session"), 1), (spec(0, 2, 1, "graceful", holder="open-txn"), 1),
                (spec(0, 1, 2, "graceful", holder="long-query"), 1)],
    # In this order, chained from a master h-2.
    # frozen-server stands in for a slow stop, whose cause in CI is not known, with the stopped slave returning at +30 s.
    "main": [(spec(*t, end, gap=g, holder=h, return_at=b, stop_slave=c), n) for t, end, g, h, b, c, n in [
        ((0, 2, 1), "graceful", 2, "writes", "after", True, 1),
        ((0, 1, 2), "graceful", 2, "frozen-server", "after", True, 1),
        ((0, 2, 1), "kill", 2, "writes", "after", True, 1),
        ((0, 1, 2), "graceful", 2, "frozen-server", 30, True, 1),
        ((1, 0, 2), "kill", 2, "none", "after", True, 1),
        ((1, 2, 0), "graceful", 2, "none", "after", True, 1),
        ((2, 0, 1), "graceful", 2, "none", "after", True, 1),
        ((2, 1, 0), "kill", 2, "none", "after", True, 1),
        ((2, 0, 1), "graceful", 2, "none", "after", False, 1),
        ((0, 1, 2), "kill", 2, "none", "after", True, 2),
        ((0, 2, 1), "graceful", 0, "none", "after", True, 1),
        ((0, 1, 2), "kill", 0, "none", "after", True, 1),
        ((0, 2, 1), "graceful", 5, "none", "after", True, 1),
        ((0, 1, 2), "graceful", 2, "frozen-server", 30, True, 2),
        ((1, 0, 2), "kill", 2, "none", "after", False, 1),
        ((0, 1, 2), "graceful", 15, "none", "after", True, 1),
        ((0, 2, 1), "kill", 5, "none", "after", True, 1),
        ((0, 1, 2), "graceful", 2, "none", "after", True, 2),
    ]],
    # What the write-load trial of "main" suggested, ahead of the rest of "main".
    "lag": [(spec(*t, end, gap=g, holder=h, return_at=b, stop_slave=c), n) for t, end, g, h, b, c, n in [
        ((0, 1, 2), "graceful", 2, "writes60", "after", True, 1),
        ((1, 2, 0), "graceful", 2, "writes60", "after", False, 1),
        ((1, 0, 2), "kill", 2, "writes60", "after", True, 1),
        ((1, 2, 0), "kill", 2, "writes60", "after", False, 1),
        ((2, 0, 1), "graceful", 2, "none", "after", False, 1),
        ((0, 1, 2), "kill", 2, "none", "after", True, 2),
        ((1, 2, 0), "graceful", 2, "none", "after", True, 1),
        ((1, 0, 2), "kill", 2, "none", "after", True, 1),
        ((0, 2, 1), "graceful", 0, "none", "after", True, 1),
        ((0, 1, 2), "kill", 0, "none", "after", True, 1),
        ((0, 2, 1), "graceful", 5, "none", "after", True, 1),
        ((2, 1, 0), "kill", 2, "none", "after", True, 1),
        ((2, 0, 1), "graceful", 2, "none", "after", True, 1),
        ((0, 1, 2), "graceful", 2, "frozen-server", 30, True, 2),
        ((1, 0, 2), "kill", 2, "none", "after", False, 1),
        ((0, 1, 2), "graceful", 15, "none", "after", True, 1),
    ]],
    # A member promoted soon after it started, against the same after a wait.
    "recent": [(spec(*t, end, fresh=True, settle=w), n) for t, end, w, n in [
        ((0, 1, 2), "graceful", 0, 1), ((0, 1, 2), "graceful", 75, 1),
        ((0, 1, 2), "kill", 0, 1), ((0, 1, 2), "kill", 75, 1),
        ((0, 1, 2), "graceful", 0, 2), ((0, 1, 2), "graceful", 75, 2),
    ]] + [(spec(0, 2, 1, "graceful", slave_return="start"), 1), (spec(0, 1, 2, "kill", slave_return="start"), 1)],
    # Repeats of what the earlier plans showed, each from the saved data.
    "again": [
        (spec(0, 1, 2, "graceful", holder="frozen-server", return_at=30, fresh=True), 2),
        (spec(1, 2, 0, "graceful", holder="writes60", stop_slave=False, fresh=True), 2),
        (spec(0, 2, 1, "graceful", holder="writes", fresh=True), 2),
        (spec(1, 2, 0, "graceful", holder="writes60", fresh=True), 1),
    ],
    # Chained from a master h-0: the two stops overlapping, other gaps, and the lowest priority stopped.
    "gaps": [
        (spec(2, 0, 1, "graceful", gap="together"), 1),
        (spec(0, 1, 2, "graceful", gap="together"), 1),
        (spec(0, 2, 1, "kill", gap="together"), 1),
        (spec(0, 1, 2, "graceful", gap=15), 1),
        (spec(0, 2, 1, "graceful", gap=0), 1),
        (spec(0, 1, 2, "kill", gap=5), 1),
    ],
}
