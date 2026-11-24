# Race detector and fuzzing

This page says which race and fuzz checks run where, and why. It records the
measurements behind that choice (#951).

## Where each check runs

| Check | Where | Blocks a merge |
| --- | --- | --- |
| Fixed fuzz seeds for the heartbeat role parser, the backup manifest parser and the automatic quarantine release decision | The ordinary `go test`, so the required Tests job (`make verify`) | Yes, as any unit test |
| Race detector on `./internal/instancemanager/...` and `./internal/controller/...` | [Race and fuzz (advisory)](../../.github/workflows/race-fuzz.yml), weekly and on manual dispatch | No |
| Fuzzing with new mutations, 30 seconds per target with two workers | The same advisory workflow | No |

The advisory workflow has no `pull_request` or `push` trigger. It never
reports a check on a pull request, so it cannot be a required check and adds
no time to the Tests, Lint or E2E gates. A failure there is a finding to
triage in an issue. Run it on a branch with:

```sh
gh workflow run race-fuzz.yml --ref <branch>
```

The fuzz targets and their seeds:

- `FuzzParseHAStatus` in
  [internal/instancemanager/parser_fuzz_test.go](../../internal/instancemanager/parser_fuzz_test.go):
  master, slave and replica output, a transition state, empty and truncated
  output, and a missing, duplicated or contradictory local member. A known role
  needs exactly one local member entry whose state matches it.
- `FuzzParseManifest` in the same file: both supported manifest versions, an
  unknown version, a negative size, malformed and upper-case checksums and
  truncated JSON. An accepted manifest must survive a canonical encode and
  parse unchanged.
- `FuzzQuarantineAutomaticRelease` in
  [internal/controller/quarantine_release_fuzz_test.go](../../internal/controller/quarantine_release_fuzz_test.go):
  48 combinations of master evidence, Pod identity, an earlier
  multiple-master quarantine and the release interval. Only one fresh master
  with stable Pod identities, after the full interval, releases the quarantine.

When fuzzing finds a failing input, Go writes it under the package's
`testdata/fuzz/<target>/` directory and the workflow uploads that directory as
an artifact. Reproduce it with `go test <package> -run '<target>/<file>'`.
After the fix, commit the file there so the ordinary `go test` keeps it as a
seed.

## Measurements

Taken on 2026-11-24 UTC on a 6-core Linux amd64 workstation with Go 1.26.0
and Kubernetes 1.37.0 envtest, `-count=1`, from a cold race build cache. Wall
time includes compiling the test binary; peak memory is the largest single
process, as reported by `/usr/bin/time`. These are local numbers, not GitHub
runner numbers: measure the first scheduled runs before changing any time
limit or making a race check required.

| Package | Without `-race`: wall / test | With `-race`: wall / test | Peak memory, without / with |
| --- | --- | --- | --- |
| `./internal/instancemanager` | 36 s / 34 s | 60 s / 53 s | 291 / 324 MiB |
| `./internal/controller` | 175 s / 161 s | 1,041 s / 174 s | 1,040 / 1,063 MiB |
| `./cmd` | 138 s / 14 s | 242 s / 15 s | 1,113 / 1,220 MiB |
| `./test/installed` | 404 s / 396 s | 1,339 s / 1,290 s | 950 / 1,044 MiB |

Every package above passed under the race detector, and no data race was
reported. All other unit packages also passed with `-race`. The race
detector mostly costs build time for the controller package and test time
for `./test/installed`. The advisory lane covers the Instance Manager and
controller packages, where the concurrent restore registration, readiness
coalescing, backup publication and watch code lives. `./test/installed`
would add over 20 minutes and is left out.

| Fuzz target | Executions in 30 s | Inputs added to the corpus | Peak memory |
| --- | --- | --- | --- |
| `FuzzParseHAStatus` | 513,212 | 66 | 301 MiB |
| `FuzzParseManifest` | 555,299 | 176 | 246 MiB |
| `FuzzQuarantineAutomaticRelease` | 69,666 | 2 | 1,251 MiB |

None of the three targets found a failing input. The quarantine target has
few new inputs because its arguments are four small enumerations, which the
48 fixed seeds already cover.

The workflow limits: each fuzz target runs for at most 30 seconds in a job
limited to 15 minutes; the race job is limited to 60 minutes, with 15 minutes
for the Instance Manager step and 40 for the controller step.
