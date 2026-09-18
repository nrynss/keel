# Keel

Go building blocks for a media app that sits on a public URL, calls paid APIs, and runs on one box.

Every product built that way needs the same machinery underneath it. A gate so the URL is not an
open wallet. Long work that outlives its request and reports progress. Media stored privately and
served with seeking. ffmpeg that dies with its job. A record of what everything cost. Settings in
one TOML file with secrets that resolve where they live. Keel is that machinery, so each product
stops rebuilding it.

Keel is a set of small packages, not a framework. Each one takes a `Config`, returns a concrete
type, and hands back `http.Handler` values that `net/http` serves. Nothing reads the environment,
nothing logs on its own, and nothing panics.

## Install

```bash
go get github.com/nrynss/keel@v0.3.0
```

Go 1.27 or newer. The `ffmpeg` package runs the `ffmpeg` and `ffprobe` binaries, so install those
if you use it. Pin ffmpeg 9.0.1: the media tests and CI assert durations exactly on that version,
and other versions may differ by milliseconds. For example, install the static build with
`docker create mwader/static-ffmpeg:9.0.1`, then `docker cp` both binaries out of it. Everything
else is pure Go, and `CGO_ENABLED=0` builds the whole module.

## Packages

| Package | Does |
|---|---|
| `wire` | The error envelope shared by gate, upload, and direct app routes, plus the event frames stream and job publish |
| `id` | Unguessable 128-bit ids in lowercase hex, for anything served in public |
| `gate` | Per-client and global token buckets, plus an optional passcode, in front of the routes that spend money |
| `stream` | Topic broker and server-sent events, with heartbeats and a non-blocking slow-subscriber policy |
| `job` | Long work started by a short request, observed over `stream`, durable across restarts |
| `mediastore` | Blobs on disk under unguessable ids, served with Range support, with retention sweeps |
| `upload` | Resumable chunked uploads that land in `mediastore`, resumable by id after a dropped connection |
| `sqlite` | One SQLite file with WAL, a writer handle, a read-only reader pool, namespaced migrations and online backup |
| `ffmpeg` | ffmpeg and ffprobe bound to a context, with a bounded wait on shutdown |
| `cost` | Money as integer nanodollars, a ledger of charges, budgets that refuse before a call, keyed budgets that divide one pool by owner, and a meter that runs one paid call |
| `flag` | Runtime flags an operator flips without a restart, read through to the store with a declared default for a missing row |
| `caption` | Word timings to SRT and WebVTT subtitle files as a pure function, with cues grouped by line length and duration |
| `erase` | A delete that finishes, fanned out over consumer targets with per-target progress, restart-safe resume, and a stuck report that names what is owed |
| `edl` | A cut list rendered into one audio file, with merged ranges, crossfades or cuts at the joins, and two-pass loudness normalisation |
| `waveform` | A still plus audio rendered to one video, with the waveform drawn over the scaled and padded still |
| `lease` | Paid sessions with a time cap, a quota, a caller supplied kill switch, settle at close, provider reconciliation, and reclaim of abandoned rows |
| `config` | TOML settings, and secret references that resolve from the environment, a file, a directory or a command, never holding a value in the file |
| `config/source` | The five built-in secret sources that a `config.Registry` carries |

The stores that need SQLite live one directory down, in `job/sqlitestore`,
`mediastore/sqlitestore`, `cost/sqlitestore`, `flag/sqlitestore` and
`lease/sqlitestore`. Only those packages import a SQLite driver, so an
app that uses `gate` alone never compiles one. The TOML parser stays the same way behind `config`
and `config/source`, so an app that uses `gate` alone never compiles it either.

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
kind that is safe to repeat opts in. A replacement attempt inherits the interrupted record's
progress snapshot, so a resume hook rebuilds from the last report even when the replacement dies
before its first one or waits queued at a saturated kind.

The browser follows the job over server-sent events:

```go
mux.HandleFunc("GET /jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
	broker.ServeTopic(w, r, job.Topic(r.PathValue("id")))
})
```

Subscribe first, then read the job's state. The broker retains the terminal event for the bound in `stream.Config.Retain`, one minute at defaults,
so a subscriber that joins within retention still learns the outcome.

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

```go
handler, err := upload.New(upload.Config{Dir: "staging", Store: store})
mux := http.NewServeMux()
handler.Mount(mux)
```

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
A `cost.KeyedBudget` divides one pool by owner under a global ceiling, so one owner cannot spend
another owner's headroom. Each owner reserves and settles through its own account from `Owner`.
A `cost.Meter` runs one paid call against an account: it reserves the estimate, runs the work,
settles the measured price, and frees the reservation on every failure path. A call that reports
no usage settles at its estimate and says so in the returned `Usage`.

### Flip a switch without a restart

```go
paid := flag.Bool{Name: "paid_calls", Default: true, Help: "switch paid calls on"}
on, _, err := store.Bool(ctx, paid)
if _, err := store.SetBool(ctx, paid, false); err != nil {
	return err // the operator flipped it, no deploy involved
}
```

`flag` declares one boolean or small text value with the time it last changed. Every read goes
through to the store, because a check is one indexed row and a cache adds an invalidation nobody
tests. A name with no stored value reads as its declared default, so a missing row is never a
silent off. `flag/sqlitestore` owns its namespaced migration, like every other store here.

### Render a transcript as subtitles

```go
cues := caption.Group(words, caption.Config{LineLength: 40, MaxDuration: 3 * time.Second})
if err := caption.SRT(w, cues); err != nil {
	return err
}
```

`caption` turns word timings into SRT and WebVTT as a pure function that writes to an
`io.Writer`. `Group` joins words into cues under a line length and a duration cap, and it never
splits a word. One cue list renders to either format. Times format exactly with no floating point
drift, so a cue at one hour reads `01:00:00,000` in SRT and `01:00:00.000` in WebVTT.

### Delete until every target confirms

```go
eraser, err := erase.New(source, erase.Config{})
jobID, err := eraser.Start(ctx, runner, ref, targets)
report, err := eraser.Inspect(ctx, runner, jobID)
```

`erase` fans a delete out over targets the consumer registers. The library owns the fan-out, the
retry, and the record of which targets confirmed. It never owns the list, so a `Source` rebuilds
the list from the ref when a run resumes. Progress is per target, so a resumed run retries only
what has not confirmed. A target that reports already gone counts as confirmed, because deleting
repeats safely. A target that fails forever leaves a stuck erasure, and `Report` names every
target still owed a delete. The work runs as a `job` kind, so cancellation, limits and resumption
come from there.

### Render a cut list into one audio file

```go
err := edl.Render(ctx, tools, src, edl.Config{Crossfade: 2 * time.Second}, dst,
	edl.Segment{Start: time.Second, End: 10 * time.Second})
```

`edl` cuts the kept ranges out of one source file and writes one normalised audio file. Ranges
that touch or overlap merge first, so no zero length range reaches the filter graph. A join
carries the configured crossfade when both neighbours are long enough, and a hard cut otherwise.
Loudness normalises in two passes: the first measures through the loudnorm filter, and the second
applies the numbers the tool itself wrote. Pin ffmpeg 9.0.1 for this package, as the Install
section says: the duration and loudness pins assert exactly on that version. Every invocation
carries the caller's context, so a cancelled render stops the child.

### Render a still plus audio to video

```go
err := waveform.Render(ctx, tools, waveform.Config{}, "still.png", "tone.wav", "out.mkv")
```

`waveform` renders one still and one audio file into a video with the waveform drawn over the
still. The default frame is 1920 by 1080. The still scales to fit and pads out to the exact
frame, so another aspect ratio keeps its shape rather than stretching. The audio stream copies
into the output and never passes an encoder. Pin ffmpeg 9.0.1 for this package, as the Install
section says. Every render carries the caller context, so a cancelled render stops the child.

### Bound a paid session with a lease

```go
manager, err := lease.New(lease.Config{Quota: quota, Meter: meter, Store: store, Cap: time.Hour})
opened, err := manager.Open(ctx, "team-a", cost.USD(0.10), "")
closed, err := manager.Close(ctx, opened.ID, cost.USD(0.12))
reconciled, err := manager.Reconcile(ctx, opened.ID, cost.USD(0.15))
```

`lease` runs paid sessions that last minutes rather than one call. `Open` counts the lease
against a quota and against a budget through the paid call seam, and it carries a time cap that
expires on its own as a row comparison against the stored deadline. A caller supplied reason
refuses new leases at once, which pairs with `flag` without importing it. `Close` settles the
reported price through the same seam. `Reconcile` applies the provider reported price as the
truth and keeps both numbers on the record. A dead process leaves its lease behind, and a later
call reclaims the slot once the cap has passed. `lease/sqlitestore` owns its namespaced
migration, like every other store here.

### Configure with a file that holds no secret value

Settings sit inline in one TOML file. A secret is a reference that names its source and its
locator. A value never goes in the file, so the file stays safe to keep in a repository.

```toml
[render]
model = "some-model-id"
max_seconds = 1200

[secrets.provider_api_key]
source = "env_file"
path = "/etc/app/env"
var = "PROVIDER_API_KEY"
read = "at_use"
```

Each source reads the locator keys it needs:

| Source | Locator keys | Reads |
|---|---|---|
| `env` | `var` | One exported variable |
| `env_file` | `path`, `var` | One entry in a root-owned env file |
| `file` | `path` | One file whose whole content is the value |
| `dir` | `path`, `name` | One entry in a credential directory |
| `command` | `command`, `args` | One program's standard output |

Load the file through the shipped sources, then read each secret with a deliberate call:

```go
var settings appSettings
reg := config.NewRegistry()
if err := source.RegisterDefaults(reg, source.Config{}); err != nil {
	return err
}
plan, err := config.Load(ctx, &settings, config.Config{Path: "app.toml", Registry: reg})
if err != nil {
	return err
}
key, err := settings.Secrets.ProviderAPIKey.Reveal()
if err != nil {
	return err
}
fmt.Println(plan)
```

`Load` fills defaults first, then the file, then environment overrides for plain settings, then
explicit flags. `Reveal` returns the value for one secret. `read = "at_boot"` resolves once at
load, and `read = "at_use"` resolves on each call so a rotated key takes effect with no restart.
The plan prints one line per key with its source and locator. No line carries a value. A secrets
file that group or other can read stops the load and names its path and mode.

### One error shape, everywhere

```go
wire.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests", detail)
```

```json
{"error":{"code":"rate_limited","message":"too many requests","detail":{"retry_after_seconds":3}}}
```

Clients branch on `code`, never on `message`. The same package writes and reads the event frames
that `stream` and `job` publish, so a browser client and the server agree on one shape.

## Endpoints

Every route below is an `http.Handler` value the app mounts on its own mux.
Gate refusals, upload failures, job terminal embeds, and direct app calls share the `wire` envelope, and clients branch on its `code`.
Mediastore answers with plain `http.NotFound` and `http.Error` bodies for 404 unknown or refused, 405, and 500, never the envelope.
ServeTopic writes SSE frames, not envelope JSON.

### Guarded routes with gate

Entry points: `gate.New`, `Gate.Protect`, `Gate.ProtectFunc`.

`Protect` wraps any handler with one rule. It carries no method or path of its own. The app names
the rule and mounts the wrapped handler where the route lives. A request passes when it carries
the shared passcode and both buckets hold a token.

```go
guarded, err := g.Protect(gate.Rule{
	Name:      "render",
	PerClient: gate.Limit{Burst: 3, Every: time.Hour},
	Global:    gate.Limit{Burst: 100, Every: time.Hour},
}, renderHandler)
mux.Handle("POST /api/render", guarded)
```

Refusals answer with the `wire` envelope and take no token from either bucket. A missing or wrong
passcode answers 403 with code `passcode_required`. The passcode arrives in the `X-Passcode`
header or the `passcode` cookie when the config leaves the names at defaults. An empty bucket
answers 429 with code `rate_limited` and a `retry_after_seconds` detail, mirrored in the
`Retry-After` header.

### Job progress over server-sent events

Entry points: `stream.New`, `Broker.Subscribe`, `Broker.Publish`, `Broker.ServeTopic`,
`job.Topic`, `job.Open`, `Runner.Start`, `Runner.StartKind`, `Runner.Result`, `Runner.Cancel`,
`wire.SetEventHeaders`, `wire.WriteEvent`, `wire.ReadEvent`.

The app exposes one stream route per job. The app chooses the path below as its convention, and it matches the stream example.
The app mounts it wherever its jobs live:

```go
mux.HandleFunc("GET /jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
	broker.ServeTopic(w, r, job.Topic(r.PathValue("id")))
})
```

`GET` opens a `text/event-stream` response with `Cache-Control: no-store`. `ServeTopic`
subscribes to the job topic and writes one frame per published event until the client goes away
or the subscription ends. ServeTopic writes SSE frames, not envelope JSON. A quiet connection
sends a `: ping` comment on each heartbeat so a proxy keeps it open. The request has no body and
no auth of its own. Wrap the route with `Protect` when the stream should draw from the same rate
limit buckets as the work.

Event names travel in each frame: `progress` while work runs, then one terminal of `done`,
`error`, `cancelled`, or `interrupted`. Payload shapes share the job id:

```json
{"job_id":"abc","stage":"encode","current":2,"total":5}
{"job_id":"abc","status":"done"}
{"job_id":"abc","status":"error","error":{"error":{"code":"job_failed","message":"encode failed"}}}
```

An `error` terminal embeds the same envelope body a failed response carries, so one parser reads
both. The broker retains the terminal event for the bound in `stream.Config.Retain`, one minute at defaults, so a subscriber that joins within retention still receives it. A subscriber that joins after expiry gets live events only. The client
subscribes first, then calls `Runner.Result` for the stored state, then drops a duplicate on the
job id. A `Result` read for an unknown id fails with `job: unknown id` rather than a frame.

### Private and public media with mediastore

Entry points: `mediastore.Open`, `Store.Persist`, `Store.PersistWithID`, `Store.Delete`,
`Store.ServeHTTP`, `Store.NewSweeper`.

`Store` is the handler. The app registers it at one route:

```go
mux.Handle("GET /media/{id}", store)
```

`GET` and `HEAD` serve one blob. Other methods answer 405 with `Allow: GET, HEAD`. The response
carries the stored content type verbatim and an `ETag` built from the blob id. `http.ServeContent`
does the range work, so a `Range` request answers 206 with the requested bytes and a matching
`If-None-Match` answers 304. `HEAD` sends headers and no body.

A public blob answers with `Cache-Control: public, max-age=31536000, immutable` because its id
names its bytes for good. A private blob answers with `Cache-Control: private, no-store` and only
when `Config.Authorize` allows the live request. A refusal answers 404, the same shape as an
unknown or malformed id, so the response never confirms a private blob exists. A metadata row
whose file has vanished also answers 404 while the server logs the fault. A lookup that fails for
another reason answers 500. These answers use plain `http.NotFound` and `http.Error` bodies, never the `wire` envelope.

### Resumable uploads with upload

Entry points: `upload.New`, `Handler.Mount`, `Handler.ServeHTTP`, `Handler.Start`,
`Handler.Sweep`, `Handler.Close`.

`Mount` registers the handler on a mux under its base path, `/uploads` by default. All four
routes share the `wire` envelope on failure, and clients branch on its code. The JSON shapes stay small:
state answers echo the upload, and completion answers with the stored id and digest.

- `POST {base}` opens an upload. The request body is JSON with `owner`, `content_type`, optional
`group`, optional `visibility` of `private` or `public`, optional `size_bytes`, and optional
`chunk_size`. It answers 201 with the upload state: `id`, `owner`, `content_type`,
`visibility`, `chunk_size`, `stored_bytes`, `received`, `missing`, and `expires_at`. The state adds `size_bytes` and `chunk_count` when the open declared a size. A bad field
answers 400 with code `invalid_request` and the field name. An upload or owner over its byte limit
answers 413 with code `limit_exceeded` and the limit name.
- `PUT {base}/{id}/chunks/{n}` stores chunk `n`. The chunk bytes are the body and
`X-Chunk-SHA256` carries the hex digest of those bytes. Chunks may arrive in any order. A repeat
of a stored index with the same digest is accepted. It answers 200 with the upload state. A
missing digest, a bad index, an oversize or empty body, or a body past the declared length answers
400 with code `invalid_request`. A digest that does not match the bytes answers 409 with code
`chunk_mismatch` and both digests. An unknown or expired id answers 404 with code `not_found`.
A chunk past either byte limit answers 413 with code `limit_exceeded`.
- `GET {base}/{id}` reports the upload state with a 200 and the same body an open returns. An
unknown or expired id answers 404 with code `not_found`.
- `POST {base}/{id}/complete` assembles the chunks in order, checks the whole-file digest, and
stores the bytes under the upload id. The request body is JSON with `sha256`. It answers 201 with
`id`, `owner`, `content_type`, `visibility`, `size_bytes`, and `sha256`. Missing chunks answer
409 with code `incomplete` and the missing indices. A digest that does not match the assembled
bytes answers 409 with code `hash_mismatch` and both digests. A content type the store refuses
answers 415 with code `unsupported_type`.

A wrong method on a known route answers 405 with code `invalid_request` and names the allowed method. Clients must branch on status plus code for method errors. An unknown path under
the base answers 404 with code `not_found`.

### The shared error envelope with wire

Entry points: `wire.WriteError`, `wire.WriteEvent`, `wire.WriteHeartbeat`,
`wire.SetEventHeaders`, `wire.ReadEvent`, `wire.NewErrorEvent`.

`WriteError` writes every non-2xx JSON response in the same shape:

```json
{"error":{"code":"rate_limited","message":"too many requests","detail":{"retry_after_seconds":3}}}
```

It sets `Content-Type: application/json` and `Cache-Control: no-store` on every response. A 429
also sets `Retry-After` from the `retry_after_seconds` member of the detail, rounded up with a
floor of 1, so header and body never disagree. `gate` refusals, `upload` failures, job terminal embeds, and any app
route that calls it directly all share this body.

## Examples

Every package carries a runnable example that shows its common case.

```bash
go test -run Example -v ./...
```

## Versioning

Keel is on v0. The exported API is frozen in `api/v0.3.0.txt` with its `api/v0.3.0.export`
baseline, and a check in CI fails on any change to them. The records are frozen for
`linux/amd64`. They cover the surface the release shipped, including `flag`, `caption`,
`erase`, `edl`, `waveform`, `lease`, and the `cost` keyed budgets with the paid call seam.
Once v1 lands, a breaking change will need a major version.

## Development

```bash
./tools/check.sh
```

That runs the same eleven checks CI runs: formatting, vet, staticcheck, a `CGO_ENABLED=0` build,
the race-enabled tests, two content scans, two dependency-boundary checks that keep SQLite and the
TOML parser inside their packages, a convention checker, and the frozen-API diff. It needs
`ffmpeg` and `ffprobe` on `PATH` for the media tests.

## License

Apache-2.0. See [LICENSE](LICENSE).
