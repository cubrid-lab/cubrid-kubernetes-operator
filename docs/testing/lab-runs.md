# Lab runs

The multi-VM lab (#80) is shared, and a fault campaign on it disrupts every
CUBRID cluster it runs there. So a lab run is never started by a pull request
or by `make test-e2e`; it is started by hand through one entry point, which
refuses to run against the wrong cluster or next to another campaign (#122).

```sh
make test-lab LAB_CONTEXT=<kubectl context> LAB_TARGET=<lab name> LAB_COMMAND='<command>'
```

Before the command starts, the entry point (`test/lab/cmd/lab`):

1. Checks that the cluster behind `LAB_CONTEXT` carries the marker of
   `LAB_TARGET`: the ConfigMap `cubrid-lab/lab` whose `name` is the lab's
   name. A cluster without the marker is not a lab, whatever its context is
   called, and a marker with another name is another lab.
2. Copies that one context into a private kubeconfig and runs the command with
   `KUBECONFIG` pointing at it, and with `LAB_CONTEXT` and `LAB_TARGET` set.
   Switching contexts in another shell during the run does not redirect it,
   and the caller's current context is neither read nor changed.
3. Creates the Lease `cubrid-lab/campaign`, naming who started the run, where
   and when. Creating an object is atomic, so of two runs that start at once
   one is refused. A run that finds the Lease is refused and told who holds the
   lab.

When the command ends, also after Ctrl-C (the signal is passed on to the
command first), the Lease is deleted, but only while it still names this run.

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
it has. Lab scenarios that use the Operator already installed on the lab, and
starting a lab run from CI, are follow-ups of #122.
