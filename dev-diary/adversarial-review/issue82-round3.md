# Issue 82 review, round 3

Reviewed hash: `6fca7922e047ccd9202f1912901f638baae9d682`, the handed head of pull request 85.
Worktree: `/home/nryn/work/keel-wt/rev-85-r3`, detached and clean.
`git rev-parse HEAD` returned the handed hash and `git status --porcelain` was empty before the review started and stayed empty through it.
I read `git diff 9857f86..HEAD` in full, the round 1 and round 2 reviews with their probes, issue 82, the pull request body, and the sqlite package's migration runner.
I read the round 3 commit `6fca792` against the round 2 head `9f1bb3b`.
The test diff `9f1bb3b..HEAD` removes two lines, both a reflowed comment, and weakens no assertion.
Every mutation ran in a scratch worktree under `/tmp`, so the reviewed tree never moved.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 0 | 1 | 0 |

## Findings

| Severity | Location | Defect | Pin | Mutation |
|---|---|---|---|---|
| M | `cost/sqlitestore/sqlitestore.go`, `reconcileSchema`, the completeness comparison | The refusal reads `rows == schema.fileCount()`, so only a ledger holding exactly this binary's file count counts as complete. A partly present table set beside a superset ledger escapes the refusal. The open succeeds, and the first read of a dropped table fails with a raw `no such table` error. The package doc and the Open doc both promise the ErrInvalid refusal for a partly present set beside a complete ledger. | PIN-SUPER below. Seven of eight tables stand beside a seven-row ledger for a six-file binary. The reopen reports no error and does not match ErrInvalid. The first Reserve then fails with `sqlitestore: read grants: SQL logic error: no such table: credits_grant (1)`, which matches no sentinel. | The shipped `==` is the defect. mutF changes the one line to `rows >= schema.fileCount()`. PIN-SUPER then reports the refusal with ErrInvalid true, and every other pin still passes, so the fix is one line and no measured healthy state misclassifies. |

### The finding in detail

The reconciliation exists to classify states on disk against the schema the opened binary carries.
The refusal is its corruption alarm, added in this pull request for damage no package path produces.
Its comparison counts ledger rows and accepts one exact value.
A ledger can hold more rows than the binary knows files.
A newer binary with a seventh migration file records a seventh row.
A rollback to this binary leaves those rows on disk, because the ledger is disk state that outlives binaries.
A store in that shape is healthy while its tables stand, and `present == len(names)` returns before the comparison, so the rollback alone never misfires.
The misfire needs the very damage the alarm exists for: one qualified table dropped beside that rolled-back ledger.
The comparison then reads `7 != 6`, falls through to the sweep, whose guard no-ops beside standing tables, and the open succeeds.
Every later read of the dropped table fails raw.
Both doc comments state the wider rule the code does not implement, so the comment rule rates this at the behaviour's own severity.
The state needs outside damage to form, and a consumer survives it, which is why this is M and not H.

## Independent pins

The probe lives at `/home/nryn/work/keel/dev-diary/probes/rev85r3/`.
Run `./run.sh` from that directory, or `./run.sh baseline` for the contrast run.
The script extracts the v0.7.0 migration files from git at `9857f86`, builds every fixture from those literal files, and measures the reviewed worktree.
Every assertion and every canary reads through a fresh `database/sql` connection, so the store under review never grades itself.

- PIN-GUARD. The reviewed build's own guarded sweep statement, run over fresh connections on two states. Beside a full table set it affects 0 rows and the ledger keeps its 6. Beside the stranded fixture it affects 6 rows and the ledger reads 0 after.
- PIN-INT. The interrupted re-apply state, five tables and three stamped ledger rows, reopens. The open succeeds, the ledger reaches 6, all 8 tables stand, the three rows stamped 0 survive untouched, and the stranded row stays unread under `cost_charge`. The convergence happens in the runner at the top of Open, which applies files 4 to 6 before the reconciliation runs. The reconciliation then sees a full set and returns.
- PIN-REFUSE. A working credits store one dropped table short beside its complete six-row ledger refuses the reopen with an error matching `ErrInvalid`.
- PIN-SUPER. The finding's state, measured. See the findings table.
- PIN-WARN. The guarded erase affects 0 rows on the interrupted re-apply state, and the reopen of that state logs no disagreement warn. The warn fires only on the real stranded sweep, where the ledger truly disagrees. The log and every comment the fix touched state the truth.
- PIN-RACE3. The bar of this round. 120 rounds of 3 concurrent openers of one stranded fixture, staggered 0 to 20 milliseconds. 360 opens, 0 refused, 0 raw failures, 0 bricked files. Every round ended with 6 ledger rows beside 8 tables.
- PIN-WIDE. 30 rounds of 6 concurrent openers: 180 opens, 0 raw failures, 0 bricked files.
- PIN-CXL. 40 rounds of 3 openers whose contexts die at random instants inside the open, so the file collects the crash states the convergence story covers. Every round converged, 0 bricks, counts agree.
- PIN-B, PIN-ISO, PIN-FOLD. The literal v0.7.0 fixed-name file reopens with no migration and keeps its rows. Two namespaces isolate across charges, holds, settle-once pairs and schema names. The case twin is one namespace. Round 1's approved behaviours stand.
- Baseline contrast. The same harness at the round 2 head `9f1bb3b` fires in round 6. It records one raw `already exists` failure and one bricked file with 0 ledger rows beside a full table set. The harness catches the shape the round 2 finding described.
- mutU. The handed head with the guard's condition forced true, one line. The identical brick fires in round 6 with the identical signature. The guard is load-bearing for PIN-RACE3.
- mutC. The comparison miscounted by one, `rows == schema.fileCount()-1`. PIN-REFUSE fires, the corrupt file opens. The comparison is load-bearing for the refusal. The race pins still pass under mutC, so the comparison guards classification alone.
- mutF. The superset-aware form `rows >= schema.fileCount()`. PIN-SUPER reports `holds 7 of its 8 tables` matching ErrInvalid, and PIN-GUARD, PIN-INT, PIN-REFUSE, PIN-WARN, PIN-RACE3, PIN-WIDE, PIN-CXL, PIN-B, PIN-ISO and PIN-FOLD all still pass.
- The atomicity claim survived its attack. The guard's count and the erase share the one implicit write transaction the DELETE statement opens, so no other opener can commit between them. The measured bar agrees. 540 clean opener runs across 3-way and 6-way concurrency, plus 40 rounds of cancelling openers, produced no raw failure and no brick. The same harness fires within 6 rounds on the unguarded shape.
- `apidiff -m ./api/v0.7.0.export .` printed nothing, so the round 3 fix grew no exported surface. It adds only the unexported `fileCount` and `countLedgerRows`.

## Gate evidence

`GOCACHE=/tmp/keel-rev85r3-gocache ./tools/check.sh` ran in the review worktree and passed all 14 checks, `go test -race ./...` included. The last line reads `all checks passed`.

## Zero residue against prior rounds

| Round | Finding | Ruling |
|---|---|---|
| 1 | H, a pre-fix custom-namespace file failed to open with a raw error while the body promised a fresh store | Fixed and stays fixed. The stranded fixture reopens as a fresh store in every one of the 190 race and cancellation rounds, each verified by a canary open and fresh counts. PIN-B and PIN-ISO confirm the default-namespace and isolation behaviours beside it. |
| 1 | L, the ForgetOwner comment named default-only table spellings | Fixed and stays fixed. The comment speaks of the owner budget table, the reservation rows and the charge rows. A grep for the fixed spellings in `keyed.go` returns nothing. |
| 2 | H, the reset's unguarded ledger delete bricked a stranded file under concurrent opens | Fixed and verified. The sweep is one statement whose guard and erase share the write transaction, PIN-GUARD measures 0 erased rows beside standing tables, and 540 raced opens plus 40 cancellation rounds left no raw failure and no brick. The same harness bricks the round 2 head and the mutU tree within 6 rounds. |

Claim, severity by severity. Round 1 held one H and one L, and round 2 held one H. No residue of any of the three survives at its own severity in the handed tree. This round adds one new M in the comparison the round 2 fix introduced, and nothing else.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. `apidiff -m ./api/v0.7.0.export .` printed nothing. The round 3 fix adds only unexported identifiers.
2. **Does the change, its issue, or its pull request name a consumer?** No. It speaks of an application with two budgets, and the consumer-names check passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The round 3 fix adds no import. The sqlite and TOML edge checks pass.
4. **Does any code, comment or test cite a planning file or a private note, or did stripping one lose the reason it carried?** No. The plan-reference check passes. The Open doc's boundary statement matches the code: the runner runs first, an interrupted re-apply converges there, and a ledger short beside a full table set fails raw in the runner, exactly as the doc says.
5. **Does the README package table still match the filesystem, package for package?** Yes. `cost/sqlitestore` keeps its row and the change adds no package.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes. All pins, mutations and the gate ran against the confirmed detached hash in `/home/nryn/work/keel-wt/rev-85-r3`, which stayed clean. The mutations ran in scratch worktrees under `/tmp`.

## Verdict

**REMEDIATE.** One M and nothing else.

The refusal's completeness comparison accepts one exact ledger size, so a class of damaged file the docs promise to refuse opens instead and fails raw at read time. The fix is one line, `rows >= schema.fileCount()`, and mutF shows every other measured behaviour survives it. Fix it as a new commit, and a fresh agent re-reviews.
