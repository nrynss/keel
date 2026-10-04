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
go get github.com/nrynss/keel@v0.4.0
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
| `mediastore` | Blobs on disk under unguessable ids, served with Range support, with retention sweeps and a snapshot that restores the same ids |
| `upload` | Resumable chunked uploads that land in `mediastore`, resumable by id after a dropped connection |
| `sqlite` | One SQLite file with WAL, a writer handle, a read-only reader pool, namespaced migrations and online backup |
| `ffmpeg` | ffmpeg and ffprobe bound to a context, with a bounded wait on shutdown |
| `duration` | WAV and MP3 length read from the bytes themselves, for a runtime that ships ffmpeg without ffprobe |
| `cost` | Money as integer nanodollars, a ledger of charges, budgets that refuse before a call, keyed budgets that divide one pool by owner, and a meter that runs one paid call |
| `throttle` | Retry of a paid call that failed for a reason a wait can clear, with full-jitter backoff and an optional cap on how many calls run at once |
| `flag` | Runtime flags an operator flips without a restart, read through to the store with a declared default for a missing row |
| `caption` | Word timings to SRT and WebVTT subtitle files as a pure function, with cues grouped by line length and duration |
| `film` | A title card, captioned stills, and an end card joined by a stream copy, with the length computed in Go |
| `erase` | A delete that finishes, fanned out over consumer targets with per-target progress, restart-safe resume, and a stuck report that names what is owed |
| `edl` | A cut list rendered into one audio file, with merged ranges, crossfades or cuts at the joins, and two-pass loudness normalisation |
| `bed` | A looping music bed mixed under a finished video, with gain and an end fade anchored to a known duration |
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

`Snapshot` writes `manifest.json` plus one file per blob, into a sibling directory that replaces `dir` only when the manifest is complete. A failed refresh leaves the previous snapshot in place. `Selection` is either an id list or an exact owner match, and a selection that matches nothing is refused. `Restore` recreates those blobs with the same ids, content types and visibility, stamps them with the store clock rather than the captured creation time, and refuses a hash mismatch or an id that is already stored. A restored row is an ordinary row. Visibility is not a retention pin. `Protected` and `Retain` are.

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
A store opened with `Period: cost.DailyUTC` restarts its ceiling every UTC day, and `MonthlyUTC`
every month, so a limit bounds one window rather than all spend ever booked. A `cost.KeyedBudget` divides one pool by owner under a global ceiling, so one owner cannot spend
another owner's headroom. Each owner reserves and settles through its own account from `Owner`.
A `cost.Meter` runs one paid call against an account: it reserves the estimate, runs the work,
settles the measured price, and frees the reservation on every failure path. A call that reports
no usage settles at its estimate and says so in the returned `Usage`. `KeyedBudget.ForgetOwner`
removes an owner's budget, reservations and settle history after its live reservations finish.

### Retry a paid call that a wait can clear

```go
err := throttle.Retry(ctx, throttle.Config{}, func(err error) bool {
    return errors.Is(err, errRateLimited)
}, func() error {
    return provider.Speak(ctx, page)
})
```

`throttle` retries only the errors the caller classifies as worth a wait. The
wait is full jitter across the upper half of each backoff, starting at 20
seconds and doubling up to 90, so a fan-out refused together does not retry in
lockstep. The call's own error comes back untouched. A cancelled context ends
the wait and still returns that error. `throttle.New` caps how many calls run
at once. The cap is held only while a call runs, and the backoff waits outside
it. `cost.Meter` still reserves and settles. This package only paces the call.
`throttle.Note` is how a caller marks an error that used up the attempts,
with the same config the call used. One spent attempt reads "1 attempt".


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

### Mix a music bed under a finished video

```go
err := bed.Mix(ctx, tools, bed.Config{}, bed.Input{
    Film: "film.mp4", Bed: "bed.mp3", Duration: filmLength, Output: "mixed.mp4",
})
```

`bed` loops or trims a music file under a finished video. The fade starts
`Duration` minus the fade, two seconds by default, and reaches silence at
that known end. Film audio that stops early is padded with silence to
`Duration`, so the fade is not cut off at the audio stream. The video stream
is copied. The bed is gained on its own chain, and the mix does not
normalise, so the film's audio stays at its own level. `Duration` is the
length the caller already computed. The mix does not probe the file.


### Render a still plus audio to video

```go
err := waveform.Render(ctx, tools, waveform.Config{}, "still.png", "tone.wav", "out.mkv")
```

`waveform` renders one still and one audio file into a video with the waveform drawn over the
still. The default frame is 1920 by 1080. The still scales to fit and pads out to the exact
frame, so another aspect ratio keeps its shape rather than stretching. The audio stream copies
into the output and never passes an encoder. Pin ffmpeg 9.0.1 for this package, as the Install
section says. Every render carries the caller context, so a cancelled render stops the child.

### Read a clip's length without ffprobe

```go
d, err := duration.Read(clip)
```

`duration` reads a WAV from its fmt and data chunks, and an MP3 from its
frame headers. A Xing or Info count is trimmed by the encoder delay and tail
padding when a LAME, Lavf, or Lavc tag carries them, which is the playable
length. `ffmpeg.Duration` stays the measurement when ffprobe is installed.

### Assemble a captioned still film

```go
length, err := film.Render(ctx, tools, film.Config{
    FontFile: "Face.ttf",
    EndTitle: "The end",
}, film.Input{
    Title: "A story",
    Output: "film.mp4",
    Pages: []film.Page{{
        ImagePath: "page.png",
        Text:      "The gate opens.",
        AudioPath: "page.mp3",
        Duration:  clip,
    }},
})
```

`film` builds a title card, one segment per page, and an end card, then joins
them with a stream copy. Every segment is 1080 by 1620. The page caption is
wrapped in Go and burned in from a text file, which is separate from the
word-timed cues in `caption`. A narrated page is held for the duration the
caller measured. A silent page is held for `film.SilentHold`. `Render` returns
that sum and does not probe the file. `film.Total` is the same sum without
rendering, which is what a later mix anchors a fade to.

`film.Page.N` is only the number named in an error. Image and narration bytes
are staged by the page's position, so two pages may share an N.

The caller is trusted. `Render` uses the ffmpeg binary and the font, image,
audio, work, and output paths it is given. It checks that a path exists, and
it does not authorize it or confine the process. Quoting a path keeps the
filter syntax intact. It is not an access check. `Concat` passes the caller's
segment paths through with `-safe 0`. An integration that accepts untrusted
jobs keeps the binary and those paths under its own policy, and runs ffmpeg
where the filesystem and the network are already limited.

The finished file replaces `Output` by a rename. A copy onto another
filesystem is written beside `Output` and renamed only when the copy and its
sync have succeeded, so a failed or cancelled publish leaves the previous
file in place. Renders that name the same output are not queued. The last
successful rename is the file a reader sees.


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
migration, like every other store here. `lease/sqlitestore.Store.ForgetOwner` removes an
owner's closed and expired leases after its live leases finish.

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
`Store.Delete` removes a finished job and its whole attempt chain. `Store.PruneFinished` removes
terminal chains older than a supplied time and leaves active chains alone.

### Private and public media with mediastore

Entry points: `mediastore.Open`, `Store.Persist`, `Store.PersistWithID`, `Store.Delete`,
`Store.ServeHTTP`, `Store.NewSweeper`, `Store.Snapshot`, `Store.Restore`.

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

Keel is on v0. Each release freezes the exported API in a pair of records under `api/`.
`api/vX.Y.Z.txt` is a readable `go doc -all` transcript, and `api/vX.Y.Z.export` is the binary
baseline apidiff compares against. Both are taken on `linux/amd64`. Between releases the records
do not move, and a check fails on any change to them. Ordinary work never touches `api/`, so
new packages and new exports join a record at the next release.

A release is the only writer. It adds exactly one pair for its version, and that version must
be strictly newer than every record already there. `tools/freeze.sh vX.Y.Z` writes the pair,
stages it, and runs the gate. A patch release may only add to the surface. While the module is
on v0, a minor release may break it. Once v1 lands, a breaking change needs a major version.

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
