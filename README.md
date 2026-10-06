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
go get github.com/nrynss/keel@v0.5.0
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
| `stream` | Topic broker and server-sent events, with heartbeats, a non-blocking slow-subscriber policy, and best-effort replay after Last-Event-ID |
| `job` | Long work started by a short request, observed over `stream`, durable across restarts |
| `mediastore` | Blobs on disk under unguessable ids, served with Range support, with retention sweeps and a snapshot that restores the same ids |
| `upload` | Resumable chunked uploads that land in `mediastore`, resumable by id after a dropped connection |
| `photo` | Normalises an uploaded photo: EXIF orientation applied to the pixels, metadata stripped, resized under side and pixel caps, re-encoded under a byte cap |
| `sqlite` | One SQLite file with WAL and synchronous FULL, so an acknowledged write survives a power cut once the file's creation window has passed, plus a writer handle, a read-only reader pool, namespaced migrations and online backup |
| `ffmpeg` | ffmpeg and ffprobe bound to a context, with a bounded wait on shutdown |
| `duration` | WAV and MP3 length read from the bytes themselves, for a runtime that ships ffmpeg without ffprobe |
| `cost` | Prices in the minor units of one denomination, USD nanodollars by default or a provider's credit, a ledger of charges that name their unit, budgets that refuse before a call, keyed budgets that divide one pool by owner, grants that lapse, and a meter that records each settle in an app-chosen charge sink |
| `cache` | Content-addressed reuse of paid generations, keyed on a canonical hash of the request, with one make per key across concurrent callers, age expiry, and optional caching of permanent refusals |
| `throttle` | Retry of a paid call that failed for a reason a wait can clear, with full-jitter backoff and an optional cap on how many calls run at once |
| `flag` | Runtime flags an operator flips without a restart, read through to the store with a declared default for a missing row |
| `caption` | Word timings to SRT and WebVTT subtitle files as a pure function, with cues grouped by line length and duration |
| `film` | A title card, captioned stills, and an end card joined by a stream copy, with the length computed in Go |
| `book` | An illustrated PDF of image pages and captions, with the font, margins and page size injected |
| `erase` | A delete that finishes, fanned out over consumer targets with per-target progress, restart-safe resume, and a stuck report that names what is owed |
| `edl` | A cut list rendered into one audio file, with merged ranges, crossfades or cuts at the joins, and two-pass loudness normalisation |
| `bed` | A looping music bed mixed under a finished video, with gain and an end fade anchored to a known duration |
| `waveform` | A still plus audio rendered to one video, with the waveform drawn over the scaled and padded still |
| `lease` | Paid sessions with a time cap, a quota, a caller supplied kill switch, settle at close, provider reconciliation, and reclaim of abandoned rows |
| `config` | TOML settings, and secret references that resolve from the environment, a file, a directory or a command, never holding a value in the file |
| `config/source` | The five built-in secret sources that a `config.Registry` carries |
| `fetch` | A user-supplied link fetched under an address policy enforced at dial time, with scheme, port, redirect, size, time and content type caps, and preview metadata read from the page |
| `outbox` | Events written durably on this box first, replayed in insertion order to an app-supplied sink, with spent attempts counted and reported |
| `identity` | Guest sessions resolved from a signed cookie or a bearer token, sign-in by emailed code or an external provider, guest upgrade with a conflict rule, and account deletion as one resumable job |

The stores that need SQLite live one directory down, in `cache/sqlitestore`,
`job/sqlitestore`, `mediastore/sqlitestore`, `cost/sqlitestore`,
`flag/sqlitestore`, `lease/sqlitestore`, `outbox/sqlitestore` and
`identity/sqlitestore`. Only those
packages import a SQLite driver, so an app that uses `gate` alone never
compiles one. The TOML parser stays the same way behind `config` and
`config/source`, so an app that uses `gate` alone never compiles it either.
The PDF library stays behind `book` the same way.

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
so a subscriber that joins within retention still learns the outcome. A reconnect sends `Last-Event-ID`. When that id is still in the topic's ring (`stream.Config.Replay`, 512 events by default), `ServeTopic` writes every later frame with its original id before following the live subscription. Event ids increase for the life of the broker, so a topic that is dropped and created again does not reuse them, and an old cursor cannot select the new ring's tail. A cursor the process has forgotten, a missing header, or `0` is a fresh subscribe: no replay and no error. A cursor equal to the retained terminal ends the response with nothing further to send. Ping comments are not replayed. The ring is dropped with the topic, so a miss still falls back to the job's stored state.

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

`Snapshot` writes `manifest.json` plus one file per blob, into a sibling directory that replaces `dir` only when the manifest is complete. `dir` is cleaned first, so a trailing separator does not leave a stray directory. `.` is refused, and so is the store's own blob directory. An existing path is replaced only when it is an empty directory or already contains `manifest.json`. A regular file, or a directory of unrelated files, is left untouched. The committed directory is always mode 0755, even when the previous snapshot was tighter. That is the portable-fixture contract. Private blob bytes in the fixture are readable to any local user who can traverse its parents, so a confidential snapshot belongs under a directory those users cannot traverse. A failed refresh leaves the previous snapshot in place. `Selection` is either an id list or an exact owner match. It is not an owner prefix, and a selection that matches nothing is refused. `Restore` recreates those blobs with the same ids, content types and visibility, stamps them with the store clock rather than the captured creation time, and refuses a hash mismatch or an id that is already stored. Cleanup after a failed restore deletes by id and does not notice another caller that deleted one of those ids and created it again before the call returns. A restored row is an ordinary row. Visibility is copied, and it is not a retention pin. `Protected` and `Retain` are. `Put.CreatedAt` lets a caller stamp a creation time. Restore leaves it zero on purpose, so an old fixture is not swept on arrival.

### Accept a recording that survives a dropped connection

`upload` takes a file in chunks, each with its own hash, in any order. A repeat of the same chunk
is accepted. A chunk whose hash differs is refused. Completion assembles the chunks, verifies the
whole file, and persists it into `mediastore` under an id the client already knows.

```go
handler, err := upload.New(upload.Config{Dir: "staging", Store: store})
mux := http.NewServeMux()
handler.Mount(mux)
```

### Normalise an uploaded photo

```go
res, err := photo.Normalize(ctx, r, photo.Config{
	MaxLongSide:   1024,
	MaxBytes:      10 << 20,
	Format:        photo.JPEG,
	Quality:       85,
	MaxPixels:     100_000_000,
	MaxInputBytes: 32 << 20,
})
```

`photo` takes one uploaded image and returns the bytes a paid model or a store can accept. It
decodes JPEG, PNG and WebP, applies the EXIF orientation to the pixels, resizes under the side
limits without ever enlarging, and re-encodes from the decoded pixels. The re-encode is what
strips metadata, so no EXIF, XMP or ICC block from the input survives. The decode set is the
package's own, so a binary that links other image decoders does not widen what an upload can
be. The result carries a SHA-256 of the normalised bytes, so the same pixels uploaded twice
hash to one cache key, whatever metadata each upload carried.

Refusals are deterministic. Each is a `photo.Error` whose `Code` maps into the `wire` envelope,
with a sentinel behind it for `errors.Is`. The codes are `unsupported_format`, `heic`,
`too_many_input_bytes`, `too_many_pixels` and `output_cannot_fit`. The HEIC refusal sniffs the
ISO BMFF brands before the rest of a large upload is read. The pixel cap reads the header alone
before any full decode, which is what bounds a decompression bomb. When a byte cap is set
and the output is over it, the JPEG quality steps down to a floor, then the scale steps down, and
the last floor refuses. Orientation is read only from a JPEG APP1 Exif segment, so a PNG or WebP
input is trusted to be upright.

### Count what it cost, and refuse before you overspend

```go
budget, err := cost.NewBudget(cost.USD(150))
if err := budget.Reserve(cost.USD(4.50)); err != nil {
	return err // over budget, before the paid call
}
// make the paid call, then record what it really cost
budget.Settle(cost.USD(4.50), actual)
```

`cost.Price` counts the minor units of one denomination in an `int64`, so no rounding creeps in.
The zero denomination is USD nanodollars, which is what every budget and charge counted before
denominations existed. `cost.NewBudgetIn` and `cost.NewKeyedBudgetIn` bound a pool of a
provider's credit instead, and every budget and account reports its unit through `Denomination`.
A `cost.Meter` stamps every charge it records with its account's denomination, so a report never
shows a credit pool as dollars. A total that would sum charges naming different denominations
refuses with `cost.ErrMixedDenomination`, and no path converts one unit into another.
`cost/sqlitestore` refuses a charge that names a unit its store does not speak with
`cost.ErrDenominationMismatch`, and refuses to reopen a file under another denomination. A
reopen under another name would silently reprice every recorded amount. `cost.Conversion`
prices a credit in nanodollars for reports alone. No budget, meter or store accepts one, so a
conversion can never move a spending decision.

`cost/sqlitestore` keeps
the ledger and the reservations across a restart, and a reservation that expires releases itself.
A store opened with `Period: cost.DailyUTC` restarts its ceiling every UTC day, and `MonthlyUTC`
every month, so a limit bounds one window rather than all spend ever booked. `Store.Grant` funds
the pool with credit that lapses. The balance is the unexpired grants minus what spend has
drawn. Spend draws from the grant that expires soonest first, and expiry is judged at read and
spend time, so a restart cannot resurrect lapsed credit. An allowance that resets every window
posts one grant per window, keyed by the window's start, which `cost.Period.Start` computes. A
repeated key answers `sqlitestore.ErrGrantRepeated` and changes nothing, so a restart or a
double post cannot fund a window twice. Once a pool holds a grant, that balance bounds
reservations beside the ceiling. A `cost.KeyedBudget` divides one pool by
owner under a global ceiling, so one owner cannot spend another owner's headroom. It divides a
granted pool by owner exactly as it divides dollars. Each owner reserves and settles through its own account from `Owner`.
A `cost.Meter` runs one paid call against an account: it reserves the estimate, runs the work,
and settles the measured price. It frees the reservation on every failure up to the settle.
A call that reports no usage settles at its estimate and says so in the returned `Usage`. The
settle lands in a `cost.ChargeSink`. The in-memory `cost.Ledger` is one sink, and the
`cost/sqlitestore` store is another. An application with its own spend store writes the
one-method sink itself. A charge the sink cannot record is the one failure past the settle.
`Meter.Call` reports it as `cost.ErrUnrecordedCharge`, with a zero `Usage`, so a figure no
store backs is never shown as booked. `KeyedBudget.ForgetOwner`
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

### Reuse a paid generation

```go
type speechKey struct {
    Provider string
    Model    string
    Voice    string
    Line     string
    InputSHA string
}

key, err := cache.Key(speechKey{Provider: "speech", Model: m, Voice: v, Line: line, InputSHA: norm.SHA256})
if err != nil {
    return err
}

c, err := cache.New(cache.Config{
    Store:      entryIndex,
    MaxAge:     7 * 24 * time.Hour,
    RefusalTTL: time.Hour,
    Classifier: func(err error) bool { return errors.Is(err, speech.ErrNoFace) },
})

res, err := c.GetOrMake(ctx, key, func(ctx context.Context) (cache.Result, error) {
    out, err := speech.Synthesize(ctx, req)
    if err != nil {
        return cache.Result{}, err
    }
    blobID, err := media.Persist(ctx, bytes.NewReader(out.Bytes), mediastore.Put{
        ContentType: "audio/ogg",
        Group:       "cache-speech",
    })
    if err != nil {
        return cache.Result{}, err
    }
    doc, err := json.Marshal(cachedSpeech{Blob: blobID, ContentType: "audio/ogg"})
    if err != nil {
        return cache.Result{}, err
    }
    return cache.Result{Payload: doc, ContentType: "application/json", ChargeRef: chargeRef, BlobID: blobID}, nil
})
```

`cache.Key` hashes a caller struct as canonical JSON, so a key is one struct
away. Struct fields keep declaration order and map keys are sorted, so the
same request hashes to the same key in every process. `photo.Normalize`
returns the input digest in `Result.SHA256`, so the same pixels hash to one
key whatever metadata each upload carried.

`GetOrMake` returns a stored entry, or runs the maker once per key across
concurrent callers. The callers that arrive while the maker runs wait for it
and receive its result, so a double click pays once. Expiry is judged inside
that flight, so callers who arrive together after expiry still trigger one
make. A store read that fails for a reason other than a missing entry is
returned, and no make runs, because a read nothing vouches for is not a
miss.

The payload is bytes plus a content type, so a structured result is as
natural as a media one. A JSON verdict stores its own bytes. A media maker
persists the bytes into a `mediastore` group first, because a provider
result URL expires, and stores a small JSON document naming the blob. The
maker must return bytes that outlive the call.

A failed make is never cached. A failure the classifier marks permanent for
its key is stored as a refusal and served for `RefusalTTL`, then it reads as
a miss again. The package never decides permanence itself. Entries expire by
age, and an expired entry behaves exactly like a miss.

Eviction rides the mediastore group that holds the blobs. The entry carries
the blob id its payload names, and `entryIndex.Sweep` deletes the rows whose
blob a retention sweep dropped. Run that sweep beside the mediastore
sweeper, with `Present` wired to the index, and rows past their age go in
the same pass. Multiple cache levels are multiple `cache.Cache` instances
with their own Config and key structs. Give each level its own key structs
or its own store namespace, which owns its own table.

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

### Sign guests in and delete accounts

```go
store, err := identitysql.Open(ctx, identitysql.Config{DB: db})
svc, err := identity.New(identity.Config{
	Store:      store,
	SigningKey: key,
	CodeKey:    codeKey,
	Mail:       sender,
	Eraser:     eraser,
	Targets:    targetsForUser,
})

handler := svc.Middleware(app) // mints a guest on a first visit
runner, err := job.Open(ctx, job.Config{Broker: broker, Store: jobStore, Kinds: svc.Kinds()})
err = svc.BindRunner(runner)
```

A first visit creates a guest user row and a session row. The browser holds a signed cookie that
carries only the session id, and a native client sends the same signed value as
`Authorization: Bearer`. Every request resolves the token against the rows, so `Revoke` takes
effect on the next request. The middleware puts the user into the request context, and `Owns`
checks it against an owner name. `AuthorizeMedia` fits `mediastore.Config.Authorize`, so a
private blob serves only to the session that owns it.

Sign-in arrives by emailed code or through an external provider. The send ceilings refuse before
anything is sent, and a known address and an unknown address get the same answer, so no screen
ever learns who registered. The mail sender is an interface the app implements, and the provider
client stays in the app too. The provider flow runs the authorization code protocol with state,
nonce and PKCE, and keys the identity on the provider subject, never on an address. A sign-in on
a fresh address attaches to the current guest, so the user id never changes and the guest's rows
become the account's rows. An address that already belongs to another user moves the device to
that user, or refuses with the guest data conflict code while the guest still owns app data.

Deleting an account needs a code typed while it is still live, and the verification consumes the
code it checked. The deletion runs as one `job` that fans the erasure out over the targets the
app registers and removes the user row last, so a restart resumes what is left. Every answer
travels the `wire` envelope with stable codes, so screens branch on codes and never on wording.

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

### Assemble an illustrated PDF

```go
var pdf bytes.Buffer
err := book.Write(ctx, &pdf, book.Config{
    FontDir:  "fonts",
    FontFile: "Face.ttf",
}, []book.Page{{
    Image:     png,
    ImageType: "png",
    Caption:   "The gate opens.",
}})
```

`book` draws one sheet per page: the image fitted inside the margins, and the
caption under it. The face is a file name inside `FontDir`, so a caption cannot
point the loader at an arbitrary path. Image-only pages need no font. A caption
taller than the page, or one that leaves no room for the image, is refused, and
so is a page with neither an image nor a caption. Caption height is measured in
runes, so a non-ASCII caption is not accepted when it would run past the margin.
The PDF library does not shape complex scripts. `Write` returns an error,
including when the PDF library panics, and a cancelled context writes nothing.

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

### Ship events to a remote sink

```go
db, err := sqlite.Open(ctx, sqlite.Config{Path: "app.db"})
store, err := outboxsql.Open(ctx, outboxsql.Config{DB: db})
box, err := outbox.Open(ctx, outbox.Config{Store: store, Sink: sink})

entry, err := box.Add(ctx, payload)
go box.Loop(ctx)
```

`outbox` writes each event to SQLite before `Add` returns, so a crash between the write and the
next delivery pass loses nothing. `Flush` hands the pending entries to the sink in insertion
order, one batch at a time, and retires a batch only after `Deliver` returned nil for all of it.
A batch the sink refuses ends the pass, so no later batch overtakes it, and each entry in it
records one failure.

Delivery is at-least-once from the outbox side. A crash after the sink accepted a batch but
before it was retired replays that batch, and so does a retirement the store could not write. A
sink that must not see a repeat checks `entry.ID`, which `Add` assigns and which never changes
across replays, and drops an id it has served. At-most-once is a sink decision, and the stable
id is everything the sink needs to make it.

An entry that exhausts `MaxAttempts` attempts stays stored with its failure count, and
`Exhausted` reports it, so a sink that never accepts one entry is visible instead of silent.
Batch size, pass interval, attempt cap and the retry waits come from `Config`. The zero value
works: a hundred-entry batch, a pass every second, ten attempts, and a retry wait that starts
at five seconds and doubles up to a minute. `Loop` runs a pass on every interval until
its context is done, and backs off on the same curve after a failed pass. `outbox/sqlitestore`
owns its namespaced migration, like every other store here.

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

### Fetch a link a user supplied

```go
resp, err := fetch.Get(ctx, link, fetch.Config{ContentTypes: []string{"text/html"}})
if err != nil {
	var fetchErr *fetch.Error
	if errors.As(err, &fetchErr) {
		wire.WriteError(w, http.StatusBadRequest, fetchErr.Code, fetchErr.Message, nil)
	}
	return err
}
title, image := fetch.Meta(resp)
```

`fetch.Get` judges a link on the address it dials, not on the name it carries. The dialler
resolves the name, and the control hook refuses loopback, private, link-local and unique-local
ranges, the carrier NAT range, multicast, and the cloud metadata addresses before a packet
leaves. A link whose name looks public but resolves inside is refused, which is what a DNS
rebinding attack cannot get past. Every redirect hop dials again, so every hop is judged again.

Schemes are https, or http beside it when `Config.AllowHTTP` says so. Ports are 80 and 443
unless `Config.Ports` names others. `Config.MaxRedirects` caps the hops at 5. `Config.MaxBytes`
caps the body at 10 MiB and refuses the whole fetch rather than truncating it. `Config.Timeout`
and `Config.HeaderTimeout` cap the total time and one hop's wait for headers.
`Config.ContentTypes` is an allowlist checked against the Content-Type header and against the
sniffed bytes, so a page that lies about its type is refused.

Every refusal carries a stable code on `*fetch.Error`, such as `fetch_blocked_address`,
`fetch_too_large` and `fetch_bad_type`, ready to be copied into the `wire` envelope.
`fetch.Meta` reads the open graph title and image, falling back to the twitter card image. A
relative image address resolves against the page's final URL, so the image is fetched back
through `Get` and meets the same policy. `Config.Classifier` replaces the address policy where a
caller has judged its own addresses, which is also how tests reach a server on loopback. The
package keeps no cookies and retries nothing, so a caller who wants a wait between attempts wraps
`Get` in `throttle`.

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
both. The broker retains the terminal event for the bound in `stream.Config.Retain`, one minute at defaults, so a subscriber that joins within retention still receives it. A subscriber that joins after expiry gets live events only. `ServeTopic` also replays frames after a `Last-Event-ID` that is still in the topic ring. A cursor that is the retained terminal closes the response. The client
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

That runs the same twelve checks CI runs: formatting, vet, staticcheck, a `CGO_ENABLED=0` build,
the race-enabled tests, two content scans, three dependency-boundary checks that keep SQLite, the
TOML parser and the PDF library inside their packages, a convention checker, and the frozen-API diff. It needs
`ffmpeg` and `ffprobe` on `PATH` for the media tests.

## License

Apache-2.0. See [LICENSE](LICENSE).
