# Lab runs

The multi-VM lab (#80) is shared, and a fault campaign on it disrupts every
CUBRID cluster it runs there. So a lab run is never started by a pull request
or by `make test-e2e`; it is started by hand through one entry point, which
refuses to run against the wrong cluster or next to another campaign (#122).

```sh
make test-lab LAB_CONTEXT=<kubectl context> LAB_TARGET=<lab name> LAB_COMMAND='<command>'
```

`LAB_COMMAND` runs as one `sh -c`, so all of it runs under the Lease,
including what follows `&&`, `;` or `|`. Make expands nothing in it; write it
as for the shell: write `$KUBECONFIG`, and do not double the `$`, since the
shell reads `$$KUBECONFIG` as its own pid followed by `KUBECONFIG`. The
command's standard input is `/dev/null`; it cannot read the terminal. The
target builds the entry point (`test/lab/cmd/lab`) into
`bin/lab` and runs it, which, before the command starts:

1. Copies that one context, with its credentials, into a kubeconfig only the
   user can read, and from then on reads the context from that copy only. The
   command runs with `KUBECONFIG` pointing at it, and with `LAB_CONTEXT` and
   `LAB_TARGET` set. Switching or redefining contexts in another shell during
   the run redirects neither the command nor the release of the Lease, and the
   caller's current context is neither read nor changed. The copy is removed
   when the run ends.
2. Checks that the cluster behind `LAB_CONTEXT` carries the marker of
   `LAB_TARGET`: the ConfigMap `cubrid-lab/lab` whose `name` is the lab's
   name. A cluster without the marker is not a lab, whatever its context is
   called, and a marker with another name is another lab.
3. Creates the Lease `cubrid-lab/campaign`, naming who started the run, where
   and when. Creating an object is atomic, so of two runs that start at once
   one is refused. A run that finds the Lease is refused and told who holds the
   lab.

While the run holds the lab, from the create until the release, it renews
the Lease every sixth of `LAB_DURATION` (default `60s`, so every 10 seconds),
by writing its `renewTime`. A renewal replaces the object the run last wrote,
by its uid and resourceVersion, so it fails if anyone else wrote the Lease;
the resourceVersion it returns is the one the next renewal and the release
carry. When the command ends, the Lease is deleted, with its uid and that
resourceVersion as preconditions: a Lease that replaced it during the run, or
that another holder wrote in place, is left in place.

The Lease is the lab's one reservation, shared with the other tools that run
campaigns there. The canonical campaign launcher
(`cubrid-lab/terraform-zadara-k8s`, `docs/operator-campaign-lease.md`) takes
the same `cubrid-lab/campaign` Lease for 60 seconds and renews it every 10
seconds, and refuses to start while any Lease exists in `cubrid-lab`; so
while a lab run holds the lab, the launcher does not start, and the other way
round.

A renewal or the release that meets a Conflict reads the Lease, since a write
of this run that the API server stored although kubectl failed (a timeout,
for example) leaves the run an old resourceVersion. When the Lease still
carries the run's uid and holder and a `renewTime` the run sent, the run
takes its resourceVersion and goes on. So an edit that keeps the holder and
`renewTime`, such as `kubectl annotate` or a change of `leaseDurationSeconds`,
is taken back as the run's, and the next renewal writes the run's spec over
it. Otherwise the Lease is lost: it was
written by someone else, or removed, and perhaps created again (the API
server reports a removed Lease as a Conflict on the uid, not as NotFound).
The run then deletes nothing, since the Lease is no longer its own.

A renewal that fails otherwise, for example while the API server cannot be
reached, is tried again at the next one. When none succeeds within
`LAB_DURATION` of the last one, even while one is still in flight, the Lease
may have ended and the next run may take the lab, so it is lost too. The
release is then still sent, with its preconditions, so a Lease left as the
run wrote it is removed and any other is left in place.

A run whose Lease is lost no longer holds the lab, and stops: it says why,
sends SIGTERM to the command's process group, and SIGKILL two seconds later
if anything of it is left, and exits 1. So after the last renewal, the
command can outlast the Lease's end by about those two seconds, plus the
difference between this host's clock and the API server's.

Ctrl-C, SIGTERM and SIGHUP (a closed terminal) are handled for the whole run.
Before the command starts, one stops the run after the current step, releasing
the Lease if it was taken. While the command runs, the first one is passed on
to the command and everything it started, which run in a process group of
their own, so the command can clean up; a second Ctrl-C would make `go test`
and Ginkgo skip their cleanup. The Lease is released, and the copy removed,
only when no process is left in that group: a `sh -c` without a trap ends at
once on a signal while what it started may still clean up with kubectl, and a
job left in the background still runs after the command. Until then the run
says it is waiting, and signals still reach the group. A job that a
non-interactive shell started in the background (`&`) ignores Ctrl-C, so a
repeated signal escalates: the second sends SIGTERM to the group, and the third
and any later one SIGKILL; the run says what it sends. A process that left the
group (`setsid`) is not waited for. The exit code is the command's, or 128
plus the number of the signal that ended it, or 1 when the Lease was lost.

`LAB_DURATION` is how long the Lease lasts without a renewal, in whole
seconds. A Lease ends its duration after its last renewal (`renewTime`, or
`acquireTime` when it has none). A Lease past its end is not taken over, since
whether its campaign still runs cannot be told from the Lease. When its holder
is a run of this entry point (`user@host/pid/time`), the refusal says so and
gives the command that removes it; remove it only after making sure no
campaign runs. Any other holder's Lease, such as the launcher's, is only
named: ask its holder, or follow the recovery of the tool that took it.

## Marking a cluster as a lab

Once per lab, by whoever operates it:

```sh
kubectl create namespace cubrid-lab
kubectl -n cubrid-lab create configmap lab --from-literal=name=<lab name>
```

The account that runs campaigns needs to read that ConfigMap and to create,
read and delete Leases in `cubrid-lab`.

## Not there yet

The entry point runs a command; it does not have scenarios of its own. The
e2e suite cannot be that command: it deploys the Operator and its CRDs and
removes them at the end, which on a lab would delete every `CubridCluster`
it has. It refuses to start there anyway, since its current context is not a
Kind cluster (`utils.RequireKindContext`, #440). A command that uses the
Operator already installed on the lab, such as the LAB00 SQL smoke
([installed-smoke.md](installed-smoke.md)), can run under it: the Lease it
takes is the `cubrid-lab/campaign` Lease that smoke's documentation asks the
launcher to hold. Lab scenarios beyond LAB00, and starting a lab run from CI,
are follow-ups of #122. How to reach the lab is in [lab-access.md](lab-access.md).
