# Issue 82 review, round 2

Reviewed hash: `9f1bb3b7888bc8cb7d9d65f1fb7ac1083cb8dd2f`, the handed head of pull request 85.
Worktree: `/home/nryn/work/keel-wt/rev-85-r2`, detached and clean.
`git rev-parse HEAD` returned the handed hash and `git status --porcelain` was empty before the review started.
I read `git diff 9857f86..HEAD` in full, the round 1 review with its probe, issue 82, the pull request body, and the sqlite package's migration runner.
I read the two remediation commits `e2b9f43` and `9f1bb3b` against the round 1 head `e86c2e8`.
The worktree stayed clean at the handed hash through the whole review.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 1 | 0 | 0 |

## Findings

| Severity | Location | Defect | Pin | Mutation |
|---|---|---|---|---|
| H | `cost/sqlitestore/sqlitestore.go`, `reconcileSchema`, the reset branch | The ledger delete and the re-apply are unguarded statements, so concurrent opens of one stranded file do not converge the way the branch's own comment claims. A delete landing between another process's per-file commits erases recorded rows whose tables already exist. Every later open then fails with the raw `table credits_charge already exists` error, the very failure the issue complains about, and no open heals it. The body's claim that no ordinary path leaves part of a schema behind is false for this path. | PIN-RACE and PIN-BRICK below. Two of sixty rounds left the file permanently unopenable with an empty ledger beside a full table set. A second run bricked four of sixty. | The baseline fires the pin, so the shipped code reintroduces its own defect. Holding one write transaction across the count, the delete and the re-apply, or re-counting after the re-apply and repeating the reset until the count reads eight, stops the pin firing. |

### The finding in detail

The reset branch counts eight absent tables, deletes the namespace ledger in one autocommit statement, and re-runs the runner, which commits one file per transaction. Nothing holds the write path across those steps and nothing re-checks the count afterwards. Two processes opening one stranded file together therefore interleave. The measured losing interleaving runs as follows. Both processes count zero and both delete. The faster process re-applies the files. The slower process's delete lands between two of those commits and erases rows the finished tables vouch for. The slower process's own re-apply then fails at the first file, because the table exists while the row that would let it skip is gone. The faster process finishes, counts eight tables, and opens a store whose ledger no longer records files already applied. The surviving opener cannot detect the damage. Every later open fails inside `sqlite.Migrate`, which runs before `reconcileSchema`, so the new guard can never see or heal the state it produced.

The comment on the reset branch says two processes converge through the runner's concurrent-open handling. That handling covers racers that both apply into an empty ledger. It does not cover a racer that deletes rows out of a filling one. The pull request body says a partly present table set refuses the open because no ordinary path leaves part of a schema behind. The reset path leaves a file whose tables are all present and whose ledger is short, which is a worse shape than a partial table set. The open refuses nothing there. It fails raw forever instead.

## Independent pins

The probe lives at `/home/nryn/work/keel/dev-diary/probes/rev85r2/`.
Run `./run.sh` from that directory.
The script extracts the v0.7.0 migration files from git at `9857f86`, builds every pre-fix fixture from those literal files, and measures the reviewed worktree.
Every assertion and every canary read through a fresh `database/sql` connection, so the store under review never grades itself.

- PIN-R1. The stranded pre-fix credits file carries the literal old migration tables, one charge and a spent 30 ceiling under the old names, and six ledger rows under `credits_schema_migrations`. The new build reopens it as credits. The open succeeds, the credits tables come back empty, the old rows stay on disk, and the ledger holds six rows under the original file names. This is the round 1 H fix, verified.
- PIN-R2. A working credits store with `credits_grant` dropped over a fresh connection refuses the reopen with an error matching `ErrInvalid`.
- PIN-X. A file holding a full usd store and a full stranded credits ledger opens as credits only through the reset. The usd charge row and ledger stay untouched. The count binds its eight qualified names and never rides the other namespace's tables.
- PIN-CRASH. A stranded fixture taken through the reset and the first three re-applied files converges on the next open. That open finishes the re-apply, leaves six ledger rows and eight tables, and the stranded row stays unread under `cost_charge`. The single-process crash story holds.
- PIN-BRICK. A working credits store with one ledger row deleted keeps all eight tables beside five ledger rows. The reopen fails with `sqlite: migrate: apply 0001_cost.sql: sqlite: migration: SQL logic error: table credits_charge already exists (1)`. `errors.Is` against `ErrInvalid` is false. A second reopen fails the same way, so the state never heals.
- PIN-RACE. Sixty rounds, each opening one fresh stranded fixture from three handles at once with a zero to fifteen millisecond stagger. Recorded run: 177 opens succeeded, 3 failed with raw already-exists errors, and 2 rounds left the file permanently unopenable with `ledger rows = 0` beside a full table set. An earlier identical run recorded 173, 7 and 4. Across the two runs, 6 of 120 files were bricked. One recorded round shows two openers failing on different files, the signature of a delete landing between another process's commits. Two handles in one process lock the file exactly as two processes do.
- PIN-B, PIN-ISO, PIN-FOLD. The literal fixed-name default file reopens with no migration and keeps its six ledger rows. Two namespaces isolate both ways across charges, holds, settle-once pairs and schema names. The case twin is one namespace. Round 1's approved behaviours stand.
- Test edits. `git diff e86c2e8..HEAD` touches the test files additively only, 116 added lines and no edited or deleted assertion.
- Mutations. mutR1 inserts `return nil` into the zero case: PIN-R1 fails with `read budget: no such table: credits_budget`, the raw error of round 1, and all 180 race opens fail raw. The body's claim that mutating the reset away fails the stranded reopen with the raw missing-table error is true. mutR2 widens the zero case to `present <= len(names)`: the one-table-short store is healed into a raw already-exists failure and PIN-R2 fails, while the race still bricks files. mutR3 voids the name binding with `OR 1 = 1`: PIN-R1 fails with `holds 10 of its 8 tables`, counting the old default tables and both ledgers.
- `apidiff -m ./api/v0.7.0.export .` printed nothing, so the remediation grew no exported surface.

## Gate evidence

`GOCACHE=/tmp/keel-rev85r2-gocache ./tools/check.sh` ran in the review worktree and passed all 14 checks, `go test -race ./...` included. The last line reads `all checks passed`.

## Zero residue against prior rounds

| Round | Finding | Ruling |
|---|---|---|
| 1 | H, a pre-fix custom-namespace file failed to open with a raw error while the body promised a fresh store | Fixed and verified. PIN-R1 reopens the rebuilt stranded file as a fresh store, the old rows stay on disk unread, and the ledger is re-recorded under the original names. The reset that delivers this carries the new round 2 H finding above. |
| 1 | L, the ForgetOwner comment named default-only table spellings | Fixed and verified. The comment now speaks of the owner budget table and the charge rows, and the quote row comment lost its `cost_quote` spelling. |

Claim, severity by severity. Round 1 held one H and one L. Both are fixed in the handed tree, and no residue of either survives at its own severity. Round 2 adds one new H in the code that implements the round 1 H fix.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. `apidiff -m ./api/v0.7.0.export .` printed nothing. The remediation adds only unexported identifiers: `schemaLedgerSuffix`, `names`, `reconcileSchema` and `countPresentTables`.
2. **Does the change, its issue, or its pull request name a consumer?** No. It speaks of an application with two budgets, and the consumer-names check passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The remediation adds no import. The strings import the reset uses was already in the SQLite edge package this change belongs to.
4. **Does any code, comment or test cite a planning file or a private note, or did stripping one lose the reason it carried?** No. The plan-reference check passes. The reset branch's comment cites no planning file, but it states a reason the code does not deliver, and the H finding records that.
5. **Does the README package table still match the filesystem, package for package?** Yes. `cost/sqlitestore` keeps its row and the change adds no package.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes. All pins, mutations and the gate ran at the confirmed detached hash in `/home/nryn/work/keel-wt/rev-85-r2`, which stayed clean.

## Verdict

**REMEDIATE.** One H and nothing else.
The reset at open bricks a stranded file under the concurrent opens the feature invites, contradicts its own comment, and contradicts the body's claim about partial schemas. Hold the reset against concurrent openers, then a fresh agent re-reviews.
