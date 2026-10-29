# Roadmap

This roadmap describes the development plan for the CUBRID Kubernetes
Operator.

The project is experimental. Merged implementation, manual CUBRID POCs and
live Operator validation are separate states. Accepted design and POC issues
remain historical evidence; integration gaps are tracked by the STP IV issues
below rather than reopening the original work.

This file is the source of truth for changing scope and validation status.

## STP IV delivery stages

Season: **2026-10-05 to 2026-12-27** (see issue #77).

| Stage | Dates (2026) | Required outcome |
|---|---|---|
| Setup | Oct 05–11 | Scope, scenario contracts and lab settled; real single-database SQL smoke |
| HA alpha | Oct 12–Nov 01 | Three-member HA, RW/RO access, representative failures and backup/restore alpha |
| Recovery validation | Nov 02–29 | Write quarantine, rejoin, rebuild and mandatory scenarios validated |
| Release preparation | Nov 30–Dec 27 | Repeated runs, regression fixes, reproduced runbooks and reviewed release artifacts |

Dates are delivery targets, not evidence of completion. Conditional work
cannot displace required safety and recovery work. If required evidence is
missing, report a blocked release or narrower experimental result rather than
waiving a mandatory scenario to meet the date.

## v0.1 scope

### Required

The initial support claim is limited to one pinned CUBRID 11.4.x/amd64
configuration, one Kubernetes distribution and one CSI/StorageClass in the
three-VM lab (#80). Distribution/version/driver selection remains unverified
until that issue records the inventory and run evidence. Three VMs do not
establish physical-zone resilience.

| Capability | Completion evidence | Tracking issues |
|---|---|---|
| Non-root authenticated Instance Manager and single-node lifecycle | S00 real SQL without manual Pod setup | #81, #97–#99, #101 |
| Three-member HA, stable identity, real replication, redundant RW/RO access | S01–S02 SQL and role evidence; RW writes accepted, RO writes rejected | #82, #83, #105–#108 |
| Pod, VM, Broker and Operator failure recovery | S03–S06 confirmed faults, transaction histories and measured recovery | #85, #109–#111 |
| Managed write quarantine for ambiguous or stale HA | S07–S08 SQL checks on new and existing connections | #86, #112–#115 |
| Retained-data rejoin and explicit PVC-loss rebuild | S09–S10 roles/data converge; healthy data is not overwritten | #87, #116–#118 |
| Backup and restore into a new cluster | S11–S12 real artifacts, interrupted-operation handling and exact dataset verification | #88, #89, #119–#121 |
| Retention, authentication and voluntary-disruption policy | S13–S15 retention, authorization and node-drain evidence | #90, #91 |
| Operational Conditions, Events and metrics | Status agrees with SQL/fault evidence; unknown observations stay unknown | #92 |
| Reproducible validation and reviewed release | S00–S15 on the candidate, repeated core faults, soak results, non-author runbook reproduction and traceable artifacts | #79, #84, #94, #95 |

Scenario contracts, exact deadlines and independent pass/fail oracles are
specified in #79 before acceptance tests are implemented. That issue defines
acknowledged, failed and unknown client outcomes, observed RPO/RTO and the
required artifacts. No zero-data-loss or automatic-failback guarantee is
inferred from a POC or a passing unit test.

### Conditional

- Compatible image/configuration rolling update: S16 / #96, only after the
  required recovery gates pass. Existing sequencing code and POC-9 do not by
  themselves establish supported live updates.
- Disk-full testing: S17, only with a volume whose size limit can be enforced
  and a documented recovery path. No disk-full support claim without evidence.
- Managed remote RW/RO Broker endpoints for a standalone cluster (#286): an
  extension of the S00 gate, claimed only with S00's remote SQL and `-ro`
  refusal evidence. `-ro` of a standalone cluster is read-only access to its
  one server, not a replica (POC-33).

Keep unvalidated conditional work on the roadmap, outside v0.1 support claims.

### Future

CUBRID engine-version migration, non-promotable read replicas, a standalone
restore workflow CR, broader engine/Kubernetes/CSI/architecture matrices and
multi-zone/provider resilience are outside the initial delivery. New features
remain proposals until scheduled; required validation takes precedence.

## Implementation and validation inventory

Baseline: main `a6cadd0` (2026-10-05); review/update these rows when a relevant
PR merges or candidate evidence arrives. The table records existing code and
POC history, not live support certification.

| Area | Existing implementation/evidence | Live Operator validation |
|---|---|---|
| Foundation | Kubebuilder scaffold, v1alpha1 APIs, generated resources, unit/envtest and manager E2E skeleton | S00 runs in CI on Kind (linux/amd64): a standalone cluster on the real 11.4 image becomes Ready, answers SQL through `csql` and keeps a row across a Pod deletion. Its two read-write and two read-only Brokers point at the one server (#286): a client with a CUBRID driver in another namespace commits through `-rw`, reads the row through `-ro` and is refused a write there with `-581` (POC-33), and `BrokerReady`, `WriteEndpointReady`, `ReadEndpointReady` and `RoutingReady` (`StandaloneServerReady`) are `True`. One member only; no HA, backup or failure scenario, and no Broker or server failure of a standalone cluster |
| Runtime | Instance Manager handlers and derivative image definition; non-root entrypoint checked with the real 11.4 image under podman, which was linux/amd64 under emulation: command output only, no timing evidence (start, stop, restart, recovery start); a database that carries the ownership marker, which the entrypoint neither creates over nor starts, is reported in the role answer and in `status.instances[].unfinishedDatabase` with an Event, as waiting for the next operation or refused (#315: unit/envtest; not run with the real image, on Kind or in the VM lab) | Runs as a non-root Pod under the restricted Pod Security Standard on Kind (S00). The Pod gets the operation store, staging roots and the cluster's object-storage settings (envtest; the HA bootstrap on Kind seeds its members through them, see the HA row) |
| HA and Broker | Role discovery, primary resolution, Broker resource/config generation, the generated `cubrid_ha.conf` member list and the per-member alias Services (unit/envtest); manual engine POCs | The HA bootstrap runs on Kind in CI with the real image: one member creates the database, the other two are seeded through object storage (a mock in CI) and join as slaves, one row replicates to both, and the cluster reports Ready with one primary. Two read-write and two read-only Brokers run as non-root Pods; a client with a CUBRID driver commits through `-rw`, reads the row through `-ro` and is refused a write there. Scenarios S01 and S02 of `docs/testing/scenario-contract.md` run on the same cluster with the JDBC workload client: a recorded history of 121 operations is checked against the rows of every member and of both Services, a write through `-ro` is refused by the read-only Broker itself (error `-581` on the pinned image, POC-22; a failure to reach it fails S01, #366), a schema change and rolled-back transactions replicate, and a replaced slave Pod holds everything again. Evidence: `summary.json` and the scenario files of the `E2E Tests` workflow. Scenario S03 runs there too, in three variants: the master Pod is deleted with a grace period, and without one both when the master is the first member and when it is not, while a client writes through `-rw`; one member is master afterwards, the client's writes are acknowledged again, and no acknowledged write is missing on any member; the run fails on two members reporting themselves active masters in two consecutive sweeps, during the failover and through the stable period, and on a new Pod or container restart of a member the fault did not touch (#368; the oracle is unit-tested, and no two-master state has been observed). A slave takes over, except when the first member is deleted without a grace period: its Pod is back within a second and CUBRID makes it the master again (POC-19). Scenario S05 runs there in three variants: the Broker process of a read-write Broker Pod is killed, one read-write Broker Pod is deleted, and all of them are deleted, while four clients write; the Brokers come back, every client's writes are acknowledged again, no acknowledged write is missing and the master does not change. Scenario S06 runs there in both variants: the Operator Pod is killed while clients write, and the master Pod is deleted while the Operator's Deployment is scaled to zero; SQL is not interrupted by the restart, CUBRID fails over without the Operator, and the returning Operator names the new master and restarts no member. Scenario S14 runs there as well: every protected endpoint of the Instance Manager refuses a caller without the token, also one that claims a loopback origin in its headers, and the token is not shown in status, events, descriptions or logs. No VM failure and no live interruption of the bootstrap is covered |
| Recovery and safety | State/role decision code; a `ReplicationHealthy` condition that reports a slave which has log pages waiting and applies nothing, or whose applier failed (unit/envtest, and a non-contract e2e step on Kind with the real image that puts a slave into the POC-21 state, `test/e2e/replication_stall_test.go`, #229, #352; nothing acts on the condition); `Ready` and `cubrid_cluster_ready` stay off while more than one fresh, correctly identified local active-master claim is observed, including claims with conflicting peer lists (#339: parser/HTTP unit tests and envtest; the engine occurrence remains unverified under #115 and #373), and a reconcile that ends before the roles are observed reports them `Unknown` instead of keeping an earlier primary (#340) (envtest; no real two-master state has been produced); the HA bootstrap copies each member after the first peer from the member observed as master, with a backup taken on it, and waits while that master is not known (#281: unit/envtest, and the engine's behavior in POC-23; a change of master during a bootstrap has not been produced on a cluster); a restarted applier process does not inherit the age of a stall (#360: unit tests on POC-30; no applier restart has been produced on a cluster); a slave whose copy of the master's log has not grown for the window while the master's own log ends pages past it is reported as `LogCopyStalled` (#623: unit tests on the readings of POC-42, and one VM-lab run of POC-41's TCP 1523 cut with the installed Operator at `fb6f345`, which reported the stuck slave from the end of its 60-second window, 83 s after the cut, until a copier restart turned it `True` again, POC-42; nothing acts on it); a database member takes a member lock on its volume before it writes there and holds it for the container's whole life, and creates, starts and recovers nothing, and does not launch its Instance Manager, while another container of the member holds that lock, at any stage of its start, or another process still holds a CUBRID lock on the volume, such as the old container of a force-deleted Pod (#472, #475: unit and entrypoint tests, and containers of the real image on one Docker volume, POC-29 and POC-31, including a server start held open before its lock; not run on Kind or the VM lab); manual failover and split-brain POCs | S03 on Kind: while the Operator reported `PrimaryResolved` and `RoutingReady` as not `True` after a master Pod deletion, writes through `-rw` went on being acknowledged, so the write quarantine is reported and not enforced. Enforced SQL quarantine, rejoin and rebuild require #112–#118 |
| Network access | Per-cluster ingress NetworkPolicies for the DB and Broker Pods (generated objects checked in envtest) | S14 on Kind checks that the DB Pods drop connections to ports 9090 and 1523 from another namespace under Kind's network plugin, and the HA scenarios run with the policies in place. On the VM lab, one connection to a Broker port from another namespace was refused once (2026-10-21 run below); the DB ports and the lab's network plugin are not checked there |
| Backup and restore | CubridBackup API/controller (object storage is the only destination with an execution path), Instance Manager artifact/restore paths, `restoreslave` when a member is seeded from a running master (POC-14), and a restore that fails while heartbeat runs without a confirmed role (#456) (unit/envtest); a member is not ready while its operation store keeps an end it could not write (#473: unit tests with an unwritable store directory; not run with a full volume on a cluster); a backup whose recorded operation its target no longer holds a record of ends `Failed` (`RecordedOperationLost`) after 30 minutes, bounded across operator restarts and never started again, and so does a recovery whose recorded restore operation its target no longer holds a record of, without a new attempt (#457: envtest with a client that answers the missing record; no operation store has been lost on a real member); a backup is taken on a standby only on fresh evidence that it applies the resolved primary's log without failures or a stall past the window, the one with the fewest pages waiting first, and not on one whose copy of that log was seen not growing for the window while pages of it waited (#291: unit/envtest; the copy rule: unit tests only, and a standby whose copy or master log end was not read stays eligible; no long transaction, stuck applier or stuck copier has been produced on an engine for this choice); a restore refuses, before any download, registration or command, an artifact that is not a full backup, and a seed restore one not taken on the named master of the same cluster (#276: unit/envtest with fixture manifests; no real artifact of another source has been restored); a backup's manifest records the engine version its member reads from `cubrid_rel`, a backup on a member whose engine is unknown or not of the declared series is refused before `backupdb`, and a restore is refused unless the restoring member's engine, the declared version and the manifest's version are of one series, and data an earlier attempt restored is not taken over on an engine of another series; a seed backup, seed restore or recovery restore whose member was last observed running another series than `spec.version` waits as `EngineVersionMismatch` and uses up no attempt (#276, ADR-0011: unit tests with a stand-in `cubrid_rel` and envtest; not run with the real image, and no restore across patch versions of a series); `databases.txt` and ownership markers are replaced whole with file and directory syncs, and the manager's writes of `databases.txt` are serialized, also against the `cubrid createdb` of an HA bootstrap, which registers its database there itself (#298: a child process killed by the kernel inside the write leaves the earlier content, and a registration made while a stand-in createdb runs keeps both lines, unit tests; not run with the real createdb); operation records, already renamed into place, are also directory-synced, and a new record whose sync failed is removed (#298: unit test with an injected sync failure; no power loss has been produced); the leftover of a failed restore or bootstrap is unregistered while its marker still names the operation, then moved aside under `.reclaim-<db>` and removed there, so a removal stopped partway leaves nothing under the database's name for the next attempt to refuse (#298: unit tests with a removal stopped partway and each intermediate state; no crash has been produced); standalone restore requests for another configured database are refused before work, and completed-restore resume keeps its marker and data on that configuration mismatch (#301: unit tests; not run on a real database); a failed or cancelled backup that removes its staging only once no process holds a file in it, and keeps it unused when one still does after 30 s (#469: unit tests with a holder process outside the utility's group; not run against a real `cub_server`); manual engine POCs. The HA bootstrap's seeding runs `backupdb`, the upload and `restoreslave` on Kind against an S3 mock (see the HA row); no e2e test creates a CubridBackup or a recovery bootstrap; on the VM lab, S11 `complete` takes a real CubridBackup after LAB01 and verifies its artifact independently, and S12 `exact` restores that artifact into a new cluster and checks it against the known point (see below) | Real workflow, interrupted recovery and dataset checks require #88 and #119–#121 |
| Hardening | Readiness probes run at most one status command at a time, and concurrent requests share its answer (#301, #494: unit and race-detector tests, not exercised with a real kubelet on a cluster); a manager started with `--watch-namespace` caches and reconciles only that namespace (#498: envtest with a namespace-Role-only identity, primary and Secret watches; no live candidate rollout on the VM lab, and baseline/candidate orchestration and CRD compatibility are not covered); the manager caches only Secrets and Pods with the operator's managed-by label and refuses to adopt an unlabeled `<cluster>-im-token` Secret (#282: envtest; memory use is unmeasured, and caches of the other owned kinds and the unused permissions are not yet narrowed); update sequencing, auth (a generated Instance Manager token per cluster, #270, and its rotation with an operator-side overlap that also moves clusters off the shared token of older versions, #324, and the report of a member that refuses the token apart from an unreachable one, #325: unit/envtest), metrics and Events code; a read-only observer that records a cluster's status, Events and the manager's Prometheus series from outside the cluster, and fails on a gauge that contradicts its Condition (#546: unit tests; two 5-minute runs on the VM lab against `ha` with the manager Pod deleted, [record](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/546#issuecomment-6364673645); no S04, S06 or S07/S08 verdict yet) | Placement/retention/PDB, security and operational accuracy require #90–#92; the token rotation and the move off a shared token are an e2e scenario on Kind (S14 `token-rotation`, `token-migration`, #343) and have not run in the VM lab, and a refused token has not been reported by real members; automatic Pod replacement for updates is switched off until its gates exist (#96, #197, #198): the manager never sets `AutomaticReplacement`, nothing sets the `FencingRequired` Condition that the update planner checks, and no PodDisruptionBudget is created or read (code inspection of `cmd/main.go` and `internal/controller/update.go`) |
| Release | Lint, unit/envtest and manager E2E workflows; `make test-lab`, an explicit lab entry point that runs one command while it holds the `cubrid-lab/campaign` Lease, on a pinned kubeconfig and context, and releases it only with uid and resourceVersion preconditions once the command's process group is empty (#441: unit tests with fake kubectl and a throwaway API server; on the VM lab it held the Lease for one LAB00 run and for one session of manual Pod replacements on 2026-10-21, and released it, [record](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6328187210); it renews the Lease every sixth of `LAB_DURATION`, stops the command when the Lease is lost, and escalates a repeated signal to SIGTERM and SIGKILL, #525: unit tests with fake kubectl; on the VM lab on 2026-10-23 it renewed the 60-second Lease through a 3-hour hold and three shorter runs and released it each time, records on [#122](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6364164038) and [#546](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/546#issuecomment-6364673645); losing the Lease was not exercised there) | Real-DB lane, lab gates, candidate artifacts and runbook reproduction require #122–#127; the demo (#128) is a post-v0.1 follow-up, not a release gate |

The installed-Operator connection command (#489, under #122) creates a fresh
namespace, checks declared and actual runtime identities, commits/reads SQL,
and confirms UID-protected namespace cleanup. Its `lab-installed-smoke` / LAB00
baseline [passed with recorded command and evidence](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/489#issuecomment-6257627016)
on a dedicated Kind v1.37.0/containerd 2.3.4 with CUBRID
11.4.6.1963 and locally built images from main `20afeaa`; wrong-target,
existing-namespace and SIGTERM/cleanup cases were also checked. Unit/race and
API precondition tests cover its safety boundaries.

The first recorded VM-lab LAB00 connection smoke
[passed on 2026-10-19](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/492#issuecomment-6269706537)
after a manual deployment of main `701b427deb22d7fc664ce00812db57bd7695a5a8`,
using published development images from
[run 39253773768](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39253773768).
The manager deployed by the private GitOps owner was observed Running/Ready
at digest `sha256:cff4fb2623ed7fd18250d4cb3ed763e648b70f9c6a572283c908a2c052668431`;
the observed Instance Manager runtime matched
`sha256:1b228d401e1b54942af098fd73da5c951e1a4fb5d9a7916af5b562300aa854cd`.
The smoke ran from `2026-10-19T14:20:40.635937212Z` to
`2026-10-19T14:21:46.805417516Z` on Kubernetes `v1.36.5+k3s1`, CUBRID
`11.4.6.1963`, four Ready nodes and node-local `local-path` storage (not CSI).
Both SQL commands produced clean outputs; LAB00 reported `pass`,
`passed=true`, `problems=[]`, `cleanupSucceeded=true` and `neverEvents=[]`.
The guarded lab launcher, private self-hosted workflow acceptance and
new-node registry access remain under #80/#122. These connection results are
separate from S00 installation or S00–S15 qualification, and do not establish
CSI or VM-fault behavior. Namespace cleanup does not certify deletion of
retained backend volumes.

The three-member HA run with an installed Operator (`lab-installed-ha` /
LAB01, [installed-ha.md](docs/testing/installed-ha.md), #546) creates an HA
cluster in a fresh namespace, waits for a subset of S01's starting state,
runs the SQL workload client and checks the data rules on every member and
both Services. On 2026-10-24 it passed twice on the VM lab (manager
`d6df2ac`, k3s `v1.36.5+k3s1`, CUBRID `11.4.6.1963`, `local-path`), once
from the final harness commit; it needed the lab's object store to admit
run namespaces (terraform-zadara-k8s #51), and a first attempt before that
change failed at the seed upload. LAB01 injects no fault and is not an S01
result at the VM-lab level.

S06 `restart` runs on top of it (`lab-installed-ha-s06`, `-fault
operator-restart`, #546): two clients write through `-rw` while the installed
manager's Pod is deleted without a grace period, and the replacement must
write a status of its own, under the leader Lease, naming the same master
within `operator_resync_limit`. On 2026-10-24 it passed once on the VM lab
from the reviewed harness: the replacement's status came 23.3 s after the
fault, no client operation failed, no member restarted, and the data rules
held; the observer ran alongside and found no contradictions. An earlier
run passed before the Lease and clock checks existed, and one failed on the
harness's own client-stop check, since fixed. One run is not a baseline, and
the applier fail count, which the Kind step checks, is not checked there.

S11 `complete` also runs on top of LAB01 (`lab-installed-ha-s11`,
`-backup-bucket`, #88): after a marker row, a real `CubridBackup` writes to
the lab's object store, and the run downloads the artifact through its own
port-forward and client and checks the manifest digest, its identity and the
sha256 and size of every object, and that neither credential appears in the
backup's status, the manifest or the namespace's Events. On 2026-10-25 it
passed four times on the VM lab (manager `d6df2ac`), the last from the merged
harness: each backup was taken on a standby and `Completed` in about 12 s,
with one object of 5,256,192 bytes verified. The Operator's logs were not
checked for credentials in those runs, and the interrupted variants are not
run. Since #621
the manifest and `status.artifact.cubridVersion` carry the engine version the
backup's member observed, and the run requires it to be the version
`cubrid_rel` reported on the master, of the `spec.version` series; until the
harness was changed to that, S11 failed on manager `fb6f345` (once, on
2026-10-28). With the change it passed once on the VM lab on 2026-10-28
(manager `fb6f345`): `Completed` on a standby in 12.0 s, both versions
`11.4.6.1963`.

Every S11 variant that takes a backup (`complete`, `missing-destination`,
`denied-credentials`, `signature-mismatch` and `interrupted-upload`) now also
searches, through `pods/log`, the installed Operator's log and the Instance
Manager's log of the Pod the backup ran on for the seed Secret's keys and the
variant's own keys (#616). A Pod that existed before the variant started is
read from one minute before it, and a node clock skew over a minute is not
measured; the member Pod of a cluster the variant created is read whole.
Each read is bounded to 16 MiB per container instance and includes the
previous instance of a restarted container. A key found fails the variant. A
log that cannot be read, a read the bound truncated, and a container that
restarted while its log was read block it. `pods/log` returns only the
container's current log file, so a log the kubelet rotated within the window
is searched only from the rotation onward; that is not reported as a
truncation. The Operator's log before the window and the Instance Managers
of other members are not searched. Unit tests cover it with a fake log
source. On 2026-10-29 it ran on the VM lab (manager `fb6f345`) in three
passing runs: S11 `interrupted-upload` and S11 `denied-credentials` with an
earlier harness of the same change (`acba678`, which read every log from
`sinceTime`), then S11 `signature-mismatch` with the final one (`f535c40`),
each after S11 `complete`. Every S11 step read the manager's log (1,510 to
13,695 bytes) and its backup member's log (2,796 to 8,094 bytes), none
truncated, no container restarted, and no key was found; in the last run the
new member `s11-signature-0` was read whole (6,217 bytes). The S11 lab
results recorded before those runs in this section were made without the log
search.

S11 `missing-destination` follows S11 `complete` on the same run
(`lab-installed-ha-s11-missing-destination`, `-backup-missing-destination`,
#616): the run confirms with its own client that a bucket named after the run
does not exist, requests a backup to it, and requires it `Failed` with
`BackupFailed` and the Instance Manager's `upload failed: put <bucket>/...`
within `backup_limit`, never `Completed` and without `status.artifact`, the
bucket still absent at the end, the source unchanged and no credential in a
status read or an Event; it deletes the `CubridBackup` it created. On
2026-10-28 it passed once on the VM lab (manager `fb6f345`, after S11
`complete` on the same run): the backup was `Running` and then `Failed` 12.0 s
after the request, with the store's answer `The specified bucket does not
exist` (SeaweedFS); the bucket was still absent with no object, no credential
was in 5 statuses or 81 Events, and the run deleted its `CubridBackup`. One
run is not a baseline; the target member's staging directory afterwards and
the Operator's and Instance Manager's logs were not checked. Other invalid
destinations and an interrupted upload are not covered.

S11 `denied-credentials` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-denied-credentials`, `-backup-denied-credentials`,
#616): the run generates keys, confirms with its own client that the store
refuses them, creates a standalone cluster whose object store Secret holds
them, and requires its backup to the backup bucket `Failed` with
`BackupFailed`, `upload failed: put <bucket>/...` and the store refusing the
keys within `backup_limit`, never `Completed` and without `status.artifact`,
nothing under its prefix at the end, the source unchanged and neither the
seed nor the denied keys in a status read or an Event; it deletes the backup,
the cluster and the Secret it created. It is lab-only. On 2026-10-29 it
passed once on the VM lab (manager `fb6f345`, after S11 `complete` on the
same run): SeaweedFS refused the generated keys to the run's own client, the
standalone cluster was `Ready` 45 s after its creation, and its backup was
`Running` and then `Failed` 12.0 s after the request, with the store's answer
`The access key ID you provided does not exist in our records.`
(`access-denied`); nothing was under the prefix at the end, no key was in 22
statuses or 114 Events, and the deletions of the backup, the cluster and the
Secret were accepted. One run is not a baseline; the target member's staging
directory and the Operator's and Instance Manager's logs were not checked.
Only an unknown access key was tested there; the seed Secret's access key
with a wrong secret key is S11 `signature-mismatch`.

S11 `signature-mismatch` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-signature-mismatch`, `-backup-signature-mismatch`,
#616): it runs the steps of S11 `denied-credentials` with the seed Secret's
access key with a wrong secret key, random and generated by the run, and
requires the store to refuse them to the run's own
client with `SignatureDoesNotMatch` before anything is created, and the
upload's answer to carry that code's message; the backup's verdict, the
checks at the end and the cleanup are the same. It is lab-only. On
2026-10-29 it passed once on the VM lab (manager `fb6f345`, after S11
`complete` on the same run): SeaweedFS refused the mismatched keys to the
run's own client with `SignatureDoesNotMatch`, the standalone cluster was
`Ready` 63 s after its creation, and its backup was `Running` and then
`Failed` 12.0 s after the request, with the store's answer `The request
signature we calculated does not match the signature you provided.`;
nothing was under the prefix at the end, no key was in 28 statuses or 117
Events, and the deletions of the backup, the cluster and the Secret were
accepted. One run is not a baseline; the run's binary was built before the
upload's answer was required to carry the code's message, and the answer it
recorded does. A key without permission on the bucket is still not covered.

S11 `interrupted-upload` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-interrupted-upload`, `-backup-interrupted-upload`,
#616), in a run namespace that enforces Pod Security `privileged` with
`warn` and `audit` at `restricted`: the run creates a standalone cluster with
the run's own Secret, limits its member's traffic to the object store
to 256 kbit/s from an ephemeral container in its Pod (to the Service's
ClusterIP, or to the one ready endpoint of a headless Service such as the
lab's), requests a
backup to the backup bucket, and once 512 KiB have passed the limit resets
that traffic in the Pod, confirmed by the reset rule's counter. The backup
must end `Failed` with `BackupFailed` and `upload failed: ` within
`backup_limit`, never `Completed` and without `status.artifact`, with no
`manifest.json` under its prefix at the end (objects left there are recorded
as a partial artifact), the source unchanged and no seed key in a status read
or an Event; the run removes both faults and deletes the backup and the
cluster it created. A backup that ends before the cut is confirmed, a fault
not seen removed, and a store address that changed leave it blocked. It is
lab-only. On 2026-10-29 it passed once on the VM lab (manager `fb6f345`,
after S11 `complete` on the same run): the store was the headless endpoint
`seaweedfs-0` at port 8333; 560,610 bytes had passed the limit (about
32 KB/s) when the cut went in during `Uploading`, confirmed by 4 rejected
packets; the backup was `Failed` 11.2 s after the cut and 33.0 s after the
request, with the store client's retry answered `connect: connection
refused` (`reset-or-refused`); nothing was under the prefix at the end, the
backup was still `Failed` without an artifact after the faults were seen
removed, no key was in 28 statuses or 118 Events, and the deletions of the
backup and the cluster were accepted. One run is not a baseline. The reset
is local to the member; a silent drop, a store that comes back during the
backup and a multipart upload are not covered, nor are the member's staging
directory and the Operator's and Instance Manager's logs.

S11 `operator-restart` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-operator-restart`, `-backup-operator-restart`,
#617): the run requests a second backup of the HA cluster to the backup
bucket under its own prefix and, once a reading shows it `Running` or
`Uploading` with an `operationRef`, deletes the installed manager's Pod
without a grace period, as S12 `operator-restart` does. The replacement must
run the same image and hold the leader Lease within `operator_resync_limit`;
the backup must end `Completed` within `backup_limit`, with
`status.completedAt` not before the second of the replacement's Lease
`acquireTime` (otherwise the old manager, still running and holding the
Lease, completed it, which blocks the variant), the same `operationRef`,
target and role in every reading from the placing one on, an artifact that
passes S11 `complete`'s verification, and exactly one attempt prefix with
one manifest under the variant's prefix, also when listed again at the end;
the source must be unchanged, and no key may be in a status read, the
manifest, an Event, the deleted manager's log as read right before its
deletion, the replacement's log or the target member's log. The manifest's
`createdAt` is recorded against the deletion (`uploadStartedBeforeRestart`)
as evidence, not a verdict. A backup that is not seen in progress with an
operation before it ends leaves it blocked, and so does `Failed` with
`RecordedOperationLost`, the documented escalation; `Failed` for another
reason fails it. The run deletes its `CubridBackup` and leaves the artifact.
The backup controller predicts `Completed` under the same operation: it
records `operationRef` with `Running`, and a replacement only polls that
operation. Unit tests cover it with fakes. On 2026-10-29 an earlier harness
of the change, which placed the restart on `Running` only and had neither
the `completedAt` check nor the second listing, passed once on the VM lab:
`Running` with its operation on `ha-1` (a slave) was read at 11:29:23.34
UTC, the manager Pod was deleted at 11:29:23.44, the replacement took the
Lease at 11:29:59.446, and `Completed` was read at 11:29:59.80 with
`completedAt` 11:29:59. The manifest's `createdAt` was 11:29:21, so the
Instance Manager had started its upload before the restart: the replacement
discovered and completed an operation already uploading, or done, there, not
one restarted in the middle of `backupdb`. The current harness then passed
once on the VM lab the same day (manager `fb6f345`): `Running` with its
operation on `ha-1` (a slave) was read at 12:02:23.17 UTC, the manager Pod
was deleted at 12:02:23.23, the replacement took the Lease at 12:02:44.686
(24.4 s after the deletion), and `completedAt` was 12:02:44, so the
replacement wrote `Completed`; one attempt prefix held the manifest and one
object of 5,256,192 bytes, which verified, at both listings, and no key was
in the old or new manager's log or `ha-1`'s. Its manifest's `createdAt` was
12:02:21, again before the restart. Both runs therefore show a replacement
completing an operation the old manager left unreported, and neither a
restart during `backupdb`, which needs a backup that runs longer than the
lab's. Two runs are not a baseline. The lab's manager is shared:
deleting its Pod briefly pauses reconciliation of every other cluster and
backup there.

S11 `instance-manager-restart` also follows S11 `complete`, in a run of its
own (`lab-installed-ha-s11-instance-manager-restart`,
`-backup-instance-manager-restart`, #617), in a run namespace that enforces
Pod Security `privileged` with `warn` and `audit` at `restricted`: the run
creates a standalone cluster with the run's own Secret, limits its member's
traffic to the object store to 256 kbit/s as S11 `interrupted-upload` does,
requests a backup to the backup bucket, and once 512 KiB have passed the
limit while the backup is `Running` or `Uploading` with an operation, kills
the member's Instance Manager with `SIGKILL` as S12
`instance-manager-restart` does, confirmed by the database container's end
with exit code 137 on the same Pod. The limit stays until the backup ends.
The backup must end `Failed` with `BackupFailed` and the Instance Manager's
restart reason within `backup_limit`, never `Completed` and without
`status.artifact`, under the same `operationRef` from the placing reading
on, with no `manifest.json` under its prefix at the end (objects left there
are recorded as a partial artifact); the member's container must restart
exactly once, and a reading of the cluster after the restarted container
was ready must show `Ready`, within the run's ready limit from the kill; a
container that restarted before the kill, for another reason, leaves it
blocked; the source and the installed manager must be unchanged, and no key may be
in a status read, an Event, the manager's log or the member's log, the
killed Instance Manager's previous container instance included. A backup
that ends before the kill is placed, a kill that is not confirmed, and
`Failed` with `RecordedOperationLost` leave it blocked; `Failed` for
another reason fails it. The code predicts `Failed` for the restart: the
manager fails every unfinished operation when it starts
(`reconcileOnStart`), the Operator maps that onto the backup and never
reconciles a `Failed` backup again, and the manifest is uploaded last. Unit
tests cover it with fakes. On 2026-10-29 an earlier harness of the change,
without the check of a restart before the kill, the from-kill ready bound
and `readyTransitionAt`, passed once on the VM lab (manager `fb6f345`, after
S11 `complete` on the same run): with 605,186 bytes through the limit and
the backup `Uploading`, the Instance Manager of `s11-imrestart-0` was killed
at 13:35:20.63 UTC (exit 137; the container's restart count was 0 before
the kill and its instance had started at 13:33:29, before the request at
13:34:59, so no earlier restart had ended the operation); the container was
back 3.3 s later, the cluster read `Ready` 15.4 s after the kill, and the
backup was `Failed` 15.4 s after the kill with `BackupFailed: manager
restarted while operation was in progress`, under its one operation;
nothing was under its prefix at the end, and no key was in the manager's
log or in either instance of the member's log. One run is not a baseline,
and the current harness has not run on the VM lab. A kill during
`backupdb` itself and the member's staging directory (#636) are not
covered.

S11 `overlapping` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-overlapping`, `-backup-overlapping`, #617), in a run
namespace that enforces Pod Security `privileged` with `warn` and `audit` at
`restricted`: the run creates a standalone cluster with the run's own
Secret, limits its member's traffic to the object store to 256 kbit/s as
S11 `interrupted-upload` does, confirmed before the requests and kept until
both backups ended, and requests two backups to the backup bucket back to
back, each under a prefix of its own. Both target the one member, and the
first upload takes minutes, so the second start meets the first's
operation: a reading of it held with `InstanceManagerOperationInProgress`
is required, and without one the variant is blocked. Both must end
`Completed` within `backup_limit` (the default 15 minutes covers two
uploads of about 5 MB, about 2.7 minutes each), each with its own
`operationRef`, never shared and never replaced, an artifact that passes S11
`complete`'s verification for that cluster, a manifest whose `operationID`
is its own backup's, and exactly one attempt prefix with one manifest and no
other object under its prefix, also when listed again at the end. The code
predicts this: each backup has its own idempotency key and staging
directory, the Instance Manager refuses the second start with `409
OperationInProgress` while the first's operation runs, and the Operator
holds the second on its recorded target and sends the start again until the
member accepts it. The run reads the member's records of both operations:
the second's `createdAt` must be at or after the first's `updatedAt`,
written when it ended after its upload and staging cleanup, both by the
member's clock; a record that cannot be read leaves the variant blocked.
The limit must be seen removed. `Failed` for a conflict,
`RecordedTargetNotStarted` or a staging failure is a never-event, and
`Failed` for another reason fails it. The source and the installed manager
must be unchanged, and no key may be in a status read, a manifest, an Event,
the manager's log or the member's log. The run removes the limit, deletes
both `CubridBackup`s and the cluster by UID, and leaves the artifacts. Two
backups on different members at once are not exercised. Unit tests cover it
with fakes. On 2026-10-29 the slowed standalone harness (`966e03f`)
passed once on the VM lab (manager `fb6f345`, after S11 `complete` on the
same run): both backups were requested at 15:38:23.7 UTC; at 15:38:26.7 the
second was held with `InstanceManagerOperationInProgress` while the first
was `Running`, and stayed `Pending` for about 2 min 45 s while the first
uploaded through the limit; the member's records show the first operation
ended at 15:41:11.0007 and the second created at 15:41:11.1030, 102 ms
later, by the member's clock; the first was `Completed` 2 min 54 s and the
second 5 min 39 s after the requests, each under its own operation, with
one attempt prefix holding its manifest and one verified object of
5,256,192 bytes; 10,613,240 bytes passed the limit in all. One run is not a
baseline. The review fix made after it (the order must be judged before a
pass, `d480fe8`) changes no path the run took.

An earlier harness ran the two backups on the HA cluster, on the VM lab on
2026-10-29 (manager `fb6f345`). Harness `6481923` passed once at 14:49 UTC
under its earlier rule, which compared the second's `status.startedAt` with
the first's manifest `createdAt`: both on `ha-1`, the second held with
`InstanceManagerOperationInProgress`, completed in 12 s and 15 s. Harness
`b498f91`, with the member's-clock rule, was blocked twice for lack of
contention: at 15:03 UTC both were requested at 15:06:53.7, the member's
records show the first operation ended at 15:06:55.53 and the second
created at 15:06:55.59, and no reading showed a hold; at 15:13 UTC `a`
completed within 3.0 s and `b` within 12.0 s, again without a hold. The
Instance Manager accepts a start with 202 and runs it in the background,
and the Operator needs a reconcile or two to record the target before it
sends the start; the lab's HA database backs up in about 2 seconds, so the
first operation had usually ended before the second's start arrived. That
is why the lane moved to a slowed standalone cluster.

S11 `operation-store-loss` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-operation-store-loss`, `-backup-operation-store-loss`,
#457), in a run namespace that enforces Pod Security `privileged`, and
needs a `-backup-limit` of at least 35 minutes (40 recommended). It reuses
the steps of S11 `instance-manager-restart` on its own standalone cluster
`s11-storeloss`. Once 512 KiB have passed the limit while the backup is
`Running` or `Uploading` with an operation, one command in the member stops
the Instance Manager with `SIGSTOP`, waits until all its threads are
stopped, removes the backup's operation record (`<id>.json`
and any `<id>.*.tmp`) from `IM_OPERATIONS_DIR`, and kills the Instance
Manager with `SIGKILL`. The kill is confirmed by exit 137 on the same Pod,
and the record's absence by listing the store once the cluster reads
`Ready` again. The code predicts that the restarted Instance Manager
answers the poll with 404 `operation not found`. The Operator should then
show `RecordedOperationMissing`, keep the phase, and end the backup
`Failed` with `RecordedOperationLost` once the record has been missing for
30 minutes, without a new start. The run requires that outcome within
`backup_limit`, with at least 30 minutes from `status.operationMissingSince`
to `status.completedAt`. It also requires the same `operationRef`
throughout, never `Completed` or `status.artifact`, no `manifest.json`
under the prefix, one container restart, the source and the installed
manager unchanged, and no key in statuses, Events or logs. A record still
there after the restart, a fault the command refused, a restart before the
fault, or a `backup_limit` that runs out while the backup still reads
`RecordedOperationMissing` before the Operator's 30 minutes leaves it
blocked. `Failed` for the restart reason, or for
another reason, fails it. The member's backup staging directory is listed
after the restart and at the end as evidence for #636, not judged. Unit
tests cover it with fakes. On 2026-10-29 it passed twice on the VM lab
(manager `fb6f345`, after S11 `complete` on the same run), once with an
earlier harness of the change (`470f55b`, which checked only the main thread
of the stopped Instance Manager) and once with the reviewed one (`1dab28d`).
In the second run, with 560,612 bytes through the limit and the backup
`Uploading`, the command removed `op-bfd729df….json` from the member's
store and killed the Instance Manager at 17:52:24.93 UTC; the container was
back 3.4 s later and the cluster read `Ready` 15.5 s after the kill; the
store held no record of the operation after the restart; the restarted
Instance Manager answered `404: operation not found`, the Operator set
`operationMissingSince` 17:52:37 and the backup read `Uploading` with
`RecordedOperationMissing` from 17:52:40.92; it ended `Failed` with
`RecordedOperationLost` at 18:22:39.70, 30 min 2 s after
`operationMissingSince`, under its one operation, with no new attempt,
nothing under its prefix and no artifact. The killed operation's staging
directory was gone after the restart and at the end, as
`reclaimBackupStaging` predicts. The first run's figures were within a
second of these. Two runs are not a baseline.

S11 `graceful-stop` also follows S11 `complete`, in a run of its own
(`lab-installed-ha-s11-graceful-stop`, `-backup-graceful-stop`, #277, with
#468 and #469), in a run namespace that keeps Pod Security `restricted`:
the run creates a standalone cluster `s11-gracefulstop` with the run's own
Secret, starts a sampler in its member's database container (one exec
session that prints the `cub*` processes, the files open below the backup
staging root and the entries below it about every 100 ms) and follows the
container's log, requests a backup, and at the first sampler reading that
shows `backupdb` deletes the member's Pod with its grace period, so that
its `preStop` hook stops the Instance Manager while `backupdb` runs. The
code predicts `Failed` with `BackupFailed` and `the member is stopping:
backupdb failed: ...`, without a kept staging: the stop cancels the
backup, the cancellation kills `backupdb`'s process group, the failed
backup waits until no process holds a staging file before it removes the
staging and records the operation, and only then does the stop run
`heartbeat stop` and `server stop`. The run requires that outcome within
`backup_limit` under one operation, never `Completed` or
`status.artifact`; no sampler reading at or after the first one without the
operation's staging directory may show a process holding a file below the
backup's staging destination; the member's log must hold `backupdb`'s
`Command failed`, the operation's staging writer events, its `Operation
failed` and then the shutdown's commands, in that order; no `manifest.json`
under the prefix; the replacement Pod ready and the cluster `Ready` within
the ready limit; the source and the installed manager unchanged; and no
key in statuses, Events, the sampler's output or the logs. A `backupdb`
never seen before the backup left `Running`, or a stop that reached the
Instance Manager after `backupdb` had ended (an upload failure, or
`backupdb`'s success in the log, with whatever `Completed`, artifact or
manifest the backup then produced), leaves it blocked. The sampler cannot
see a hold shorter than its interval, nor anything after the container
ended. Unit tests cover it with fakes, and a Linux unit test runs the
sampler script against a process holding a file. On 2026-10-29 the first
harness of the change (`9ae6629`) ran once on the VM lab from 19:36 UTC
(manager `fb6f345`), without filler data. `backupdb` took 615 ms and the
sampler saw it 385 ms after the request. The deletion was sent 24 ms after
that, but the stop reached the Instance Manager only after `backupdb` had
succeeded, and the backup completed with its manifest. The staging
directory was gone 767 ms after the deletion, with no holder after it. That
run was a placement miss which the harness misjudged as a failure.

The current harness classifies such a run as blocked. It grows a filler
table in the cluster's database before the request (default 1024 MiB,
`-backup-graceful-stop-filler-mb`) so that `backupdb` lasts for seconds. It
records the time from the deletion to the first log line the stop caused,
to show the `preStop` latency. Its first VM-lab run, at 2026-10-29
20:00 UTC (`c2e31c3`), was blocked while it grew the filler, after about
30 s, at about 196,608 rows. csql had printed `ERROR CODE = -199` and
`-111` after a statement it had committed. A manual check on the lab
(POC-44) found those statements' rows committed and the server unharmed,
with the cause unknown. The filler now judges each statement by a row
count read separately, runs a statement that added no row once more, and
records the error text as `fillerExitErrors`. That harness has not yet run
on the VM lab.

S12 `exact` follows S11 on the same run (`lab-installed-ha-s12`, `-restore`,
#121): after 20 more writes through the source, a new cluster restores S11's
artifact with a seed Secret of its own, and the run requires `Ready` only after
the bootstrap completed, one master by `cubrid heartbeat status`, each
cluster's HA node list naming only its own members, the data rules on all
three restored members and both Services against the history at the known
point with no row written after the backup, S11's marker and a new write on
every restored member, the source unchanged, and no credential in any status
it read. On 2026-10-25 it ran seven times on the VM lab (manager `d6df2ac`):
five passed, the last from the merged harness code; in the last three passes
the restored cluster was `Ready` 2 min 40 s to 2 min 58 s after creation, as
the run measures it. Two were blocked, not failed:
after the data checks had held, a `csql` read on a restored member returned
its row and then lost its session (`-199`, `-111`) 10 to 17 s after the
restored cluster became ready, once on the master and once on a slave; the
cause is unknown and tracked on #238 (POC-36 measured about 3% of plain `csql`
reads aborted this way on a running slave). Since #575 the harness reads
such a lost session once more and records it per scenario (`lostSessions`), so
it no longer blocks a run. Interruptions and restarts during the restore, and
the Operator's logs, are not covered.

S12 `tampered` follows S12 `exact` on the same run
(`lab-installed-ha-s12-tampered`, `-restore-tampered`, #121): a copy of S11's
artifact with one byte of one object changed, the manifest unchanged, is
checked by the run, and a new cluster created from it must end its bootstrap
`Failed` with `RestoreFailed` naming that object's checksum mismatch within
`restore_limit`, never `Ready` or `Complete`, with the source unchanged, S11's
manifest still at its recorded digest and no credential in any status read;
the copy is removed. On 2026-10-25 it ran three times on the VM lab (manager
`d6df2ac`): the Operator refused the copy 1 min 12 s after creation, after
three restore attempts, in both runs that reached it, and the last, from the
merged harness, passed. The first was blocked by the lost session of #238 on a
source member after the refusal, and in the second S12 `exact` was blocked by
it, so `tampered` did not run. The refused cluster's volume afterwards, a
changed manifest or a missing object, and the Operator's logs and Events are
not covered.

S12 `wrong-target` also follows S12 `exact` (`lab-installed-ha-s12-wrong-target`,
`-restore-wrong-target`, #121): a third cluster is formed, takes a marker row
and is deleted with its PVCs kept (`Retain`); a recovery cluster of the same
name over those PVCs, naming S11's artifact, must end its bootstrap `Failed`
with `RestoreFailed` and the Instance Manager's guard (`target ... already
holds database ...; refusing to overwrite (ADR-0008)`) within `restore_limit`,
never `Ready` or `Complete`, on the same PVCs, and its members must still hold
the marker and not S11's. On 2026-10-25 it passed three times on the VM lab
(manager `d6df2ac`), the last from the merged harness, refused in 1 min 3 s to
1 min 24 s after three attempts; one earlier run was blocked in S12 `exact` by
#238 before #575. A normal cluster over retained PVCs (S13 `retain`) is not
covered; the code suggests it would refuse to seed its other members (#90).

S12 `operator-restart`, a lab-only variant (`lab-installed-ha-s12-operator-restart`,
`-restore-operator-restart`, #121), deletes the installed manager Pod without a
grace period while another restored cluster is `Restoring`, and requires the
replacement back within `operator_resync_limit`, the Operator to follow the
operation recorded in `status.bootstrap.operationID` (no restore started again,
`BootstrapReady` never `True` before `Complete`), the restore not already moved
on when the replacement led, by a bound on the clock offset, and then what S12
`exact` requires. On 2026-10-25 it passed twice on the VM lab (manager
`d6df2ac`): the manager was back in 24 to 39 s, the next status showed the same
operation in `SeedingReplicas` with no new attempt, and the restore completed
about 2 minutes later. Of the other runs, one of the final harness was blocked
in S12 `exact` by #238 (two lost sessions in a row on one member), and one
failed in S12 `exact`: an acknowledged write through the read-write Service was
not on one seeded slave within 2 minutes while the data rules had held there,
the pattern of #416. A restart between the start of a restore and its status
write and restarts at other points of the flow are not covered.

S12 `instance-manager-restart`, a lab-only variant
(`lab-installed-ha-s12-instance-manager-restart`,
`-restore-instance-manager-restart`, #121), kills the Instance Manager of the
restore's target member while another restored cluster is `Restoring`, which
restarts that member's container on the same Pod and volume, and requires
exactly one retry naming the manager's restart, a new operation that
completes, one container restart, and then what S12 `exact` requires. On
2026-10-26 it passed on the VM lab (manager `d6df2ac`): the container was back
in 3.4 s, the Operator reported `manager restarted while operation was in
progress; starting attempt 2 of 3`, and the new operation restored, seeded both
peers and completed 3 min 26 s after creation. Whether the retry removed or
took over the killed attempt's data is not read.

Two more lab-only placements of that kill (`-restore-instance-manager-restart-at`,
#121) passed on the VM lab on 2026-10-27. At `preparing` the target's Instance
Manager was killed while the bootstrap was `Preparing`; one retry and a new
operation completed, and the killed operation's own record held no restored
artifact. That shows `restoredb` had not succeeded, not that it had not
started: the Instance Manager reports `Restoring` before it verifies or
downloads the backup (#598). At `seeding` the peer being seeded was killed;
exactly one seed retry with a new seed operation completed, and the restore was
not run again. Kills during `Starting`, before a durable `Completed`, on the
seed source and during the seed backup are not covered.

A [manual VM-lab run on 2026-10-21](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/122#issuecomment-6328187210)
moved the lab to main `9c6ae9c1ef97eae29c13dff7a272d1082715c9e2` through the
private GitOps repository (images from
[run 39623042119](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39623042119),
pulled by the nodes from the now public packages), on k3s `v1.36.5+k3s1`,
CUBRID `11.4.6.1963` and node-local `local-path` storage (not CSI). One run
each; LAB00 and the manual Pod replacements ran while `make test-lab` held
the Lease:

- LAB00 passed (`passed=true`, `problems=[]`, `cleanupSucceeded=true`).
- The upgraded manager created the standalone Broker endpoints of #286 on an
  existing cluster; a driver client in the cluster's namespace committed
  through `-rw` and read through `-ro`. A client in another namespace was
  refused (connection refused) by the `<cluster>-broker` NetworkPolicy, the
  documented default, while a client in the cluster's namespace reached the
  same Service. This is consistent with the policy being enforced on this
  run; the network plugin is not identified. The `-ro` write refusal of S00
  was not checked there.
- Accepting a new Instance Manager image rolled both clusters' Brokers at
  once; the DB Pods moved only when deleted by hand (#96). Deleting a slave
  of the three-member HA cluster turned `PrimaryResolved` to
  `PrimaryObservationIncomplete` for about 17 s and 23 s for the two
  slaves deleted (2 s sampling), and back.
- Deleting the master with a client committing every 0.5 s through `-rw`:
  CUBRID promoted another member, the client saw two failed commits, about
  1.1 s from the first failure to the next acknowledged commit (1.7 s
  between acknowledgements), and all 438 acknowledged rows were on all three members, whose rows
  were identical; neither failed row was anywhere. The Operator claimed
  `RoutingReady` for the new master about 33 s after the deletion (2 s
  sampling), while commits
  through `-rw` were already acknowledged (quarantine reported, not
  enforced, as in S03; #113).
- A full backup with `PreferStandby` completed on a slave afterwards; it was
  not restored.

These are single manual runs, not scenario passes or recovery-time
measurements, and they include no VM power loss, network fault, CSI
storage or restore.

Ordinary backup uploads (#337) use a fresh artifact directory for each upload,
including a retry with a reset operation store; the returned manifest URI is
recorded in the operation and backup status. Unit/race tests and a dedicated
MinIO service built from release source `RELEASE.2025-10-15T17-29-55Z`
verify separate concurrent attempts and preservation after an ambiguous
manifest response. Explicit HA seed requests retain their stable location;
legacy seed-operation adoption and unchanged restore paths are covered in
envtest/unit tests. These results do not establish real CUBRID/PVC replacement,
CSI or VM behavior; the integrated backup scenarios remain with #88.
The [storage command, failure reproduction and verification record](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/337#issuecomment-6259247347)
identify the backend revision and the evidence level.

The Instance Manager HTTP server bounds request headers, complete body reads,
response writes and idle keep-alive connections while preserving the configured
synchronous backup/shutdown budgets (#301; real TCP and unit/race-detector tests).
[Bounded VM connection checks](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/301#issuecomment-6281330433)
at `39b1cb6` (build [39353186855](https://github.com/cubrid-lab/cubrid-kubernetes-operator/actions/runs/39353186855))
on a standalone member with CUBRID `11.4.6.1963`, k3s `v1.36.5+k3s1` and
node-local `local-path` storage (not CSI) observed incomplete-header closure at
6.013 s, an incomplete-body HTTP 400 at 10.011 s (peer closure at 11.019 s;
server duration 9.997 s), and idle keep-alive closure at 61.002 s. During the
idle check, 21 other `/livez` requests kept the port-forward active. No case
hit its client watchdog; the Ready Pod/container incarnation and pinned image
remained stable, and the empty operation inventory stayed unchanged. On the
previous image, the body check ended at its 18 s client-watchdog cutoff without
an HTTP response or peer closure. A separate fresh LAB00 connection smoke
passed on this VM environment. These results do not establish sustained load,
a two-hour synchronous backup, VM-fault or CSI behavior, or S00–S15 qualification.

Operation-store persistence failures (#299) have filesystem/HTTP injection tests
for `RunningBackup`, `Downloading`, `Restoring`, `Creating` and restore's
`Starting`, plus the restored-artifact and adoption-record writes. These
complement the existing staging-claim, upload, HA heartbeat-start and
terminal-settlement checks. With the record directory readable but unwritable,
the attempted write is logged, the preceding disk record and ownership remain,
and operation lookup and same-key replay return 500 while the failed end is only
in memory. Repair, settlement and replay through a reopened store retain the
same failed operation and recorded artifact without repeating commands or
object-store work; failed adoption leaves the previous owner's data and marker
unchanged. Guard-omission mutations verify the negative oracles. These are
non-root unit/HTTP-handler tests with actual directory-permission failures and
CLI/object-store fakes, not CUBRID, disk-full, power-loss or Pod/PVC-replacement
validation. Directory durability and retention remain separate work under #298
and #369.

For live scenarios use **not run**, **blocked**, **fail**, **pass** or **not
applicable**, with a revision, environment, command, evidence link and reason.
A required scenario cannot pass through skipping, missing evidence, a timeout
or unsuccessful fault injection. A unit test pass is not a live scenario pass.

### Shared lab historical baseline

The [lab inventory](docs/testing/lab-inventory.md) records measurements from
2026-10-15: k3s `v1.36.5+k3s1`, containerd `2.3.4-k3s1.36`, node-local
`local-path` storage (not CSI), and locally imported images reported as built
from `2c39873`. It records measurement scopes and unresolved inventory gaps;
it does not qualify that configuration for v0.1.

The inventory author reported one manual Pod-deletion run on 2026-10-13 at
`2c39873`, with three promotable members: after `kubectl delete pod ha-0`,
`ha-1` reported itself master after 13 s; `Ready=True` returned after 26 s,
with `ha-0` a slave, and a row written on the new master was readable on
`ha-0`. These are the observations of that single run, not a scenario pass,
repeated recovery measurement or VM-failure evidence. The report supplied no
scenario artifact set or independent transaction-history oracle.

The 2026-10-15 record did not establish VM stop/start or return/recovery,
network partitions, CSI behavior, an accessible provisioning/cleanup
reproduction, off-node evidence retention or a target-identity fault guard.
Those gaps remain tracked by #80/#122; neither the inventory nor the manual
Pod deletion completes their acceptance conditions.

## Two-maintainer start order

| Work stream | First independently reviewable result | Handoff |
|---|---|---|
| Runtime | #97 non-root lifecycle, then #98 Pod wiring | #101 consumes a runnable authenticated image and Pod configuration |
| Test contracts and infrastructure | #78 reference/reuse inventory, #79 scenario oracles, #104 fast-check failure propagation | #100 workload checks and #103 evidence use the agreed contracts |
| Shared lab | #80 pinned inventory and reproducible provisioning | VM faults start only when controls and cleanup are verified |

These are work streams, not three simultaneous assignments. With two
maintainers, each chooses one available S/M issue, reviews the other's PR,
and coordinates the next handoff. Each author owns implementation, tests and
documentation. Dependencies mean the specific artifact listed in the issue;
L/XL tracking issues are not implementation assignments. Full-lab runs are
serialized. Follow [CONTRIBUTING.md](./CONTRIBUTING.md) for external intake,
test-first exceptions and review rules.

---

## Phase 0 — Architecture Decisions

**Status:** Accepted

- [x] Existing Operator review
- [x] CUBRID HA semantic review
- [x] Broker architecture ADR (#3 / ADR-0002)
- [x] Instance Manager ADR (#10 / ADR-0003)
- [x] Backup execution POC / ADR (#7 / ADR-0007)
- [x] Restore semantic ADR (#8 / ADR-0008)
- [x] Failover / split-brain ADR (#5 / ADR-0005)
- [x] Join / rebuild ADR (#6 / ADR-0006)
- [x] Compatibility target
- [ ] API group ownership decision (#20) — pending (governance)

### Exit Criteria

```text
All P0 ADRs accepted.
No unresolved decision remains that changes the basic CRD,
StatefulSet, Service, or Instance Manager architecture.
```

---

## Phase 1 — Foundation

**Implementation:** Foundation code and CI are merged.
**Live validation:** S00 (standalone cluster, real SQL, remote SQL through the
`-rw` and `-ro` Broker Services, data kept across a Pod deletion) runs on Kind
in CI. Nothing beyond one member is validated.

- Kubebuilder scaffold
- leader election
- `CubridCluster` v1alpha1
- CRD validation
- reconciliation skeleton
- single-node deployment
- StatefulSet / Service / PVC
- Conditions
- envtest
- E2E smoke harness (minikube locally, Kind in CI)
- CI

---

## Phase 2 — CUBRID HA

**Implementation:** HA role discovery, Instance Manager and Broker model code
are merged; manual CUBRID POCs are recorded in `docs/poc/RESULTS.md`.
**Live validation:** Operator-driven formation of three members and the
replication run on Kind in CI, as scenarios S01 and S02 of the scenario
contract with a recorded workload, together with a master Pod deletion (S03),
Broker failures (S05) and Operator failures (S06). A live interruption of the bootstrap remains
unverified. RW/RO access through the Broker Services works on Kind in CI.

- 1 master + 2 slaves
- HA configuration
- stable DNS identity
- role discovery
- Instance Manager
- database-aware health
- Broker model
- current master discovery
- failover observation
- stable application endpoint

---

## Phase 3 — Recovery Lifecycle

**Implementation:** Recovery decision paths and ambiguous-primary detection
are merged; engine failure/partition behavior has manual POC evidence.
The test-only network faults for S07 and S08 are merged: a partition between
members in one or both directions, and a delay to one peer, each confirmed by
observed traffic and proven removed. They were run on the VM lab against a
three-member cluster (#114, POC-39). That covers only the fault controls; how
CUBRID and the Operator react to them is not yet run (#115).
**Live validation:** SQL quarantine, real rejoin and explicit rebuild remain
unverified (#86, #87).

- Pod failure recovery
- slave rejoin
- PVC-loss rebuild
- scale-out
- network partition handling
- split-brain detection
- write-under-failure E2E

---

## Phase 4 — Backup / Recovery

**Implementation:** Backup API/controller, artifact handling and recovery
bootstrap code are merged.
**Live validation:** On the VM lab, S11 `complete` and S12 `exact`,
`tampered`, `wrong-target`, `operator-restart` and `instance-manager-restart`
passed (see above); a lost SQL
session (#238) blocked S12 runs until the harness read it again (#575), and one
S12 `exact` run failed on a seeded slave that applied nothing (#416). The
interrupted backup and restore variants, restarts at other points of a
restore, and the Operator's logs remain unverified (#88, #89, #121).

- `CubridBackup`
- backup execution implementation
- artifact destination
- backup validation
- recovery bootstrap
- restored dataset verification

---

## Phase 5 — Production Hardening

**Implementation:** Rolling sequencing, metrics, Events and authentication
code are merged; placement, retention and disruption work remains.
**Live validation:** Hardening evidence is tracked by #90–#92 and #95.
Compatible rolling updates remain conditional (#96).

- PDB
- topology spread
- storage expansion
- retention
- rolling updates
- observability
- security hardening
- compatibility CI matrix

---

## Implementation Gate

The gate below required all P0 ADRs accepted before the HA controller
implementation started. It is now **satisfied** — architectural directions were accepted (including
Accepted (POC-gated) decisions). This is not the release validation gate:

```text
[x] #1 HA topology
[x] #2 database model
[x] #3 Broker architecture
[x] #4 hostname/DNS
[x] #5 failover/split-brain
[x] #6 join/rebuild
[x] #7 backup POC
[x] #8 restore semantics
[x] #9 update/upgrade
[x] #10 Instance Manager
```

Kubebuilder scaffold, CI, envtest, and Kind harness construction may
start in parallel with Phase 0.

---

## Historical architecture work order

```text
1. HA topology
        ↓
2. Database model
        ↓
3. Broker architecture
        ↓
4. hostname / DNS
        ↓
5. Instance Manager
        ↓
6. Failover / split-brain
        ↓
7. Join / rejoin / rebuild
        ↓
8. Backup POC
        ↓
9. Restore
        ↓
10. Update vs Upgrade
        ↓
11. CubridCluster API finalization
        ↓
12. Kubebuilder implementation
```

### Historical parallel tracks

```text
Track A — Architecture
P0 ADRs

Track B — Foundation
Kubebuilder
CI
envtest
E2E harness (minikube / Kind)

Track C — CUBRID POCs
Broker failover
hostname
backup
rejoin
```

---

## MVP Success Scenario

```text
Create CubridCluster
        ↓
3-node CUBRID HA cluster becomes Ready (1 master + 2 slaves)
        ↓
Write monotonically increasing IDs
        ↓
Delete the current Primary Pod under write load
        ↓
CUBRID HA transitions to a new Primary
        ↓
Operator discovers the new Primary
        ↓
Write endpoint remains available
        ↓
Failed instance recovers and rejoins
        ↓
Data consistency is validated
(acknowledged transactions, missing committed records,
duplicate records, ordering anomalies, RTO)
```

The MVP is successful when this scenario can be reproduced automatically
in an E2E test.

The project does not promise "zero data loss" at this stage — it measures
recovery behavior first.
