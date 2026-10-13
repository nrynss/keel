# Issue 82 review, round 1

Reviewed hash: `e86c2e86464d90636c428eb2b90702d50a2cbcb4`, the handed head of pull request 85.
Worktree: `/home/nryn/work/keel-wt/rev-85-r1`, detached and clean.
`git rev-parse HEAD` returned the handed hash and `git status --porcelain` was empty before the review started.
I read `git diff 9857f86..HEAD`, issue 82, the pull request body, the cost section of the README, and every file the diff touches.
The worktree stayed clean at the handed hash through the whole review.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 1 | 0 | 1 |

## Findings

| Severity | Location | Defect | Pin | Mutation |
|---|---|---|---|---|
| H | `cost/sqlitestore/sqlitestore.go` Open path, and the pull request body | A pre-fix custom-namespace file fails to open with a raw SQLite error. The body promises a fresh store, so an upgrading deployment cannot start and the error names no fix. | PIN-C below. Reopen as credits errors with `no such table: credits_budget`, and `errors.Is` against `ErrInvalid` is false. | Drop the ledger rows on a copy of the fixture. The same binary then opens a fresh store, so the pin measures exactly the ledger that vouches for tables the prefix never received. |
| L | `cost/sqlitestore/keyed.go`, the ForgetOwner comment | The comment names `cost_owner_budget` and `cost_charge`, spellings only the default namespace owns after this change. | The comment text, beside PIN-A's object list, which shows `credits_owner_budget` for a credits store. | The handed comment text is the defect. Rewriting it to the namespace-qualified wording flips the finding. |

## Independent pins

The probe lives at `/home/nryn/work/keel/dev-diary/probes/rev85r1/`.
Run `GOCACHE=/tmp/keel-rev85r1-probe-gocache ./run.sh` from that directory.
The script extracts the v0.7.0 tree from git at 9857f86, builds the files that version leaves on disk, and measures the reviewed worktree.
Every assertion reads a fresh `database/sql` connection, so the store under review never grades itself.

- PIN-A. It opens usd and credits stores on one file and drives charges, holds, grants, settle-once pairs, an owner ceiling and a quoted run. It listed 40 schema objects over a fresh connection, every table and every index qualified, and no v0.7.0 name present. A `SELECT` on `cost_charge` failed with no such table, so the unqualified tables do not exist. The two budget rows hold different denominations, so the guard reads its own namespace row.
- PIN-S. The default-namespace schema text of the reviewed build equals the schema text of a file the v0.7.0 build wrote, byte for byte, ledger DDL included.
- PIN-B. A file hand-built from the literal v0.7.0 migration files, with the ledger rows seeded, reopens under the default namespace. The seeded charge and spent 30 read back, the ledger keeps six rows, and no object appears.
- PIN-C. The v0.7.0 build leaves a credits-namespace file with its rows in unqualified cost tables and six ledger rows. The reviewed build refuses to open it as credits with `no such table: credits_budget`, and `errors.Is` against `ErrInvalid` is false. Opened as default it fails with `table cost_charge already exists`. Dropping the ledger rows on a copy yields a working fresh store, and the old charge row stays unread on disk.
- Mutations. mutA, one line in `newTables` dropping the prefix, made PIN-A fail with `no such table: cost_charge` in both namespaces while PIN-S and PIN-B still passed. mutB, `qualifiedSchema` serving the token unrewritten, failed every open at the first migration with an unrecognized-token error.
- The migration ledgers of both namespaces record the original file names, `0001_cost.sql` through `0006_cost_settle_once.sql`, read back with the sqlite3 CLI.
- `apidiff -m ./api/v0.7.0.export .` printed nothing, so the exported surface is unchanged from v0.7.0.

## Gate evidence

`GOCACHE=/tmp/keel-rev85r1-gocache ./tools/check.sh` ran in the review worktree and passed all 14 checks, the race suite included.
The last line reads `all checks passed`.

## Zero residue against prior rounds

No prior round exists for this pull request. This is round 1, and no round was skipped.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. The change adds no exported identifier, and apidiff against the v0.7.0 export is silent.
2. **Does the change, its issue, or its pull request name a consumer?** No. It speaks of an application with two budgets, and the consumer-names check passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The new imports are bytes, embed, io/fs, sort and strings, all standard library, inside the SQLite edge package this change already belongs to.
4. **Does any code, comment or test cite a planning file or a private note, or did stripping one lose the reason it carried?** No. The plan-reference check passes, and the rewritten migration comment states its reason itself.
5. **Does the README package table still match the filesystem, package for package?** Yes. The table and the sqlitestore paragraph match the tree, and the change adds no package.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes. All pins, mutations and the gate ran at the confirmed detached hash in `/home/nryn/work/keel-wt/rev-85-r1`, which stayed clean.

## Verdict

**REMEDIATE.** One H and one L.
The H finding contradicts the pull request body's own back-compat claim, so the fix lands as new commits on the branch.
