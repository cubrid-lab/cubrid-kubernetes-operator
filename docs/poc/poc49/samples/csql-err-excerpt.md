# csql's error log of the POC-49 run: counts and excerpt

csql writes its own error log, `csql.err`, in the current directory of the
csql process (POC-35 in [`../../RESULTS.md`](../../RESULTS.md)). In POC-49
that directory was `/tmp/out` in the container for the in-container loops
(`loop.sh`, parts A to L3); the `docker exec` parts (XA, XB05, XB2) ran csql
in another directory, whose `csql.err` was not copied. The whole file,
`/tmp/out/csql.err` as copied from the container after the run, was
2,232,189 bytes and 40,344 lines (SHA-256
`2f3c0b8ed23099a391051f3ee872dc0f0ad7e75726b25824eac11865e28e0b37`). It is
not kept in the repository; this page records the counts and the lines the
POC-49 entry relies on. Times in the file are the container's clock, UTC.

## Counts over the whole file

| What | Command | Count |
|---|---|---|
| Entries (lines starting `Time: `) | `grep -c '^Time: ' csql.err` | 6,725 |
| `CODE = -971` NOTIFICATION entries (`Program 'csql' (pid N) connected to database server 'appdb@localhost'`) | `grep -c 'CODE = -971,' csql.err` | 6,723 |
| `ERROR CODE = -199` entries (`Server no longer responding.... Invalid argument`) | `grep -c 'ERROR CODE = -199,' csql.err` | 1 |
| `ERROR CODE = -111` entries (`Your transaction has been aborted by the system due to server failure or mode change.`) | `grep -c 'ERROR CODE = -111,' csql.err` | 1 |
| Entries of any other code | | 0 |

Per part, by the entry's time against the part's first start and last end
in `<part>.csql.tsv`: each of parts A to K2 has exactly one -971 entry per
csql run (5,400 in all) and no other entry. Parts L1, L2 and L3 ran three
loops at once from 08:26:09.780Z to 08:27:07.968Z: 1,323 -971 entries for
their 1,500 csql runs, and the one -199 and -111 pair. Why 177 runs of the
three concurrent loops left no -971 entry was not examined.

## The -199 and -111 pair

The pair was logged at 08:26:09.817, in the first second of parts L1 to L3,
by csql pid 89969 (lines 32,402 to 32,423 of the file, quoted as they are):

```
Time: 11/05/26 08:26:09.789 - NOTIFICATION *** file /home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE = -971, Tran = -1, EID = 1
Program 'csql' (pid 89967) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).

*** The previous error message is the last one. ***


Time: 11/05/26 08:26:09.794 - NOTIFICATION *** file /home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE = -971, Tran = -1, EID = 1
Program 'csql' (pid 89969) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).

Time: 11/05/26 08:26:09.817 - ERROR *** file /home/jenkins/workspace/cubrid_release_11.4/src/communication/network_cl.c, line 177 ERROR CODE = -199, Tran = 3, EID = 2
Server no longer responding.... Invalid argument

Time: 11/05/26 08:26:09.817 - ERROR *** file /home/jenkins/workspace/cubrid_release_11.4/src/transaction/transaction_cl.c, line 600 ERROR CODE = -111, Tran = 3, EID = 3
Your transaction has been aborted by the system due to server failure or mode change.

*** The previous error message is the last one. ***


Time: 11/05/26 08:26:09.822 - NOTIFICATION *** file /home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE = -971, Tran = -1, EID = 1
Program 'csql' (pid 89983) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).

*** The previous error message is the last one. ***
```

The csql runs in progress at 08:26:09.817 were run 1 of L1
(08:26:09.780Z to 08:26:09.820Z) and run 1 of L2 (08:26:09.782Z to
08:26:09.823Z), both `SELECT COUNT(*) FROM t`; run 1 of L3 had ended at
08:26:09.797Z. Both exited 0 and their captured output (stdout and stderr)
held neither `-199` nor `-111` (`L1.csql.tsv`, `L2.csql.tsv`), so the pair
was written to the log without being printed by the run. Which of the two
runs was pid 89969 was not recorded. The L1 background loop's
`cubrid heartbeat status` call that ran from 08:26:09.795Z to 08:26:09.817Z
(`L1.bg.tsv`, line 2) ended in the same millisecond.

## Start of the file

```

Time: 11/05/26 08:13:35.462 - NOTIFICATION *** file /home/jenkins/workspace/cubrid_release_11.4/src/transaction/boot_cl.c, line 1085  CODE = -971, Tran = -1, EID = 1
Program 'csql' (pid 317) connected to database server 'appdb@localhost' on the host 'localhost' (port 1523).

*** The previous error message is the last one. ***
```
