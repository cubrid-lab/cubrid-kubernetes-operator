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
as for the shell. The target builds the entry point (`test/lab/cmd/lab`) into
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

When the command ends, the Lease is deleted, with its uid as a precondition:
a Lease that replaced it during the run is left in place.

Ctrl-C, SIGTERM and SIGHUP (a closed terminal) are handled for the whole run.
Before the command starts, one stops the run after the current step, releasing
the Lease if it was taken. While the command runs, each one is passed on once
to the command and everything it started, which run in a process group of
their own, so the command can clean up; a second Ctrl-C would make `go test`
and Ginkgo skip their cleanup. The Lease is released when the command has
ended. The exit code is the command's, or 128 plus the number of the signal
that ended it.

`LAB_DURATION` (default `2h`) is how long the run is expected to take. A
Lease past that time is not taken over, since whether its campaign still runs
cannot be told from the Lease; the refusal says so and gives the command that
removes it. Remove it only after making sure no campaign runs.

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
Kind cluster (`utils.RequireKindContext`, #440). Lab scenarios that use the Operator already installed on the lab, and
starting a lab run from CI, are follow-ups of #122.
