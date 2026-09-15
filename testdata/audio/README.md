# Audio fixtures

This directory holds deterministic audio fixtures for the decoder tests. The script regenerates every file byte-identically with ffmpeg n9.0.1 and python3 3.14.7. Run `tools/make_fixtures.sh` from anywhere in the repository.

Durations and sample rates below come from ffprobe. Durations are in seconds.

| File | Duration | Sample rate | Bytes | SHA-256 |
| --- | --- | --- | --- | --- |
| tone-48k.wav | 3.000000 | 48000 Hz | 288044 | 9a593112fb5663188883aa2d740268600a002e87c1d8016ca92ba511ed753c33 |
| tone-24k.wav | 3.000000 | 24000 Hz | 144044 | 4054a016e840d7c0fcd977267c2f8a65ffb3a9d506502cf8c011bf0749655528 |
| tone-48k.opus | 3.006500 | 48000 Hz | 29395 | d281fcd50dfcd6995e7d4dbc70f68591bacebcb5d6f3cfffbff635642c36f43f |
| tone-48k.mp3 | 3.000000 | 48000 Hz | 48788 | 3b0e5156bfa621f1d15179cd1f5708f4543df4ead5f91b4fc6aa2d7c614801f7 |
| truncated.wav | 1.194208 (estimated) | 48000 Hz | 114688 | 7f32b96cbf18152203400036dad2e6c0bf70af6a82d4bbee107381ab3ee3707a |
| random-20m.bin | N/A | N/A | 20971520 | e628b4e4b0ef3ddd578a8724ec0034593809836af00226372c305f88d0926770 |

Every tone file holds one 440 Hz sine channel. `tone-48k.wav` uses pcm_s16le at 48 kHz. `tone-24k.wav` uses pcm_s16le at 24 kHz. `tone-48k.opus` is an Ogg Opus file encoded with libopus. `tone-48k.mp3` is an MP3 encoded with libmp3lame.

`truncated.wav` is the first 28 blocks of 4096 bytes of `tone-48k.wav`. The cut lands inside the data chunk. ffprobe warns about a corrupt packet and estimates the duration from the bitrate.

`random-20m.bin` holds 20971520 bytes from the seeded Mersenne Twister in the script. Its bytes carry no audio.
