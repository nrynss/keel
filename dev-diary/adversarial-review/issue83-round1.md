# Issue 83 review, round 1

Reviewed hash: `de89a148d955eafa005c3fd06cd58be110b876bf`, the handed head of
pull request [#84](https://github.com/nrynss/keel/pull/84) against issue
[#83](https://github.com/nrynss/keel/issues/83).
Worktree: `/home/nryn/work/keel-wt/rev-84-r1`, detached and clean.
`git rev-parse HEAD` matched the handed hash and `git status --porcelain` was
empty before the review started.
I read `git diff 9857f86..HEAD` in full, the issue, the pull request, the job
README section, and the tests the diff touches.
The diff touches seven files under `job/` and `job/sqlitestore/` and nothing
else.

## The open judgment on blank codes

The pull request asks whether a Failure with a blank Code and a non-nil Detail
should publish the `job_failed` code and keep the Detail.
I rule that this is the right contract.

- The wire envelope treats its members independently. `wire.NewErrorEvent` and
  `WriteError` publish any detail that does not encode to null, whatever the
  code says. A fallback code beside a detail is not a new shape.
- The alternative drops data the work named. A one-field slip would silently
  erase the detail, which is the worse outcome for the same input.
- No client breaks. Clients branch on the code, `job_failed` keeps its
  meaning, and Detail stays optional and app-read.

The probe pins the behaviour, and mutation A below shows the pin rejects the
alternative contract.

## Severity counts

| C | H | M | L |
|---|---|---|---|
| 0 | 0 | 0 | 3 |

All three L rows are recorded false positives. I judged each defect not real.
Nothing blocks the merge.

## Findings

| Severity | Location | Claimed defect | Pin | Mutation | Disposition |
|---|---|---|---|---|---|
| L | `job/job.go`, over-cap branch of `Runner.run` | A result over the cap stores an empty Code while a plain error stores `job_failed`. | Probe line `p4 over-cap Result.Code empty means fallback` passed with `code=""` while the envelope still carried `job_failed`. | Mutation C removed the `publishTerminal` fallback. Probe line `p4 over-cap keeps job_failed` then failed with `code=""`. The fallback is what keeps the wire stable. | False positive. `Result.Code` and `Record.Code` define empty as the `job_failed` fallback, and `TestResultCapFailsJob` pins the empty value on purpose. The wire never shows a difference. |
| L | `job/job.go`, `Runner.run` Failure extraction | A `*Failure` in the chain is not found by `errors.As` with a `job.Failure` target, so its code and detail drop. | Probe `degenerate` printed `pointer-failure errors.As found=false` for a wrapped `*Failure` and `found=true code="input_rejected"` for the value form. | Making `run` also match a non-nil `*Failure` would add the behaviour. | False positive. The doc comment says return it bare or wrapped. Failure is a value type by house style, and a dropped code fails loudly in testing. `wire.NewErrorEvent` grants the same trust to its caller. |
| L | `job/job.go`, `Failure.Error` | A Failure with a nil Err and an empty Code renders `job: ` with a trailing space. | Probe `degenerate` printed `Failure{}.Error()="job: "`. | None. The value is degenerate and the consumer constructs it. | False positive. The message is never branched on, the doc says the code is the fallback text, and the envelope shape stays valid. |

## Independent pins

The probe lives at `/home/nryn/work/keel/dev-diary/probes/rev84r1/`.
It is a nested module with its own `go.mod`, so the `dev-diary/probes/go.mod`
boundary stays intact.
Its `replace` directive points at this review worktree, so every pin ran
against the handed hash.
Run `go run .` and `go run ./degenerate` from that directory.
The main probe printed 36 PASS lines and `PROBE RESULT: ALL PINS PASS`.

- The envelope pin drives a coded wrapped Failure, a blank code with detail, a
  plain error, and a panic through a real `Runner`. It runs them over a real
  `sqlitestore` and a broker the probe owns, plus one over-cap job and one
  cancelled job. It serializes each terminal through `wire.WriteEvent` into a
  recorded `ResponseWriter` and parses the frame back with `wire.ReadEvent`, so
  the bytes are what a browser receives. The coded case carried
  `code="budget_exhausted"`, the message `plan: reserve: over budget`, and the
  detail bytes `{"retry_after":30}`. The blank-code case carried `job_failed`
  and kept the detail. The plain case carried `job_failed` with no detail
  member. Panic, over-cap and cancel kept their pre-change semantics.
- The SQLite pin opened the job file with a fresh `modernc.org/sqlite`
  connection the Runner never touched. It read `error_code` and `error_detail`
  straight from three `status='error'` rows: `budget_exhausted` with its
  detail, `job_failed` with the blank-code detail, and `job_failed` with an
  empty detail for the plain error. The error text column kept the composed
  message.
- The migration pin built a v0.7.0 database by hand: the 0001 schema only, a
  `job_schema_migrations` ledger row for `0001_jobs.sql`, and one failed row
  written through the old column list. The new `sqlitestore.Open` applied 0002
  over it. The old row then decoded with an empty Code, a nil Detail, its error
  text, and its result blob intact. The ledger held both files in order.
- The durable read pin confirms `Result` through the store keeps the code and
  detail but only the error's text. That is the documented store contract, and
  it is the reason the code column exists.

apidiff judged the frozen surface. I wrote the build's export data with
`apidiff -m -w` and compared it against `api/v0.7.0.export`.

```text
Compatible changes:
- ./job.Failure: added
- ./job.Record.Code: added
- ./job.Record.Detail: added
- ./job.Result.Code: added
- ./job.Result.Detail: added
```

No existing export changed shape or semantics. The five additions match the
pull request claim, so v0 stays intact.

I ran three mutations against a throwaway copy of the tree, never against the
reviewed branch. Each one failed the pin that guards it.

- Mutation A moved the detail encoding inside the non-blank code branch. Probe
  lines `p1 blank-code detail still travels` and `p2 blank-code row column`
  failed. The blank-code ruling is load-bearing.
- Mutation B made `Finish` write an empty `error_code`. Probe lines
  `p1 coded Result.Code` and `p2 coded row column` failed. The durability pin
  catches a lost code.
- Mutation C removed the `publishTerminal` empty-code fallback. Probe line
  `p4 over-cap keeps job_failed` failed with `code=""`. The fallback carries
  the unchanged paths.

## Gate evidence

`GOCACHE=/tmp/keel-rev84r1-gocache ./tools/check.sh` passed all 14 checks in
this review worktree at the handed hash.
The first attempt needed network access to fetch pinned staticcheck, and the
run passed with network access.
The tail read `== check 14/14 api == ok` and `all checks passed`.
The race run reported `ok github.com/nrynss/keel/job 2.262s` and
`ok github.com/nrynss/keel/job/sqlitestore 1.537s`.
`git diff 9857f86..HEAD --name-only -- wire testdata` printed nothing, so no
golden file moved and the envelope shape is untouched. No Chaaya coordination
is needed. PR CI ran `check` green at the same commit. The worktree stayed
clean after the gate and the pins.

## Zero residue against prior rounds

No prior round exists for this pull request. This is its first review, and
none was skipped.
The only prior note on the change is the CodeRabbit comment about `*Failure`,
which the second findings row answers with a measurement.

## The six review questions

1. **Does any exported identifier exist that no consumer needs?** No. The
   issue asks for the Failure escape hatch. `Result.Code` and `Result.Detail`
   carry the code to the catch-up path a page reload takes. `Record.Code` and
   `Record.Detail` are what a Store implementer must persist, so the restart
   reports the stream's code. apidiff found only the five claimed additions.
2. **Does the change, its issue, or its pull request name a consumer?** No.
   The issue speaks of any app with predictable paid-job failures. The
   consumer-name scan passes.
3. **Does a core package now import SQLite or the TOML parser?** No. The diff
   adds only stdlib `strings` to `job`. The sqlite edge, the parser edge, and
   the chain edge checks all pass.
4. **Does any code, comment or test cite a planning file or a private note,
   or did stripping one lose the reason it carried?** No. The plan-reference
   scan passes. The new comments state their reasons directly, including the
   blank-code rule and the null-detail rule.
5. **Does the README package table still match the filesystem, package for
   package?** Yes. The diff adds no package, so no row is owed. I walked the
   table against the top-level directories and the store paragraph. All
   matched.
6. **Did the review run on the handed commit, in a checkout of its own?** Yes.
   The hash matched at the start, the worktree stayed clean, and the probe
   module's `replace` pointed at this worktree for every pin.

## Verdict

**APPROVE.** Zero C, H, or M findings. Three L rows, each recorded a false
positive with a pin and a disposition. The open judgment on blank codes is the
right contract. Zero residue, because no prior round exists.
