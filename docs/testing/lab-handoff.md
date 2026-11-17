# VM-lab campaign handoff and elapsed-time measurements

This is an operational **measurement protocol**, not a queue implementation or
permission to run a test. The shared VM lab has one reservation, the
`cubrid-lab/campaign` Lease, which `make test-lab` and the canonical campaign
launcher both take and each refuses to start while the other holds it (see
[lab runs](lab-runs.md)). Only one
disruptive campaign may use it at a time. Namespace isolation is not
sufficient for a VM stop, node drain or network fault.

## Concurrent work, exclusive work

| Work | May overlap another campaign? | Boundary |
| --- | --- | --- |
| Compile tests, review PRs, inspect scenario contracts | Yes, off-lab | Do not deploy or modify shared lab resources |
| Prepare pinned source/image identities and a proposed command | Yes, off-lab | A proposal is not an authorized dispatch |
| Analyze already-retained, sanitized run evidence | Yes, off-lab | Do not read mutable live resources as historical evidence |
| Read-only lab inventory | Only if the owner permits it | No reservation, readiness or capacity claim from a stale snapshot |
| Inject faults, install/reconfigure Operator, create test namespaces, clean up | No | Approved target and the campaign Lease |
| Run the release-candidate soak | No competing disruptive run | Preserve the full duration and data checks |

## Handoff for one approved scenario

1. **Prepare off-lab.** Select the owning issue and descriptive scenario key.
   Record the full source commit, Operator and Instance Manager image digests,
   client image digest, proposed command, expected SQL oracle, artifact
   destination and cleanup owner. Check that the scenario's implementation
   and mandatory prerequisites are ready. Do not include kubeconfig, token,
   object-store credentials or private hostnames in public records.
2. **Authorize and reserve.** A maintainer confirms the named target, fault
   authority, maintenance window, affected shared workloads, storage budget
   and cleanup. A prepared item must not take the Lease merely because
   another run has finished; it waits for that authorization.
3. **Recheck live preflight.** Confirm the context marker, exact installed
   image identities, node placement, schedulable capacity, storage/CSI
   applicability, off-node client/evidence path and any scenario-specific
   authority. If any requirement is absent, report **blocked**, not passed.
4. **Run exclusively.** Use the existing `make test-lab` entry point, which
   takes the campaign Lease (or the canonical launcher, which takes the same
   Lease, never both). Preserve fault confirmation, SQL transaction histories,
   observed conditions and recovery timeline. No concurrent fault campaigns.
5. **Finish safely.** Collect evidence and verify fault removal, namespace
   ownership and bounded cleanup **before** releasing the lab. On cancellation
   or Lease loss, follow the existing launcher recovery process; never
   delete an apparently stale Lease or someone else's PVC to clear the queue.
6. **Analyze off-lab.** Copy the sanitized immutable bundle to an approved
   location, verify completeness and provenance, then evaluate pass/fail/
   blocked/not-run independently. This analysis can overlap the next
   separately authorized run, but must not change the prior verdict or
   evidence. Send the result to its owning issue, and to #126 when the run
   qualifies a release candidate.

## Measure before optimizing

Record one row per **attempt**, including failures and blocked preflights.
Use UTC timestamps from the same clock for durations; if clocks differ,
record the offset and do not compute an unsupported duration. A blank
timestamp means *not measured*, not zero.

| Field | Meaning |
| --- | --- |
| `run_id`, `scenario_key`, `issue` | Unique attempt and owner |
| `source_sha`, `image_digests`, `lab_target` | Exact tested identity (redact private infrastructure) |
| `prepared_at` | Inputs and test binary ready, off-lab |
| `authorized_at` | Named target and disruptive action approved |
| `lease_requested_at` | Authorized run starts waiting for the campaign Lease |
| `lease_acquired_at`, `preflight_finished_at` | Exclusive preflight duration |
| `fault_started_at`, `fault_removed_at` | Confirmed fault window, if applicable |
| `cleanup_verified_at`, `lease_released_at` | Exclusive cleanup and lab occupancy |
| `evidence_archived_at`, `verdict_at` | Off-lab evidence handoff and review |
| `verdict`, `blocked_reason`, `evidence_url` | Outcome and immutable evidence |
| `retries`, `retest_reason` | Avoid silently treating a rerun as the same attempt |

Suggested derived durations (only when both timestamps exist):
- **Lease wait:** Lease acquired minus requested.
- **Exclusive lab occupancy:** Lease released minus acquired.
- **Exclusive preflight:** preflight finished minus Lease acquired.
- **Cleanup:** cleanup verified minus fault removed (or test completion for a
  fault-free run).
- **Evidence handoff:** archived minus Lease released (can be negative if a
  sanitized copy finishes while the Lease is held).
- **Decision turnaround:** verdict minus prepared.

Compare median and p95 durations on like-for-like scenario variants over
multiple runs, and count blocked attempts separately. Also report lab
utilization as total exclusive occupancy divided by the **authorized
available** lab window; do not count time without fault authority as idle
capacity. Never optimize by shortening mandatory observations, weakening
SQL/data checks, omitting cleanup or reducing the 24-hour soak.

## Incremental automation boundary

An optional preparation command may eventually validate input formats and
generate a proposed command and evidence manifest **without contacting the
lab**. A later reviewed dispatcher may use the existing private runner only
with explicit approval, the campaign Lease and scenario-specific safeguards.
Neither step exists merely because this document was merged. Track actual
implementation and measured outcomes in [#946](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/946);
#122 owns lab integration and #126 owns candidate qualification.
