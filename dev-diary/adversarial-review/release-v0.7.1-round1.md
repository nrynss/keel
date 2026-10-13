# Release v0.7.1 review, round 1

Reviewed hash: `73f261bffce9b9eae74c92eef2cfd515558d8f68`, the head of pull request 86, one commit on top of main at `37b362d`.
Checkout: `/home/nryn/work/keel-wt/rev-86-r1`, detached and clean.
`git rev-parse HEAD` returned the handed hash and `git status --porcelain` was empty before the review started and stayed empty through it.
I read AGENTS.md, `dev-diary/project.md`, the pull request body, `git diff 37b362d..73f261b` in full, and the merged diffs the release carries. Pull request 84 is `git show eac9859`. Pull request 85 is `git diff eac9859..37b362d`.
The diff adds exactly two files, `api/v0.7.1.txt` with 7833 lines and `api/v0.7.1.export` at 225754 bytes. It touches no Go file.
Pull request 84 contributes every new export. It adds `job.Failure` with its `Error` and `Unwrap` methods, the `Code` and `Detail` fields on `job.Result` and `job.Record`, and rewords the `Store.Finish` doc comment. Pull request 85 changes `cost/sqlitestore` internals only.
I rebuilt both records with the exact commands `tools/freeze.sh` and `tools/check.sh` use, byte-compared them, and ran two mutations in a scratch worktree so the reviewed tree never moved.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 0 | 0 | 0 |

## Findings

None. The pair is honest about the tree it freezes, the delta against v0.7.0 is additions only, and the patch bump obeys the policy.

## Independent pins

The probe lives in the scratch worktree `/tmp/keel-rel86-mutation`, cut from the reviewed hash with `git worktree add --detach /tmp/keel-rel86-mutation 73f261b` and removed after the runs. It sat outside every module root, so no probe package joined the tree CI compiles. Every other command ran at the confirmed detached hash in the review checkout. Record hashes, measured by `sha256sum`: `7ac9491a52e6d4958bc8b3017fd1fa52232af2e830a323f5158d619587597c87` for `api/v0.7.1.txt` and `2538346057e4d69adc386836797468b474a21b5b7f748afe279078a708a187ad` for `api/v0.7.1.export`.

- PIN-DOC. I rebuilt the transcript in the review checkout. One `########## <pkg>` header per package, `GOOS=linux GOARCH=amd64 go doc -all` per package, one blank line between packages, over every package `go list ./...` reports except `/tools`. `cmp` against `api/v0.7.1.txt` reported no difference. The committed transcript equals a fresh regeneration, byte for byte.
- PIN-EXPORT. I installed apidiff at `v0.0.0-20260908205506-85c1c2202aba`, the version both tools pin. I wrote a fresh export with `GOOS=linux GOARCH=amd64 apidiff -m -w /tmp/keel-rel86-fresh.export github.com/nrynss/keel`. `cmp` against `api/v0.7.1.export` reported no difference. The committed export equals the build.
- PIN-DELTA. `GOOS=linux GOARCH=amd64 apidiff -incompatible -m api/v0.7.0.export github.com/nrynss/keel` printed nothing and exited 0. The plain report lists five compatible additions and nothing else: `./job.Failure`, `./job.Record.Code`, `./job.Record.Detail`, `./job.Result.Code`, `./job.Result.Detail`. That matches the pull request body's claim exactly. `job.Failure` arrives with `Error` and `Unwrap`, which the transcript section shows. The transcript delta between the two records touches the job package alone, and inside it carries only the five additions plus the `Store.Finish` doc-comment reword that pull request 84 merged. No signature changed, so the additions are compatible and the patch bump is legal.
- PIN-HEADERS. The `##########` header sets of `api/v0.7.0.txt` and `api/v0.7.1.txt`, compared with `comm` in both directions, are identical. Each side holds 40 packages. No package dropped, which apidiff alone cannot see.
- PIN-ORDER. `sort -V` orders v0.7.0 strictly before v0.7.1. `git diff --name-status --no-renames 37b362d..73f261b` names exactly `A api/v0.7.1.txt` and `A api/v0.7.1.export`. No earlier record moved.
- PIN-MUTATE-ADD. In the scratch worktree I added one exported constant to package job and reran both pins. The regenerated transcript differs, the fresh export differs, and the plain apidiff report names the addition. The same run measured that `apidiff -incompatible` alone stays silent on a compatible addition. The gate's export check therefore needs the transcript check beside it to pin additions. Check 14 runs both. My pins ran the byte compares, and both caught the addition.
- PIN-MUTATE-BREAK. In the scratch worktree I deleted the exported `Failure.Unwrap` method, a one-line edit that still compiles. `GOOS=linux GOARCH=amd64 apidiff -incompatible -m api/v0.7.1.export github.com/nrynss/keel` printed `- ./job.Failure.Unwrap: removed`. The bump-policy command is load-bearing, and a patch bump carrying that edit would refuse. A first mutation attempt changed a field type and broke compilation, so apidiff could not load the package and proved nothing. The compiling deletion is the recorded evidence.
- PIN-GATE. `GOCACHE=/tmp/keel-rel86-gocache ./tools/check.sh` ran in the review checkout at the committed HEAD and passed all 14 checks, `go test -race ./...` included. The log holds `== check 1/14 gofmt ==` through `== check 14/14 api ==`, each followed by `ok`, and ends with `all checks passed`. One measured property of that run belongs in this record. At a clean commit nothing under api/ is staged, so check 14 takes its ordinary path and re-judges no record. The freeze run judged the pair on its staged bytes. PIN-DOC, PIN-EXPORT, PIN-DELTA and PIN-HEADERS re-judge the committed bytes with the same commands plus stronger byte compares, and they agree.

## Gate evidence

The full `./tools/check.sh` run passed at the reviewed commit with a fresh cache under `/tmp/keel-rel86-gocache`. Its tail reads:

```
== check 13/14 conventions ==
ok
== check 14/14 api ==
ok
all checks passed
```

The log holds `ok` after every one of the 14 check headers, the race suite included.

## Zero residue against prior rounds

This is round 1, the first review of pull request 86. No prior round exists for it, so no residue is possible. I claim zero residue against every prior round, trivially, and no finding of my own at any severity.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. The release records five additions and adds none itself. Each addition carries a reviewed need from pull request 84 and issue 83, a client that branches on a stable failure code instead of an error message. Pull request 85 added no export at all.
2. **Does the change, its issue, or its pull request name a consumer?** No. I grepped the diff for the banned names and found no hit. The pull request body speaks of applications and existing consumers in general, and the consumer-names check passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The change touches no Go file. The sqlite, TOML, PDF, object storage and chain edge checks all pass at this commit.
4. **Does any code, comment or test cite a planning file or a private note, or did stripping one lose the reason it carried?** No. The diff is two generated records over the documented tree. The plan-reference scan passes, and my own grep over the diff found no planning reference.
5. **Does the README package table still match the filesystem, package for package?** Yes. The table holds 30 rows, each naming a package an application imports, `config/source` and `mediastore/s3` included. The paragraph below names the 10 `*/sqlitestore` packages one directory down. Together they cover all 40 packages `go list ./...` reports beside `/tools`. The five `config/source` kinds that also take backtick rows are source kinds, not packages, and are documented inside the config section. This change adds no package.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes. Every pin and the gate ran at the confirmed detached hash `73f261b` in `/home/nryn/work/keel-wt/rev-86-r1`, which stayed clean throughout. The mutations ran in the scratch worktree `/tmp/keel-rel86-mutation`, which the run removed afterwards.

## Verdict

**APPROVE.** No findings at any severity. Both committed records equal a fresh regeneration and a fresh build byte for byte. The delta against v0.7.0 is exactly the five claimed additions, no package dropped, and the version moves forward. The patch bump obeys the policy, and the gate passes all 14 checks at the reviewed commit.
