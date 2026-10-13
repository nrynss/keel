# Issue 82 review, round 4

Reviewed hash: `07391efea96e077c1d5fc5f379e73666df6ec70b`, the handed head of pull request 85.
Worktree: `/home/nryn/work/keel-wt/rev-85-r4`, detached and clean.
`git rev-parse HEAD` returned the handed hash and `git status --porcelain` was empty before the review started and stayed empty through it.
I read `git diff 9857f86..HEAD` in full, the round 1, 2 and 3 reviews with their probes, issue 82, the pull request body, and the sqlite package's migration runner.
I read the round 4 commit `07391ef` against the round 3 head `6fca792`. It changes the completeness comparison to `rows >= schema.fileCount()`, adds the superset-ledger test, and sharpens four doc comments.
Every mutation ran in a scratch worktree under `/tmp`, so the reviewed tree never moved.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 0 | 0 | 0 |

## Findings

None. No defect surfaced in the round 4 state, and the pins below measure every behaviour the comments promise.

## Independent pins

The probe lives at `/home/nryn/work/keel/dev-diary/probes/rev85r4/`.
Run `./run.sh` from that directory, or `./run.sh mutation` for the reverted comparison.
The script extracts the v0.7.0 migration files from git at `9857f86`, builds every fixture from those literal files, and measures the reviewed worktree.
Every assertion and every canary reads through a fresh `database/sql` connection, so the store under review never grades itself.

- PIN-SUPER. The round 3 M state, rebuilt over a fresh connection: seven of eight tables stand beside a seven-row ledger for a six-file binary. The reopen refuses with `sqlitestore: namespace "credits" holds 7 of its 8 tables: invalid config`, the error matches `ErrInvalid`, and no store is handed back.
- PIN-SUPER under the mutation. The one line reverted to `==` in the scratch worktree `/tmp/rev85r4-mutation`. The same state opens with no error, and the first Reserve fails raw with `sqlitestore: reserve: read grants: SQL logic error: no such table: credits_grant (1)`, which matches no sentinel. That is the round 3 finding's signature, reproduced by my own run.
- The branch walk of the reconciliation, each branch pinned. Partial set plus a short ledger converges. Partial set plus an equal ledger refuses. Partial set plus a superset ledger refuses. Full set opens with the ledger untouched. Empty set sweeps and re-applies.
- PIN-INT. The partial-plus-short state, five tables and three stamped rows, converges in the runner at the top of Open. The reopen finishes files 4 to 6, the ledger reaches 6, all 8 tables stand, the three stamped rows survive untouched, and the stranded row stays unread under `cost_charge`.
- PIN-REFUSE. The partial-plus-equal state, one dropped table beside the complete six-row ledger, refuses with `ErrInvalid`.
- PIN-ROLL. The full-set-plus-superset state, a seventh ledger row beside all eight standing tables, opens cleanly. The ledger keeps its seventh row, no disagreement warn fires, and a fresh charge lands. A future schema upgrade rolled back on a healthy store is untouched. The full-set branch returns before any ledger read, and this pin measures the consequence.
- PIN-SHORT. The full-set-plus-short state, a deleted ledger row beside eight standing tables, fails in the runner with `table credits_settle_once already exists (1)`, which matches no sentinel. The reconciliation never runs. The Open doc's boundary statement describes the behaviour the code has.
- PIN-R1. The stranded pre-fix file, literal v0.7.0 DDL with old rows and a six-row credits ledger, opens as a fresh store. The qualified tables come back empty, the old `cost_charge` row and the spent 30 stay on disk unread, and the ledger records the original six file names.
- PIN-GUARD. The reviewed build's own guarded sweep statement over fresh connections. Beside a full table set it affects 0 rows and the ledger keeps its 6. Beside the stranded fixture it affects 6 rows and the ledger reads 0. The empty-set branch sweeps and re-applies through this statement.
- PIN-RACE4. My own harness, not a prior round's. 80 rounds of 3 concurrent openers of one stranded fixture, staggered 0 to 20 milliseconds. 240 opens, 0 refused, 0 raw failures, 0 bricked files. Every round ended with 6 ledger rows beside 8 tables and a canary open. Six handed-tree battery runs repeated it clean, 1440 live opener runs in all.
- PIN-WIDE. 30 rounds of 6 concurrent openers, 180 opens, 0 raw failures, 0 bricked files, every round clean in every run.
- PIN-CXL. 40 rounds of 3 openers whose contexts die at random instants inside the open. Every round converged, 0 bricks, counts agree. One opener across the runs surfaced a raw already-exists error while its own context was already dead. The runner's own doc names that shape, the Open doc names the same exception, and the file converged in every round. I record it as measured behaviour of the untouched runner, not a finding.
- PIN-B, PIN-ISO, PIN-FOLD. The literal v0.7.0 fixed-name file reopens with no migration and keeps its rows. Two namespaces isolate across charges, holds, settle-once pairs, a quote run and schema names, with no unqualified table left on a two-namespace file. The case twin is one namespace.
- The mutation run also held PIN-REFUSE, PIN-INT, PIN-ROLL, PIN-SHORT, PIN-R1, PIN-GUARD and PIN-RACE4, and every one passed under `==`. The widened comparison changes the superset classification alone, exactly as the round 3 finding said.

### The comment audit

I checked every comment the four remediation commits `e2b9f43`, `9f1bb3b`, `6fca792` and `07391ef` touched, in the final state of `git diff 9857f86..HEAD`.

- "A ledger holding at least one row per file this binary carries" states the shipped `rows >= schema.fileCount()`. PIN-SUPER and PIN-REFUSE measure both sides of that bound.
- The superset rationale, a newer binary recording rows that outlive it on disk, is the rollback shape PIN-ROLL measures. The comments claim no more and no less than the code does.
- The boundary statement about a ledger short beside a full table set is PIN-SHORT's raw runner failure, and the claim that the guard keeps that state from forming is PIN-GUARD's zero erase beside standing tables.
- "Nothing in this package deletes a table" is true. A grep finds no DROP statement in the package's sources. The only DROP TABLE statements in the tree are the tests that build the damaged states the refusal classifies.
- A grep for the fixed default spellings in non-test sources finds two comment sites, and both sentences scope them to the empty namespace. No unscoped default-only spelling remains.

## Gate evidence

`GOCACHE=/tmp/keel-rev85r4-gocache ./tools/check.sh` ran in the review worktree and passed all 14 checks, `go test -race ./...` included. The tail reads `== check 14/14 api ==`, `ok`, `all checks passed`.
`go test -race -count=1 ./cost/... ./sqlite/...` passed on its own, the sqlitestore suite in 100 seconds.
`apidiff -m ./api/v0.7.0.export .` printed nothing and exited 0, so the exported surface is unchanged from v0.7.0.

## Zero residue against prior rounds

| Round | Finding | Ruling |
|---|---|---|
| 1 | H, a pre-fix custom-namespace file failed to open with a raw error while the body promised a fresh store | Fixed and stays fixed. PIN-R1 reopens the rebuilt stranded file as a fresh store, the old rows stay on disk unread, and the ledger records the original names. The race harness reopens 1440 such files across the runs without one raw failure or one brick. |
| 1 | L, the ForgetOwner comment named default-only table spellings | Fixed and stays fixed. The comment speaks of the owner budget table, the reservation rows and the settle history. My own grep over non-test sources finds fixed spellings only in the two sentences that scope them to the empty namespace. |
| 2 | H, the reset's unguarded ledger delete bricked a stranded file under concurrent opens | Fixed and stays fixed. PIN-GUARD measures 0 erased rows beside standing tables, and my own 80-round, 3-opener harness plus the 6-opener and cancellation harnesses left no raw failure and no brick in six runs. |
| 3 | M, the completeness comparison accepted only an exact ledger size, so a superset ledger beside a partial set opened and failed raw at first use | Fixed and stays fixed. PIN-SUPER measures the `ErrInvalid` refusal on the handed tree, and the same pin under the reverted comparison reproduces the raw first-use signature in the scratch worktree. |

Claim, severity by severity. Round 1 held one H and one L. Round 2 held one H. Round 3 held one M. No residue of any of the four survives at its own severity in the handed tree. This round adds no new finding at any severity.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. `apidiff -m ./api/v0.7.0.export .` printed nothing. The whole pull request adds no exported identifier, and the round 4 commit adds only the unexported test helper `freshPresentTables`.
2. **Does the change, its issue, or its pull request name a consumer?** No. It speaks of an application with a dollar budget and a provider-credit budget, and the consumer-names check passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The edge checks pass. The package's imports are standard library beside cost, id and sqlite, inside the SQLite edge package this change already belongs to.
4. **Does any code, comment or test cite a planning file or a private note, or did stripping one lose the reason it carried?** No. The plan-reference check passes. Every comment the four remediation commits touched states a behaviour my pins measure, and the superset rationale and the boundary statement carry their reasons themselves.
5. **Does the README package table still match the filesystem, package for package?** Yes. The table's 30 rows cover the 28 top-level package directories beside the `mediastore/s3` and `config/source` rows, and the change adds no package.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes. All pins, mutations and the gate ran at the confirmed detached hash in `/home/nryn/work/keel-wt/rev-85-r4`, which stayed clean. The mutation ran in the scratch worktree `/tmp/rev85r4-mutation`, which the run removed afterwards.

## Verdict

**APPROVE.** No findings. Every prior round's finding is fixed, measured by my own pins, and leaves no residue at its own severity.
