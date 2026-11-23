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
| Kubernetes distribution/version, architecture, StorageClass (CSI untested) | _Not assessed_ |
| Storage and object-store scope, including retention | _Not assessed_ |
| Release-PR identity and non-publishing #124 dry-run link | _Not assessed_ |
| Approved VM-lab reservation, context/ownership and fault authority | _Not assessed_ |
| Evidence destination, custodian, access and retention/expiry policy | _Not assessed_ |

The tag requested in a manifest is not proof of the image the Pod ran.
The derived Instance Manager digest does not identify the CUBRID base
image on its own. Record both intended and **observed** identities.
Use the [compatibility limits](../compatibility.md) and
[controlled lab instructions](lab-runs.md); do not treat a local-path,
single-node or emulated run as the three-VM k3s `local-path` reference
environment, and do not read any run as evidence of CSI behavior.

## 2. Expand v0.1 required and measured scenarios by variant

The table below is **only a navigation list of scenario families**. It has
no results and does not enumerate variants. Expand the v0.1 must-pass and
measured/safety-reviewed families into their exact variants using
[Required scenarios](scenario-contract.md#required-scenarios) and the
[2026-11-21 v0.1 release scope](../../ROADMAP.md#current-execution-gates--2026-11-21-scope-decision).
S09 automatic retained-data rejoin is deferred to v0.2; S10 explicit rebuild
remains required. Historical raw failures and scenario contracts are not
reclassified by that release-scope decision. A result from another lane or
a lower level cannot fill a higher-level requirement.

| Scenario | What its evidence must establish | Contract level / boundary |
|---|---|---|
| S00 | Install, write/read and Pod recovery | Real database on Kind |
| S01 | Three-member HA and RW/RO access | Real database on Kind |
| S02 | Replication under acknowledged writes | Real database on Kind |
| S03 | Primary Pod failure, reconnect, recovery and timing | Real database on Kind; abrupt must-pass, graceful measure + safety review |
| S04 | Primary VM power failure and return | VM lab; role/routing recovery required, timing and acknowledged-loss measured; Kind is not a substitute |
| S05 | Broker failure, client results and HA stability | Real database on Kind; expand variants |
| S06 | Operator interruption and continuity | Real database on Kind; expand variants |
| S07 | Network partition and safe write-path behavior | VM lab; measure closure and prove no managed `-rw` acknowledgements after closure |
| S08 | Replication lag followed by repeated failures | VM lab; not a separate required result: counted through repeated S04 and S07 runs (maintainer decision 2026-11-23, #126) |
| S09 | Automatic rejoin of retained data (v0.2) | Deferred; not a v0.1 release acceptance gate |
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

For **each in-scope required or measured lane, scenario and variant**, the reviewer records
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

To generate a read-only Markdown inventory, place the unpacked bundles in a
directory named for their respective run IDs. Pass the immutable release
evidence index and the parent directory to the reporter:

```sh
go run ./test/evidence/cmd/candidate-matrix \
  -index candidate-index.json -runs ./retained-runs > candidate-matrix.md
```

The reporter matches each local summary to the index, checks SHA-256
digests for the summary, JUnit and listed scenario files, and shows the
recorded verdict alongside the verdict supported by the local evidence.
Rows retain their run's lane and level. The run table identifies filtered
and unfinished runs, missing bundles or files, mismatched revisions and
images, and lanes the evidence reader could not validate. The separate
**v0.1 scope** column labels requirements as mandatory pass, mandatory
measurement and safety review, v0.2 deferred, conditional or unmapped.
It **does not** change any raw or independently judged result, nor does
a measured-only classification make a failed S04/S07 or graceful S03
run eligible for release automatically. A reviewer must establish the
scenario's required safety, recovery, identity and SQL evidence by hand,
and only then record the separate release-scope verdict in the indexed
candidate review. Unknown/unmapped variants need a maintainer decision,
not a default pass. Related evidence stays in its original run directory.

Use GitHub Actions run or artifact URLs in this repository for `source`.
The report omits raw log content, archive locations, and local paths. The
[generated sample](../../test/evidence/cmd/candidate-matrix/testdata/sample.md)
uses synthetic inputs and a placeholder Actions link. Keep manual judgement
of SQL, repetitions, soak results, and release approval in the candidate
review.

### v0.1 Go/No-Go verifier

The same command with `-verify` judges the indexed runs against the
[v0.1 candidate acceptance](scenario-contract.md#v01-candidate-acceptance--2026-11-21-decision)
table. It exits `0` only for **AUTOMATED CHECKS GO — manual gates
pending**; **NO-GO** and **REVIEW REQUIRED** exit `1`. Even the first is
not release approval: the report lists the manual gates it does not check.

```sh
go run ./test/evidence/cmd/candidate-matrix -verify \
  -index candidate-index.json -runs ./retained-runs > candidate-verdict.md
```

The report lists each run's check, one row per required variant and one
row per recorded result with its raw recorded verdict, the verdict its
evidence supports now and the separate v0.1 verdict. It writes nothing
into the bundles. The rules, failing closed:

- Every run must be the indexed bundle: `summary.json`, `junit.xml` and
  every file the index lists or archives must be present, non-empty and
  match its SHA-256 digest. The run must have finished, belong to a
  registered lane (not `kind-filtered`), and record the candidate's full
  commit and both image digests. It must have passed its lane, or every
  problem it or the evidence reader records must belong to a result the
  verifier judged as not Go; any other problem, such as a credential in
  the evidence, a step without its label or a suite deadline, blocks the
  run. A run that fails any of these makes the candidate No-Go and
  qualifies none of its results.
- A required variant is every registered result, at the Kind and VM-lab
  levels, of S00–S07 and S10–S15 whose v0.1 scope is must-pass or
  must-measure. It is Go only when it has at least one record and every
  record of it is Go; a missing variant is No-Go.
- A must-pass record is Go only when its evidence supports a `pass` and
  its cleanup did not fail. `fail`, `blocked`, `not_run`,
  `not_applicable` or a pass without its evidence is No-Go.
- A must-measure record (S03 `graceful`, S04 `power-off`, the S07
  variants) must also record a confirmed fault and every measurement its
  rule lists, each of its kind: a duration or number of seconds, a whole
  count, a non-empty name, or a non-empty map of counts; `unknown`, an
  empty value or another type is No-Go. Acknowledged writes on the
  established S07 session after the managed `-rw` path was observed closed
  must be `0`, and S03 `graceful` must miss no acknowledged write. A raw
  `fail` stays `FAIL` and is No-Go: no recorded field shows that only a
  time limit failed, so a timing overrun cannot be told from another
  failure.
- S08 is counted through repeated S04 and S07 runs on the candidate
  (maintainer decision 2026-11-23, #126), so it is not a required variant.
  A recorded S08 result is **NOT COUNTED** with that reason, and a `fail`,
  `blocked` or failed cleanup of it is **REVIEW REQUIRED**. The report lists
  the S08 coverage among the manual gates; the number of repetitions is not
  yet decided. S10 has two registered must-pass results, `ha-pvc-loss-rebuild`
  (Kind) and `installed-ha-pvc-loss-rebuild` (VM lab). Each is judged by
  `rebuild_limit`, 5 minutes on Kind and on the VM lab since the
  [maintainer decision of 2026-11-23](https://github.com/cubrid-lab/cubrid-kubernetes-operator/issues/126#issuecomment-7096254639),
  and like any other must-pass result: a `blocked` result, such as one
  recorded with the limit explicitly unset, is No-Go, and each stays No-Go
  until a run on the candidate passes it.
- S08, S09, S16 and S17 results are never required. A `pass` or `not_run` is
  **NOT COUNTED**; a `fail`, `blocked` or failed cleanup is **REVIEW
  REQUIRED**, because the contract keeps S09's held-member safety behavior.
  A result the registry or the scope cannot map is **REVIEW REQUIRED**.

The verifier judges only the runs the index names: it cannot detect a run
left out of it. Index completeness, with every failure and rerun of the
candidate linked as #126 requires, remains a manual review item, as do
Conditions/Events/metrics as separate requirements, repetitions (including
the S08 coverage), the
24-hour soak, the real-fault review of VM runs, independent reproduction
(#127) and release approval. The
[sample report](../../test/evidence/cmd/candidate-matrix/testdata/verify-sample.md)
comes from synthetic runs, records a synthetic S10 pass, and is Go for the
automated checks.

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
