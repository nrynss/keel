# Keel

Go building blocks for a media app that sits on a public URL, calls paid
APIs, and runs on one box.

Every product built this way needs the same machinery underneath it: a gate
so the URL is not an open wallet, long work that outlives its request and
reports progress, media stored privately and served with seeking, ffmpeg that
dies with its job, and a record of what everything cost. Keel is that
machinery, extracted so each product stops rebuilding it.

## Packages

| Package | Does |
|---|---|
| `wire` | The error envelope and the event contract every response and stream shares |
| `id` | Unguessable 128-bit ids in lowercase hex, for anything served in public |
| `gate` | Per-client and global token buckets, plus an optional passcode, in front of the routes that spend money |
| `stream` | Topic broker and server-sent events, with heartbeats and a non-blocking slow-subscriber policy |
| `job` | Long work started by a short request, observed over `stream`, durable across restarts |
| `mediastore` | Blobs on disk under unguessable ids, served with Range support, with retention sweeps |
| `ffmpeg` | ffmpeg and ffprobe bound to a context, with a bounded wait on shutdown |
| `cost` | Money as integer nanodollars, and a ledger of charges |
| `sqlite` | One SQLite file with WAL, a writer handle, a read-only reader pool, namespaced migrations and online backup |
| `upload` | Resumable chunked uploads that land in `mediastore`, staged on disk and resumable by id after a dropped connection |

## Examples

Every package carries a runnable example that shows its common case. Run
`go test -run Example -v ./...` to see them.

## Status

Tagged `v0.1.0`. The exported API is frozen in `api/v0.1.0.txt`, and CI
rejects any change to it. The packages are extracted from services that
already run them in production.

## License

Apache-2.0. See [LICENSE](LICENSE).
