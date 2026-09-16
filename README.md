# Keel

Go building blocks for a media app that sits on a public URL, calls paid APIs, and runs on one box.

Every product built that way needs the same machinery underneath it. A gate so the URL is not an
open wallet. Long work that outlives its request and reports progress. Media stored privately and
served with seeking. ffmpeg that dies with its job. A record of what everything cost. Keel is that
machinery, so each product stops rebuilding it.

Keel is a set of small packages, not a framework. Each one takes a `Config`, returns a concrete
type, and hands back `http.Handler` values that `net/http` serves. Nothing reads the environment,
nothing logs on its own, and nothing panics.

## Install

```bash
go get github.com/nrynss/keel@v0.1.1
```

Go 1.27 or newer. The `ffmpeg` package runs the `ffmpeg` and `ffprobe` binaries, so install those
if you use it. Everything else is pure Go, and `CGO_ENABLED=0` builds the whole module.

## Packages

| Package | Does |
|---|---|
| `wire` | The error envelope and the event contract every response and stream shares |
| `id` | Unguessable 128-bit ids in lowercase hex, for anything served in public |
| `gate` | Per-client and global token buckets, plus an optional passcode, in front of the routes that spend money |
| `stream` | Topic broker and server-sent events, with heartbeats and a non-blocking slow-subscriber policy |
| `job` | Long work started by a short request, observed over `stream`, durable across restarts |
| `mediastore` | Blobs on disk under unguessable ids, served with Range support, with retention sweeps |
| `upload` | Resumable chunked uploads that land in `mediastore`, resumable by id after a dropped connection |
| `sqlite` | One SQLite file with WAL, a writer handle, a read-only reader pool, namespaced migrations and online backup |
| `ffmpeg` | ffmpeg and ffprobe bound to a context, with a bounded wait on shutdown |
| `cost` | Money as integer nanodollars, a ledger of charges, and a budget that refuses before a call |
| `config` | TOML settings, and secret references that resolve from the environment, a file, a directory or a command, never holding a value in the file |

The stores that need SQLite live one directory down, in `job/sqlitestore`,
`mediastore/sqlitestore` and `cost/sqlitestore`. Only those packages import a SQLite driver, so an
app that uses `gate` alone never compiles one.

## Using it

### Guard what spends money

```go
g, err := gate.New(gate.Config{})
if err != nil {
	return err
}

handler, err := g.Protect(gate.Rule{
	Name:      "render",
	PerClient: gate.Limit{Burst: 3, Every: time.Hour},
	Global:    gate.Limit{Burst: 100, Every: time.Hour},
}, renderHandler)
```

Every rule carries two buckets. The per-client bucket is fairness between callers, and it is keyed
on an address a client behind a proxy can forge. The global bucket is what actually bounds spend.
A refusal takes no token from either bucket and answers with the `wire` envelope.

### Run long work and report progress

A request that starts work returns an id at once. Nothing holds a response open, because a proxy
in front of the app may cut a request at 100 seconds.

```go
db, err := sqlite.Open(ctx, sqlite.Config{Path: "app.db"})
store, err := jobsql.Open(ctx, jobsql.Config{DB: db})
runner, err := job.Open(ctx, job.Config{Broker: stream.New(stream.Config{}), Store: store})

jobID, err := runner.Start(ctx, func(ctx context.Context, progress func(job.Progress)) ([]byte, error) {
	progress(job.Progress{Stage: "encode"})
	return []byte("mp3-bytes"), nil
})
```

The job keeps the request's values and drops its cancellation, so a client that goes away does not
kill the work. State lives in SQLite, so a restart does not lose it. Unfinished work comes back as
`interrupted` rather than running again, because a repeat of a paid call spends the money twice. A
kind that is safe to repeat opts in.

The browser follows the job over server-sent events:

```go
mux.HandleFunc("GET /api/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
	broker.ServeTopic(w, r, "job:"+r.PathValue("id"))
})
```

Subscribe first, then read the job's state. A topic can retain its terminal event, so a subscriber
that joins late still learns the outcome.

### Store media privately and serve it with seeking

```go
index, err := mediasql.Open(ctx, mediasql.Config{DB: db})
store, err := mediastore.Open(ctx, mediastore.Config{Dir: "blobs", Index: index})

blobID, err := store.Persist(ctx, reader, mediastore.Put{
	ContentType: "audio/ogg",
	Owner:       "alice",
	Visibility:  mediastore.Private,
})
```

Blobs are private by default. A private blob is served `private, no-store` and only through the
authorizer the config carries, and a refused request gets a 404 rather than a 403, so a refusal
does not confirm the blob exists. A public blob is cached forever under its unguessable id. The
handler answers Range requests, which is what audio and video seeking needs.

Bytes reach disk before the row that names them, so a crash never leaves a row pointing at nothing.
The sweeper reclaims orphans and evicts whole groups over a byte budget.

### Accept a recording that survives a dropped connection

`upload` takes a file in chunks, each with its own hash, in any order. A repeat of the same chunk
is accepted. A chunk whose hash differs is refused. Completion assembles the chunks, verifies the
whole file, and persists it into `mediastore` under an id the client already knows.

### Count what it cost, and refuse before you overspend

```go
budget, err := cost.NewBudget(cost.USD(150))
if err := budget.Reserve(cost.USD(4.50)); err != nil {
	return err // over budget, before the paid call
}
// make the paid call, then record what it really cost
budget.Settle(cost.USD(4.50), actual)
```

`cost.Price` counts nanodollars in an `int64`, so no rounding creeps in. `cost/sqlitestore` keeps
the ledger and the reservations across a restart, and a reservation that expires releases itself.

### One error shape, everywhere

```go
wire.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests", detail)
```

```json
{"error":{"code":"rate_limited","message":"too many requests","detail":{"retry_after_seconds":3}}}
```

Clients branch on `code`, never on `message`. The same package writes and reads the event frames
that `stream` and `job` publish, so a browser client and the server agree on one shape.

## Examples

Every package carries a runnable example that shows its common case.

```bash
go test -run Example -v ./...
```

## Versioning

Keel is on v0. The exported API is frozen in `api/v0.1.0.txt`, and a check in CI fails on any
change to it. The records are frozen for `linux/amd64`. Once v1 lands, a breaking change will need
a major version.

## Development

```bash
./tools/check.sh
```

That runs the same ten checks CI runs: formatting, vet, staticcheck, a `CGO_ENABLED=0` build, the
race-enabled tests, two content scans, a dependency-boundary check that keeps SQLite out of the
core packages, a convention checker, and the frozen-API diff. It needs `ffmpeg` and `ffprobe` on
`PATH` for the media tests.

## License

Apache-2.0. See [LICENSE](LICENSE).
