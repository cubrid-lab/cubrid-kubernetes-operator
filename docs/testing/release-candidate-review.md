# Release candidate evidence review — preparation worksheet

> [!IMPORTANT]
> **Not a release verdict or a new test contract.** This page is a review
> worksheet for [candidate qualification #126](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/126).
> No candidate was installed, faulted, validated, or approved by creating it.
> The [scenario contract](scenario-contract.md) defines the oracles,
> required variants, result vocabulary and file formats; [ROADMAP.md](../../ROADMAP.md)
> owns implementation and validation status. If this worksheet disagrees with
> either source, correct the worksheet rather than weakening the source.

This page helps reviewers move from a set of scattered CI and laboratory runs
to **one candidate-specific acceptance decision**. It does not add a release
gate, copy historical results into the new candidate, or replace the
[immutable release evidence index](scenario-contract.md#release-evidence-index).
The actual run records belong in their scenario-owning issue and retained
artifact bundle, not in this prose.

## 1. Freeze the identity being judged

Before choosing a result, fill in the actual values, and keep this worksheet
with the review. An unknown digest or missing observation is **unknown**, not
an empty string that happens to compare equal.

| Candidate identity | Exact value / source to record |
|---|---|
| Reviewed source commit (full 40-character SHA) | _Not assessed_ |
| Candidate install manifest + SHA-256 | _Not assessed_ |
| Operator image repository and observed digest | _Not assessed_ |
| Derived Instance Manager image and observed digest | _Not assessed_ |
| CUBRID base image identity and observed engine build | _Not assessed_ |
| Kubernetes distribution/version, architecture, CSI/StorageClass | _Not assessed_ |
| Storage and object-store scope, including retention | _Not assessed_ |
| Release-PR identity and non-publishing #124 dry-run link | _Not assessed_ |
| Approved VM-lab reservation, context/ownership and fault authority | _Not assessed_ |
| Evidence destination, custodian, access and retention/expiry policy | _Not assessed_ |

The tag requested in a manifest is not proof of the image the Pod ran.
The derived Instance Manager digest does not identify the CUBRID base
image on its own. Record both intended and **observed** identities.
Use the [compatibility limits](../compatibility.md) and
[controlled lab instructions](lab-runs.md); do not treat a local-path,
single-node or emulated run as a qualifying CSI-backed VM environment.

## 2. Expand every mandatory scenario and variant

The table below is **only a navigation list of scenario families**. It has
no results and does not enumerate variants. Expand each family into the
required rows from
[Required scenarios](scenario-contract.md#required-scenarios), preserving
each lane's exact variant and validation level. A result from another
lane or a lower level cannot fill a higher-level requirement.

| Scenario | What its evidence must establish | Contract level / boundary |
|---|---|---|
| S00 | Install, write/read and Pod recovery | Real database on Kind |
| S01 | Three-member HA and RW/RO access | Real database on Kind |
| S02 | Replication under acknowledged writes | Real database on Kind |
| S03 | Primary Pod failure, reconnect, recovery and timing | Real database on Kind; expand all required variants |
| S04 | Primary VM power failure and return | VM lab; a Kind fault is not a substitute |
| S05 | Broker failure, client results and HA stability | Real database on Kind; expand variants |
| S06 | Operator interruption and continuity | Real database on Kind; expand variants |
| S07 | Network partition and safe write-path behavior | VM lab for the verdict; Kind may be preliminary |
| S08 | Replication lag followed by repeated failures | VM lab |
| S09 | Rejoining former primary and preserved commits | Clean variant on Kind, applicable fault variants on VM lab |
| S10 | Rebuild after PVC loss without overwriting source | Real database on Kind |
| S11 | Backup, interrupted/failed operations and artifact verification | Real database on Kind contract level; use required installed-lab paths |
| S12 | Restore into a new cluster and dataset/convergence checks | Real database on Kind contract level; use required installed-lab paths |
| S13 | Cluster deletion and PVC retention | Real database on Kind; expand variants |
| S14 | Authentication, credential handling and token changes | Real database on Kind; expand variants |
| S15 | Node drain, PDB behavior and SQL | Multi-node Kind; VM lab for real node behavior |

**Important distinction:** an unfiltered `make test-e2e` run has the
particular required `kind` lane described in the contract. It does **not**
run every S00–S15 family or independently establish the mandatory VM and
installed-HA recovery requirements. A successful `lab-installed-smoke`
or `lab-installed-ha` baseline likewise is not a blanket S00–S15 pass.
Check the exact lane and the variant in the original result. Do not mark
an unexecuted fault `not_applicable` because a command did not select it.

## 3. Review one observed result at a time

For **each exact required lane, scenario and variant**, the reviewer records
the following alongside its run identity. Blank fields mean **not assessed**,
not `pass`. Do not paste example run IDs or assume the test met its limits.

| Field | Required observation |
|---|---|
| Lane / scenario / variant | Exact canonical values as recorded in `summary.json` |
| Original run | Immutable run ID, UTC start/end, original CI or lab URL |
| Environment | Source commit, observed images, engine, node and storage details |
| Recorded verdict | One of the contract's actual recorded results |
| Independently judged verdict | Recomputed from the evidence, with reviewer and UTC date |
| Fault and recovery | Confirmed start/stop boundaries, affected member and client path |
| SQL / data | Attempt, acknowledgement, failure or unknown history; per-member keys and values |
| Timings | Observed recovery duration, limit and sampling/clock uncertainty |
| Original artifacts | Exact `summary.json`, `junit.xml`, history, timeline and required scenario files |
| Diagnostics and limits | Missing, truncated or stale data, failed collection and unverified hypotheses |
| Reproduction / follow-up | Original negative run, linked correction and later rerun, if any |

The canonical [release evidence index](scenario-contract.md#release-evidence-index)
already defines `recorded` versus `judged`, candidate versus observed
`environment`, and the file states `archived`, `listed`, and
`unavailable`. **Use that schema rather than inventing another JSON
manifest.** A recorded `pass` whose mandatory raw evidence is absent cannot
be independently accepted as `pass`.

Review the original bundle and the scenario-specific
[oracle](scenario-contract.md#oracle-correctness), not the green status
of a wrapper workflow. A fault that did not occur, untrusted role evidence,
a connection failure in place of a server rejection, a missing acknowledged
write or an exceeded time limit must retain its contract-defined outcome.
A retry that later passes does not erase the original failing run.

Report actual recovery time and observed missing acknowledged operations
separately from any formally chosen **RTO/RPO targets**; do not infer zero
loss or invent a numeric objective. Unknown commit outcomes must not be
silently retried as successful writes.

## 4. Preserve the proof, not merely its name

For the [release evidence index](scenario-contract.md#release-evidence-index)
and #94's retention review, verify all of these before using an `archived`
claim:

1. The reviewed original bytes exist and can be read from the stated durable
   location **independently of expiring CI artifacts**.
2. Each retained file's SHA-256 matches the recorded digest after it is
   downloaded again; both failing and passing traces remain available.
3. The retained bundle is sanitized without deleting the critical fault,
   SQL, timeline or process-identity evidence needed for the oracle.
4. Storage custodian, permitted access, retention term, expiry-monitoring
   owner and retrieval procedure are recorded.
5. Missing or unreadable files remain `unavailable`; a digest computed
   before copying is only `listed`, not proof of archival.

Do not place Secrets, tokens, kubeconfigs or unredacted user data in the
review, issue, PR or public archive.

## 5. Reconcile a changed candidate and hand off

- If the candidate commit, CRDs, manifests, image digest or relevant
  configuration changes, identify which existing results no longer apply.
  Record a fresh candidate-specific index and rerun all affected required
  tests; do not overwrite the prior index or edit historical evidence.
- Preserve the complete failure, its owning issue and the verification
  after a fix. `blocked`, `not_run`, `not_applicable` and `fail` must
  retain their meanings. For a required scenario only a fully evidenced
  contractually valid result can support release acceptance.
- Check the #124 non-publishing release dry-run separately from the
  authorization to publish. Ordinary `main` commits must not publish a
  release, and a successful artifact build is not the scenario verdict.
- Hand the candidate identity and exact procedure/evidence URLs to the
  independent reviewer in
  [#127](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/127).
  They must reproduce using the documented procedures, not undocumented
  author knowledge. Shared VM campaigns must be serialized.
- Give the reviewed index, verified retention and all outstanding gaps to
  [#94](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/94)
  and [#95](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/95).
  Neither this worksheet nor a completed Kind run authorizes publication.

## Review disposition (fill only after observing the candidate)

| Decision field | Reviewer record |
|---|---|
| Candidate and evidence index URL | _Not assessed_ |
| Required lanes / variants independently judged | _Not assessed_ |
| Failed, blocked, missing or out-of-candidate evidence | _Not assessed_ |
| Repeat-fault and initial 24-hour workload results | _Not assessed_ |
| Independent installation and recovery reproduction | _Not assessed_ |
| Evidence retrieval and long-term retention check | _Not assessed_ |
| Maintainer's release decision, UTC timestamp and link | _Not assessed_ |

**No release decision has been made by this document.** Keep each actual
validation status in `ROADMAP.md`; this is only a handoff/review aid.
