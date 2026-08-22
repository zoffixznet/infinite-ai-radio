# Infinite AI Radio

Infinite AI Radio plays endless AI-generated music in your terminal,
running entirely on your own machine. Start it and music plays; type plain English while it
plays ("calmer", "add vocals about winning", "switch to piano") and the
stream follows. No cloud, no API keys, no subscriptions.

It is a single Go binary (`iar`) that manages everything else: it installs the
music model, supervises it, buffers generated tracks ahead of playback,
joins them with smooth crossfades, and keeps audio flowing even when the
generator hiccups. It is built for background listening: focus and study
music, ambient, sleep sounds, and an energetic vocal mode for workouts.

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Running](#running)
- [Starting from a prompt](#starting-from-a-prompt)
- [Steering the music](#steering-the-music)
- [Commands](#commands)
- [Sessions](#sessions)
- [Presets](#presets)
- [MP3 export](#mp3-export)
- [Saving tracks you like](#saving-tracks-you-like)
- [Listening from your phone](#listening-from-your-phone)
- [Accounts and permissions](#accounts-and-permissions)
- [Email setup](#email-setup)
- [Saved chunks and tags](#saved-chunks-and-tags)
- [Desktop integration](#desktop-integration)
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
- Optional: a local [Ollama](https://ollama.com) daemon. When present, the player
  uses it to polish prompts and write lyrics; without it, a built-in
  deterministic path is used and everything still works.

## Install

```sh
make deps    # check system tools; prints sudo apt-get line if anything is missing
make build   # build the ./iar binary
make setup   # one-time: install the music engine and download models (~18 GB)
```

`make setup` is resumable: if the download is interrupted, run it again and
it continues where it stopped. It never uses sudo; everything lands in your
user directories. `make install` copies the binary to `~/.local/bin`.

## Running

```sh
./iar
```

That is all. Setup pre-generates a small library of starter tracks, so a
launch begins playing one within a few seconds and crossfades to freshly
generated music as soon as it is ready (each generation is independent,
so playing a banked track never changes what gets generated). While anything loads, Infinite AI Radio shows
live progress: which phase it is in, how long it has been running, and
how long it usually takes.

Behind the scenes the engine runs as a shared background process. Three
numbers matter: launch with banked tracks, audio in seconds; fresh
generation ready in under a minute while the engine is warm, or a couple
of minutes after a cold engine start; and the idle engine shuts itself
down 15 minutes after the last Infinite AI Radio process exits (that number is a
memory-saver, not a boot time).

Useful variants:

```sh
./iar "dark techno"           # start straight from a prompt
./iar --preset lofi-study     # start from a built-in preset
./iar --session gym-grind     # resume a saved session
./iar --engine noise          # noise only, no GPU needed
./iar --plain                 # line-based interface (also used automatically in pipes)
./iar presets                 # list the built-in presets
./iar doctor                  # check your environment
./iar engine status           # is the shared engine running and ready?
./iar engine stop             # stop the engine now and free GPU memory
```

Quit with `quit` (or Ctrl+C). Music generation runs a few times faster than
realtime on a modern GPU, so the stream stays ahead of playback.

The engine runs as a shared background process: quitting Infinite AI Radio leaves it
warm so the next launch starts making music almost immediately, and it
shuts itself down after 15 minutes without a Infinite AI Radio process using it
(tunable via `idle_minutes`). Only one interactive Infinite AI Radio player runs at a
time; a second one tells you where the first is.

## Starting from a prompt

You do not need a preset; describe what you want:

```sh
./iar "dark techno"
./iar --prompt "energetic rock with vocals about winning"
```

Inside the player, `new <prompt>` drops the current steering context and
starts a fresh session seeded with the prompt (auto-persisted like any
session, name it with `name`). Vocal requests in the prompt work the
same way they do in steering.

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
| `new <prompt>` | fresh session from a prompt |
| `save [prev] [tag]` | save the playing (or previous) track as MP3, filed under a tag |
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

Longer output than fits on screen starts at its top with a "more below"
marker; PgUp/PgDn scroll the message backlog and End (or Esc) returns to
the live view.

## Sessions

Everything you type is persisted automatically; you never have to save.
Name the current session to make it easy to find again:

```
name gym-grind
```

Later, get the same vibe back with `./iar --session gym-grind` or `load
gym-grind` inside the app. `./iar sessions` lists everything. Session files
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

Select at launch (`./iar --preset sleep`) or inside the app
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
./iar export --minutes 20 --preset sleep
./iar export --minutes 30 --session gym-grind --out ~/Music/grind.mp3
```

Exports are capped at 180 minutes per run.

## Saving tracks you like

When a track lands just right, type:

```
save
```

and the currently playing track is written as a high-quality MP3 (with
the prompt in its tags) into the snippets folder, path shown in the
acknowledgment. `save prev` captures the previous track instead, for
when it clicks a moment too late. Saving never interrupts playback.

Add a tag to file the track where you will find it again:

```
save gym
save prev late night
```

Tags become folder names (`snippets/gym/`, `snippets/late_night/`;
anything that is not a letter, digit or underscore turns into an
underscore) and are also written to the MP3's album tag. Saves without
a tag go to `snippets/untagged/`, and tracks saved before tags existed
are moved there the next time the player starts. The phone remote's saved-chunk player
loops these folders by tag (see [Saved chunks and tags](#saved-chunks-and-tags)).
The base folder is configurable via `snippets_dir`.

## Listening from your phone

Infinite AI Radio has a built-in web remote: a phone-first page with the
live stream, the now-playing state, a steering box, a start-fresh action,
a save button with a tag field, and a player for the tracks you have
saved. Everything on it needs a login, and the first account is created
in the terminal:

```sh
./iar remote setup          # create the first admin (email + password, typed twice)
./iar --remote              # start playing with the remote on
```

(`remote.enabled` in the config keeps it on permanently.) The player
prints the URL to open; `iar doctor` shows it too under the remote
section. Open it on the phone, log in, tap play. The stream is MP3 at
~192 kbps and runs a few seconds behind the machine's speakers;
steering, starting fresh and saving act instantly and show up in the
terminal as well.

<p>
<img src="assets/remote-login.png" alt="the login page" width="190">
<img src="assets/remote-player.png" alt="the live stream page" width="190">
<img src="assets/remote-saved.png" alt="the saved chunks player" width="190">
<img src="assets/remote-users.png" alt="the users page with a fresh invite link" width="190">
</p>

For safety the remote binds only to localhost and, when the machine has
one, its Tailscale address; it never listens on your LAN or the internet
unless you add addresses to `remote.bind` in the config. The intended
setup is a private [Tailscale](https://tailscale.com) network between
your computer and phone:

1. Install Tailscale on the computer per the official Linux guide
   (`https://tailscale.com/download/linux`; installing and running
   `sudo tailscale up` needs sudo) and sign in.
2. Install the Tailscale app on the phone and sign in to the same
   account.
3. Run `./iar remote setup` once, then start `./iar --remote`.
4. Open the printed `http://100.x.y.z:8246` URL in the phone's browser,
   log in and tap play.

To also reach the remote on your home LAN (say your laptop is
192.168.1.20), add that address to `remote.bind`; the player keeps
listening on localhost and the tailnet as well, and addresses you bind
are automatically accepted in URLs:

```json
{ "remote": { "enabled": true, "bind": ["192.168.1.20"] } }
```

Then open `http://192.168.1.20:8246/` from any device on that network
and log in. `"bind": ["0.0.0.0"]` listens on every network the machine
is on.

Security notes, plainly:

- Logins happen over plain HTTP. Tailscale encrypts everything between
  the devices, so that is fine on the tailnet. On your home LAN a
  password travels in clear text to the laptop; that is your call for
  a network you trust. Never expose the port to the public internet
  without TLS in front of it (a reverse proxy with a certificate);
  behind such a proxy the login cookie is marked secure automatically.
- A login lasts 30 days of inactivity on that browser (it is a radio).
  Log out from the menu to end it early; changing or resetting a
  password logs every other device out.
- Failed logins are rate-limited per address and per account, and the
  page never reveals whether an email exists.
- The remote refuses requests whose Host or Origin is not localhost,
  your tailnet address, an address you bound, or an entry in
  `remote.allowed_hosts` (a cross-site and DNS-rebinding defense).

## Accounts and permissions

Every listener has an account: the email address is the login, and
only the account holder ever knows the password. The first admin is
made with `./iar remote setup`; after that everything happens on the
remote's **Users** page (visible to admins):

- **Adding a user** takes an email and four checkboxes. It produces an
  invitation link, shown with a Copy button (and emailed too if
  [email is set up](#email-setup)). Send it by text or chat; the person
  opens it, sees their email, chooses a password, and is logged in. The
  link works once and expires after 7 days. The invitee has to be able
  to reach the address in the link, so create it from a browser that is
  on the same route in (the tailnet address for tailnet users, the LAN
  address for LAN users).
- **Pending links** are listed with Regenerate (which invalidates the
  old link) and Revoke.
- **Permissions** are four independent switches per account: *admin*
  (manage users and links), *can steer*, *new prompts*, *can save*.
  Listening needs none of them. Admin does not imply the other three;
  an admin can tick them for themselves. The page only shows the
  controls an account may use, and the server refuses the rest either
  way.
- **Password reset**: an admin presses *Reset link* on the account. The
  link works once, expires after 24 hours, and the user chooses the new
  password themselves; every other login of that account ends.
- **Guard rails**: you cannot delete your own account, and the last
  admin can neither be deleted nor demoted.

Each user changes their own password on the **Account** page. Forgot it?
An admin hands you a reset link. If the only admin is locked out, run
`./iar remote setup` again with that email in the terminal: it resets the
password and restores every permission.

Accounts and login sessions are small JSON files under the data
directory (`remote/users.json`, `remote/sessions.json`), readable only
by your user. Logins, user changes and refused actions all show up in
the log.

## Email setup

Optional. With no email configured, you pass invitation and reset links
on yourself, and nothing is missing. If you would rather have them
emailed automatically as well, point `remote.smtp` at a mail provider:

```json
{ "remote": { "smtp": {
    "host": "smtp-relay.brevo.com", "port": 587, "tls": "starttls",
    "username": "your-login", "password": "your-smtp-key",
    "from": "radio@example.com" } } }
```

and check it with:

```sh
./iar remote test-email you@example.com
```

Providers with a free tier that works for this (re-checked August
2026; numbers change, so confirm on their pricing pages):

- [Brevo](https://www.brevo.com): 300 emails a day on the free plan,
  SMTP relay at `smtp-relay.brevo.com:587`.
- [Resend](https://resend.com): 3,000 emails a month (100 a day) free,
  SMTP at `smtp.resend.com:465` with `"tls": "tls"` (username
  `resend`, password is an API key).
- [Mailjet](https://www.mailjet.com): 200 a day (6,000 a month) free,
  SMTP relay at `in-v3.mailjet.com:587`.
- Gmail: `smtp.gmail.com:587` with an
  [app password](https://support.google.com/accounts/answer/185833)
  (requires 2-step verification on the Google account); about 500
  messages a day.

`"tls"` is `"starttls"` (default, port 587), `"tls"` (implicit TLS, port
465) or `"none"`. Keep the config file readable only by you when it
holds a password (`chmod 600`); `iar doctor` warns otherwise.

There is deliberately no "just send it from my computer" mode: mail sent
straight from a home connection is blocked by most ISPs and rejected or
spam-foldered by the big mailbox providers, so it would fail quietly
for exactly the people it is meant for. The links are the reliable path;
email is a convenience on top.

## Saved chunks and tags

The remote's **Saved chunks** mode plays the tracks you have saved,
entirely on the phone: it never touches the live stream, other
listeners, or the laptop's speakers.

- Tick the tags you want (every tag with at least one saved track is
  listed; all are selected to begin with) and the player loops through
  their chunks forever.
- Each chunk shows the prompt that produced it, its tag, length and
  when it was saved, with *Play* and *Loop this one*. Looping one chunk
  repeats it until you press *Back to looping the tags*.
- The built-in controls seek, pause and set volume as usual.
- The mode and tag selection are remembered per browser.

Saving from the phone works like the terminal `save`: an optional tag
in the box next to the button, the file appears in the chunk list a
moment later.

## Desktop integration

Infinite AI Radio shows up as a regular media player (MPRIS) on the desktop bus, so
media keys, KDE's media controls, KDE Connect and `playerctl` all work:

```sh
playerctl --player iar play-pause
playerctl --player iar next
playerctl --player iar volume 0.5
playerctl --player iar metadata xesam:title
```

PipeWire/PulseAudio per-stream volume also works independently of the player
(the playback stream belongs to `pw-play`):

```sh
pactl set-sink-input-volume "$(pactl list sink-inputs \
  | awk '/^Sink Input #/{id=substr($3,2)} /application.name = "pw-play"/{print id; exit}')" 50%
```

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
- Tracks are loudness-normalized to a consistent level, and the whole
  pipeline is lossless (48 kHz WAV from the engine, raw PCM to your
  speakers); only exports and snippets are encoded, at top MP3 quality.
  The remaining quality ceiling is the model itself: `inference_steps`
  defaults to 12 (measurably more spectral detail than the model's
  quick-start 8, at no meaningful speed cost on a strong GPU) and can
  be raised to 20 for a little more brightness, but no setting turns
  the model into a mastering studio.

If you run [Ollama](https://ollama.com), the player uses it in the background to
polish prompts and write themed lyrics; it never delays the music, and
it quietly stops asking if the model is slow or failing.

Track-to-track transitions are equal-power crossfades (about 3 seconds by
default), which suits continuous background listening.

## Configuration

Optional. The player reads a JSON config file (path shown by `iar doctor`,
usually `~/.config/iar/config.json`) with these defaults:

```json
{
  "engine": "acestep",
  "player": "auto",
  "track_seconds": 150,
  "crossfade_seconds": 3,
  "buffer_tracks": 2,
  "volume": 80,
  "bed_while_waiting": false,
  "pipe_latency_ms": 200,
  "normalize_loudness": true,
  "mp3_quality": 0,
  "snippets_dir": "",
  "library_max_mb": 600,
  "remote": {
    "enabled": false, "port": 8246, "bind": [], "allowed_hosts": [],
    "smtp": { "host": "", "port": 0, "username": "", "password": "", "from": "", "tls": "starttls" }
  },
  "acestep": {
    "port": 0,
    "idle_minutes": 15,
    "lm_model_path": "",
    "lm_backend": "auto",
    "inference_steps": 12,
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
Data lives under XDG paths (`~/.local/share/iar` by default); the
`IAR_DATA_DIR` and `IAR_CONFIG_DIR` environment variables relocate
everything, which is also how the test suite keeps clear of your real
state.

## Models and licensing

The code is MIT licensed. Model weights are never bundled with the player;
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

`iar doctor` checks your environment: system tools, GPU, engine install,
and whether the engine API is responding. The structured log (path printed
by doctor) has the full story including the engine's own output.

Common cases:

- **"music engine not installed"**: run `make setup`.
- **No sound**: confirm `pw-play` or `pacat` exists and plays something;
  try `./iar --player pipe`.
- **First track takes long**: the initial model load takes a few minutes;
  the progress display shows which phase is running and how long it
  usually takes. While the engine stays warm (see `iar engine status`),
  later launches skip the load entirely.
- **Generation failures**: the stream degrades gracefully (buffer, then
  looping the last track, then a noise bed). The engine restarts itself
  after repeated failures, and instantly on known-fatal faults, even
  when its health endpoint still claims everything is fine; expect at
  most a couple of minutes of looped music while it reloads. The
  interface and `iar doctor` show the current failure streak and the
  last reason.
- **Buzz or static from the speakers while a track generates**: heavy
  GPU load can induce electrical interference in analog audio chains
  (laptop headphone out, unbalanced cables into a mixer) that sounds
  like cell-phone buzz and follows the GPU's duty cycle, not the audio
  data. Check the underrun counter in the interface header and in
  `iar doctor`: if it stays at zero while you hear the noise, the audio
  stream itself is clean and the interference is happening after the
  digital output. Mitigations that work: cap the GPU's power draw
  (`sudo nvidia-smi -pl <watts>`), use shielded or shorter audio
  cables, ground the laptop's power supply, or switch to a digital
  output (USB DAC/interface, HDMI audio).

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
- The phone remote has no self-service password reset: an admin hands
  out reset links, and a locked-out sole admin recovers with
  `iar remote setup` in the terminal.

## Development

```sh
make help          # list targets
make test          # unit tests (silent; no audio devices touched)
make smoke         # end-to-end test of the built binary (sandboxed, silent)
make browser-test  # the phone remote in headless Firefox (needs geckodriver, firefox, pactl)
make screenshots   # re-shoot the README's remote screenshots into assets/
make lint          # go vet + gofmt check
```

The test suite, smoke test and browser test never emit audible sound:
they use the null or file audio backends, sandboxed data directories,
and a temporary null audio sink for the browser.
