#!/usr/bin/env bash
# Generate the deterministic audio fixtures under testdata/audio.
#
# Every output is a pure function of the options below. Nothing reads the
# clock, the environment, or system entropy, so repeated runs on the same
# toolchain produce byte-identical files.
#
# Schemes:
# - Tones come from the lavfi sine source at fixed sample rates. One 440 Hz
#   sine runs for exactly 3 seconds at each rate.
# - The bitexact flag pins the Ogg stream serial number. Without it the muxer
#   draws a random serial per file, and every page CRC follows that serial.
#   The flag also drops tool version strings from container headers.
# - random-20m.bin uses the Python standard library Mersenne Twister with the
#   fixed seed 571033. random.Random(571033).randbytes(1 << 20) runs 20 times
#   and the chunks are written in order. No /dev/urandom bytes are used.
# - truncated.wav is the first 28 blocks of 4096 bytes of tone-48k.wav, so the
#   cut lands inside the data chunk at about forty percent of the file.
#
# Outputs are staged in a temporary directory and moved into place. The final
# file names are fixed and never derived from the staging path.

set -euo pipefail

command -v ffmpeg >/dev/null
command -v python3 >/dev/null

cd "$(dirname "$0")/.."

out=testdata/audio
names=(tone-48k.wav tone-24k.wav tone-48k.opus tone-48k.mp3 truncated.wav random-20m.bin)

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i sine=frequency=440:sample_rate=48000:duration=3 \
  -c:a pcm_s16le -ac 1 -fflags +bitexact "$stage/tone-48k.wav"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i sine=frequency=440:sample_rate=24000:duration=3 \
  -c:a pcm_s16le -ac 1 -fflags +bitexact "$stage/tone-24k.wav"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i sine=frequency=440:sample_rate=48000:duration=3 \
  -c:a libopus -b:a 64k -ac 1 -fflags +bitexact "$stage/tone-48k.opus"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i sine=frequency=440:sample_rate=48000:duration=3 \
  -c:a libmp3lame -b:a 128k -ac 1 -fflags +bitexact "$stage/tone-48k.mp3"

# 28 blocks of 4096 bytes keep about forty percent of the source and cut the
# stream inside its data chunk.
dd if="$stage/tone-48k.wav" of="$stage/truncated.wav" bs=4096 count=28 status=none

# Seeded Mersenne Twister, 20 chunks of 1 MiB, written in order.
python3 - "$stage/random-20m.bin" <<'PY'
import random
import sys

rng = random.Random(571033)
with open(sys.argv[1], "wb") as f:
    for _ in range(20):
        f.write(rng.randbytes(1 << 20))
PY

mkdir -p "$out"
for name in "${names[@]}"
do
  rm -f "$out/$name"
  mv "$stage/$name" "$out/$name"
done

printf 'wrote %d fixtures into %s\n' "${#names[@]}" "$out"
