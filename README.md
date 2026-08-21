# bgm

Endless AI-generated background music in your terminal, running entirely on
your own machine. Start it and music plays; type plain English while it
plays ("calmer", "add vocals about winning", "switch to piano") and the
stream follows. No accounts, no API keys, no cloud.

bgm is a single Go binary that manages everything else: it installs the
music model, supervises it, buffers generated tracks ahead of playback,
joins them with smooth crossfades, and keeps audio flowing even when the
generator hiccups. It is built for background listening: focus and study
music, ambient, sleep sounds, and an energetic vocal mode for workouts.

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Running](#running)
- [Steering the music](#steering-the-music)
- [Commands](#commands)
- [Sessions](#sessions)
- [Presets](#presets)
- [MP3 export](#mp3-export)
- [What the music sounds like](#what-the-music-sounds-like)
- [Configuration](#configuration)
- [Models and licensing](#models-and-licensing)
- [Troubleshooting](#troubleshooting)
- [Known limitations](#known-limitations)
- [Development](#development)

## Requirements

- Linux with PipeWire or PulseAudio (the player uses `pw-play` or `pacat`).
  Other platforms are not supported yet.
- An NVIDIA GPU with 8 GB+ VRAM and a driver capable of CUDA 12.8 is
  strongly recommended for music generation. Without a GPU the music engine
  is impractically slow, but the noise modes (pink/white/brown) still work.
- About 20 GB of disk space for the engine and model weights.
- `ffmpeg` (with libmp3lame), `git`, and a C compiler for the engine's
  Python dependencies. `make deps` checks these and prints the exact
  install command for anything missing.
- Go 1.26+ to build.
- Optional: a local [Ollama](https://ollama.com) daemon. When present, bgm
  uses it to polish prompts and write lyrics; without it, a built-in
  deterministic path is used and everything still works.

## Install

```sh
make deps    # check system tools; prints sudo apt-get line if anything is missing
make build   # build the ./bgm binary
make setup   # one-time: install the music engine and download models (~18 GB)
```

`make setup` is resumable: if the download is interrupted, run it again and
it continues where it stopped. It never uses sudo; everything lands in your
user directories. `make install` copies the binary to `~/.local/bin`.

## Running

```sh
./bgm
```

That is all. While the engine starts and the first track generates, bgm
shows live progress (what phase it is in, how long it has been running,
and how long it usually takes); music begins as soon as the first track
is ready (the very first track is generated a bit shorter so it arrives
sooner). The engine keeps running in the background between launches, so
after the first start, relaunching bgm reaches music in well under a
minute.

Useful variants:

```sh
./bgm --preset lofi-study     # start from a built-in preset
./bgm --session gym-grind     # resume a saved session
./bgm --engine noise          # noise only, no GPU needed
./bgm --plain                 # line-based interface (also used automatically in pipes)
./bgm presets                 # list the built-in presets
./bgm doctor                  # check your environment
./bgm engine status           # is the shared engine running and ready?
./bgm engine stop             # stop the engine now and free GPU memory
```

Quit with `quit` (or Ctrl+C). Music generation runs a few times faster than
realtime on a modern GPU, so the stream stays ahead of playback.

The engine runs as a shared background process: quitting bgm leaves it
warm so the next launch starts making music almost immediately, and it
shuts itself down after 15 minutes without a bgm process using it
(tunable via `idle_minutes`). Only one interactive bgm player runs at a
time; a second one tells you where the first is.

## Steering the music

While music plays, just type what you want and press Enter:

```
make it more energetic
calmer
faster
switch to piano
add vocals about winning
no vocals
generate pink noise
```

Each input is acknowledged with how it was understood and shapes the next
generated track (the current track finishes playing; use `skip` to jump).
If the engine is still loading when you type, the acknowledgment says so
and estimates how long until your steering can be heard.
Steering accumulates: "calmer" then "no drums" gives you calm, drumless
music. `clear` wipes the accumulated steering and returns to the session's
base sound.

## Commands

Commands work with or without a leading slash, always with plain words and
Enter:

| Command | Effect |
| --- | --- |
| `clear` | wipe the steering context |
| `name <name>` | save the current session under a name |
| `sessions` | list presets and saved sessions |
| `load <name>` | resume a saved session |
| `preset <name>` | switch to a built-in preset |
| `mp3 <minutes> [file]` | export minutes of the current vibe to MP3 |
| `skip` | jump to the next track |
| `pause` / `resume` | pause or continue output |
| `volume <0-100>` | set output volume |
| `status` | engine, buffer and session status |
| `help` | list commands |
| `quit` | exit |

## Sessions

Everything you type is persisted automatically; you never have to save.
Name the current session to make it easy to find again:

```
name gym-grind
```

Later, get the same vibe back with `./bgm --session gym-grind` or `load
gym-grind` inside the app. `./bgm sessions` lists everything. Session files
are plain JSON in your data directory.

## Presets

Six curated starting points ship built in. They behave like read-only
sessions: starting from one seeds a fresh session you can steer and name.

| Preset | Sound |
| --- | --- |
| `lofi-study` | chill lofi hip hop beats for studying and working |
| `deep-focus` | beatless ambient pads for deep concentration |
| `sleep` | slow beatless drones for falling asleep |
| `calm-piano` | gentle solo piano, quiet and intimate |
| `grind` | energetic electronic rock with motivational vocals |
| `pink-noise` | steady pink noise, no music |

Select at launch (`./bgm --preset sleep`) or inside the app
(`preset sleep`).

## MP3 export

Render any amount of the current session's sound to a file you can copy to
a phone or player:

```
mp3 20
```

produces a ~20-minute MP3 (path printed, default in the exports directory).
The export runs in the background and never interrupts playback: it only
generates while the playback buffer is full. There is also a headless
subcommand:

```sh
./bgm export --minutes 20 --preset sleep
./bgm export --minutes 30 --session gym-grind --out ~/Music/grind.mp3
```

Exports are capped at 180 minutes per run.

## What the music sounds like

The default engine is ACE-Step 1.5, a full-song generation model.
Honestly, by style:

- **Lofi, electronic, and beat-driven music** is the strong suit:
  produced-sounding tracks with real instrument timbres, coherent rhythm
  and structure. This is what the model was made for.
- **Ambient and sleep material** works but the model is song-trained, so
  it sometimes injects rhythm or song structure where you wanted pure
  texture. The presets use "beatless, no drums" phrasing to counter this;
  occasional tracks still drift toward songhood. Steering ("more drone,
  no melody") helps.
- **Solo piano** comes out convincingly piano-like, though more "produced
  piano track" than "microphone in a quiet room".
- **Vocals** are genuinely supported: tracks come with sung lyrics in a
  pop/rock delivery, and the singing follows the written lyrics closely
  enough that speech recognition can transcribe most lines back. The
  occasional garbled word or artifact happens, especially on fast verses.
  The `grind` preset gives a fair picture of vocal quality. With Ollama
  installed the lyrics follow your theme closely; without it the engine's
  own planner writes them from a description (and typically picks its own
  track length while doing so).
- Every generation has some luck involved; a weak track is usually
  followed by a better one, and `skip` is always there.

If you run [Ollama](https://ollama.com), bgm uses it in the background to
polish prompts and write themed lyrics; it never delays the music, and
bgm quietly stops asking if the model is slow or failing.

Track-to-track transitions are equal-power crossfades (about 3 seconds by
default), which suits continuous background listening.

## Configuration

Optional. bgm reads a JSON config file (path shown by `bgm doctor`,
usually `~/.config/bgm/config.json`) with these defaults:

```json
{
  "engine": "acestep",
  "player": "auto",
  "track_seconds": 150,
  "crossfade_seconds": 3,
  "buffer_tracks": 2,
  "volume": 80,
  "bed_while_waiting": false,
  "acestep": {
    "port": 0,
    "idle_minutes": 15,
    "lm_model_path": "",
    "lm_backend": "auto",
    "inference_steps": 8,
    "thinking": true
  },
  "ollama": {
    "enabled": true,
    "url": "http://127.0.0.1:11434",
    "model": ""
  }
}
```

See [docs/configuration.md](docs/configuration.md) for what each knob does.
Data lives under XDG paths (`~/.local/share/bgm` by default); the
`BGM_DATA_DIR` and `BGM_CONFIG_DIR` environment variables relocate
everything, which is also how the test suite keeps clear of your real
state.

## Models and licensing

bgm's own code is MIT licensed. Model weights are never bundled with bgm;
they are downloaded on demand from their publishers, and the default model
requires no account, token, or license click-through.

| Model | Used for | Weights license |
| --- | --- | --- |
| [ACE-Step 1.5](https://huggingface.co/ACE-Step/Ace-Step1.5) (default) | music generation | MIT |
| [ACE-Step 5Hz LM 0.6B](https://huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B) (optional, auto-selected on smaller GPUs) | planning/lyrics inside the engine | MIT |

The ACE-Step model card states that generated music may be used
commercially. If you point the optional Ollama integration at a model of
your choice, that model's own license applies to it.

See [docs/models.md](docs/models.md) for more detail.

## Troubleshooting

`bgm doctor` checks your environment: system tools, GPU, engine install,
and whether the engine API is responding. The structured log (path printed
by doctor) has the full story including the engine's own output.

Common cases:

- **"music engine not installed"**: run `make setup`.
- **No sound**: confirm `pw-play` or `pacat` exists and plays something;
  try `./bgm --player pipe`.
- **First track takes long**: the initial model load takes a few minutes;
  the progress display shows which phase is running and how long it
  usually takes. While the engine stays warm (see `bgm engine status`),
  later launches skip the load entirely.
- **Generation failures**: the stream degrades gracefully (buffer, then
  looping the last track, then a noise bed) while the engine restarts;
  check the log for the engine's error output.

## Known limitations

- Linux only for now, and playback expects PipeWire or PulseAudio.
- The music engine needs an NVIDIA GPU to be practical; CPU-only machines
  are limited to the noise modes.
- On gaming laptops, hours-long generation sessions can heat the GPU
  enough to throttle. Capping the GPU power limit (`nvidia-smi -pl`, needs
  root) noticeably reduces heat with little quality impact.
- A steering tweak affects the next generated track, not the one already
  playing; with the default track length that can mean a couple of minutes
  before you hear it (use `skip` to get there sooner).
- Vocal lyrics are short and chorus-driven; this is a background-music
  tool, not a songwriting studio.

## Development

```sh
make help    # list targets
make test    # unit tests (silent; no audio devices touched)
make smoke   # end-to-end test of the built binary (sandboxed, silent)
make lint    # go vet + gofmt check
```

The test suite and smoke test never emit audible sound: they use the null
or file audio backends and sandboxed data directories.
