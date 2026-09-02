# Infinite AI Radio

Endless AI-generated music, made on your own machine. Start it and music
plays. Type plain English while it plays - "calmer", "add vocals about
winning", "switch to piano" - and the stream follows. No cloud, no API
keys, no subscriptions, nothing leaves the machine.

It is one Go binary that manages everything else: it installs the music
model, supervises it, buffers songs ahead of playback, joins them with
crossfades, and keeps audio flowing when the generator hiccups. There is
a terminal interface and a phone-first web remote, so the machine can
sit in a cupboard and the radio can live in your pocket.

[![the live stream](assets/thumbs/remote-player.png)](assets/remote-player.png) [![the saved songs player](assets/thumbs/remote-saved.png)](assets/remote-saved.png) [![the words of the playing song](assets/thumbs/remote-lyrics.png)](assets/remote-lyrics.png)

[![settings](assets/thumbs/remote-settings.png)](assets/remote-settings.png) [![listener accounts](assets/thumbs/remote-users.png)](assets/remote-users.png) [![the login page](assets/thumbs/remote-login.png)](assets/remote-login.png)

## What it sounds like

Complete two-minute renders, one take each, the audio exactly as the
radio produced it:

| Listen | Preset | What it is |
| --- | --- | --- |
| [grind.mp3](samples/grind.mp3) | `grind` | energetic electronic rock, sung in English |
| [pop-punk.mp3](samples/pop-punk.mp3) | `pop-punk` | fast pop-punk with a singalong chorus, sung in English |
| [funk-soul.mp3](samples/funk-soul.mp3) | `funk-soul` | 70s funk and soul with horns, sung in French |
| [sunshine-pop.mp3](samples/sunshine-pop.mp3) | `sunshine-pop` | bright feel-good pop, sung in Spanish |
| [reggae-dub.mp3](samples/reggae-dub.mp3) | `reggae-dub` | sunny reggae with dub delays, sung in Japanese |
| [lofi-study.mp3](samples/lofi-study.mp3) | `lofi-study` | chill lofi hip hop beats, instrumental |
| [night-drive.mp3](samples/night-drive.mp3) | `night-drive` | neon 80s synthwave, instrumental |

Reproduce any of them with `iar export --minutes 2 --preset <name>` -
you will get a different song, because every generation is its own roll
of the dice. The non-English takes were rendered with that language as
the configured `languages` list; out of the box these presets sing in
English. A weak track is usually followed by a better one, and `skip`
is always there.

## Requirements

Infinite AI Radio is one static binary with no libraries to install
beside it. Almost everything below is about the **music engine** it
manages for you: that engine is a Python program the binary downloads,
installs into your home directory and supervises, and it is the part
that wants a graphics card and a few ordinary system tools.

**To run it**

- Linux with PipeWire or PulseAudio - playback goes through `pw-play` or
  `pacat`. No other platform is supported yet.
- An NVIDIA GPU with 8 GB or more of VRAM, and a reasonably current
  driver. Without one the music engine is impractically slow, though the
  noise modes (pink, white, brown) still work on any machine. The engine
  installs its own CUDA build of PyTorch, so there is no CUDA toolkit to
  set up yourself.
- About 20 GB of disk space for the engine and the model weights.
- `ffmpeg` (built with libmp3lame) and its `ffprobe`, plus `git`, `curl`
  and a C compiler. `iar setup` uses them once to fetch and build the
  engine's Python dependencies; the player keeps using `ffmpeg`
  afterwards for MP3 encoding. Everything else, including the engine's
  own Python, is installed into your home directory without sudo.

`iar doctor` names anything missing. On Debian and Ubuntu, `make deps`
installs the lot with `sudo apt-get install -y`; on other distributions
use your own package manager.

**To build it from source**

- Go 1.26 or newer, and nothing else. The presets, the phone remote's
  entire interface and the pronouncing dictionary are compiled into the
  binary. You can skip this by [downloading a release](#install).

**Optional**

- A local [Ollama](https://ollama.com) daemon. When present, the player
  uses it to refine steering and write lyrics; without it a built-in
  deterministic path takes over and everything still works.
- [Tailscale](https://tailscale.com), to reach the phone remote from
  outside the machine.

## Install

A static binary for `linux/amd64` and `linux/arm64` is published on the
[releases page](https://github.com/OWNER/REPO/releases), with a
`SHA256SUMS` file beside it:

```sh
tar xzf iar_1.0.0_linux_amd64.tar.gz
cd iar_1.0.0_linux_amd64
./iar setup      # one-time: install the music engine and download models (~18 GB)
./iar
```

Or from source:

```sh
make deps        # check system tools (Debian/Ubuntu: installs missing ones)
make build       # build ./iar
make setup       # same one-time engine and model install
make install     # optional: copy the binary to ~/.local/bin
```

`setup` is resumable - if the download is interrupted, run it again and
it continues where it stopped. It never uses sudo. The examples below use
`./iar`; once the binary is on your `PATH` it is just `iar`.

## Running

```sh
./iar
```

That is all. Setup banks a few starter tracks, so a launch begins playing
within seconds and crossfades to freshly generated music as soon as it is
ready. A bare `./iar` starts on the `nu-metal` preset; `default_preset`
in the [configuration](docs/configuration.md) picks a different one.

```sh
./iar "dark techno"          # start straight from a prompt
./iar --preset lofi-study    # start from a built-in preset
./iar --session gym-grind    # resume a saved session
./iar --engine noise         # noise only, no GPU needed
./iar --remote               # run as a station: remote on, local speakers muted
./iar --telemetry            # add a CPU, memory and graphics-card readout
./iar --plain                # line-based interface (used automatically in pipes)
./iar presets                # list the built-in presets
./iar doctor                 # check your environment
./iar engine stop            # stop the engine now and free GPU memory
```

Quit with `quit` or Ctrl+C. Only one player runs at a time; a second one
refuses to start and says so.

The engine is woken only when there is work: songs are planned and
rendered in batches to a disk buffer, and between batches the engine
shuts down completely, giving back all of its graphics and system memory
while playback continues from the buffer. A relaunch with a healthy
buffer plays immediately without touching the graphics card at all.

## Steering the music

While music plays, type what you want and press Enter:

```
make it more energetic
calmer
switch to piano
add vocals about winning
no vocals
generate pink noise
```

The player switches to the newly steered sound as soon as the first
track matching it is generated - with a warm engine that is typically
tens of seconds, not the end of the current track.

Steering has real semantics, not just word-appending:

- "less guitars" dials guitars down a notch without banishing them
  (repeat it to go further); "no drums" (or "without", "remove",
  "drop") removes the thing everywhere and tells the model to avoid it
  from then on; "more synths" (repeatable) raises the emphasis.
- Mood words replace their opposites: "calmer" also withdraws
  "energetic" if you asked for that earlier.
- "120 bpm", "faster", "slower", "in C minor", "3/4 time" and
  "vocals in Spanish" set tempo, key, meter and vocal language as hard
  constraints the engine honours directly.
- Repeating a request already in effect changes nothing (and says so)
  instead of churning the queue.

Steering accumulates: "calmer" then "no drums" gives you calm, drumless
music. `clear` wipes the accumulated steering and returns to the
session's base sound, and `new <prompt>` starts a fresh session from a
prompt.

### Lyrics

Vocal tracks get their words from a pluggable lyric writer; two are
built in:

- `scribe` (the default) plans each song before writing it: it pins the
  theme down to concrete images, gives every section its own job and its
  own rhyme sound, writes one section at a time, and checks every draft
  against a pronouncing dictionary - syllable counts, rhyme schemes,
  clichés, repetition budgets and theme coverage - sending corrections
  back for anything that fails. The dictionary machinery is
  English-only; in any other sung language scribe writes in one pass.
- `smoothbrain` is the original writer: one quick prompt, no plan, no
  revision. Kept selectable for comparison.

`lyrics` shows the active writer, `lyrics smoothbrain` switches, and the
choice is saved with the session. Lyric writing runs through Ollama;
without a daemon the engine's own planner invents words from the theme
instead. Songs are named from their own lyrics, so lock screens and
saved-song lists show a real title instead of a prompt fragment.

### Sung languages

Left alone, the engine sings in whatever language it feels like. Name
the languages you want and every song picks one of them at random:

```
languages English, Russian, French, Bisaya (Cebuano)
languages -Russian     # not in the mood for Russian right now
languages +Russian
languages none         # back to the engine's own choice
```

The list is saved in the configuration, so it survives restarts and
preset switches; the phone remote shows one switch per language on its
Live screen. Languages the engine has no tag for still work best-effort
(the words are written in that language and sung without a language
hint), and the `languages` listing says which ones those are. Steering
a language by hand ("sing in French") pins the session to it; editing
the list or flipping a switch releases the pin and drops the tracks
queued ahead.

### Commands

Commands work with or without a leading slash:

| Command | Effect |
| --- | --- |
| `clear` | wipe the steering context |
| `new <prompt>` | fresh session from a prompt |
| `save [prev] [tag]` | save the playing (or previous) track as MP3, filed under a tag |
| `name <name>` | save the current session under a name |
| `sessions` | list presets and saved sessions |
| `load <name>` | resume a saved session |
| `preset <name>` | switch to a built-in preset |
| `delete <name>` | delete a session or hide a preset (asks first) |
| `mp3 <minutes> [file]` | export minutes of the current vibe to MP3 |
| `skip` | jump to the next track |
| `loop` | repeat the playing track until toggled off |
| `pause` / `resume` | pause or continue output |
| `volume <0-100>` | set output volume |
| `lyrics [name]` | show or switch the lyric writer |
| `languages [list]` | show, set or switch the sung languages |
| `status` | engine, buffer and session status |
| `help` | list commands |
| `quit` | exit |

## Sessions and presets

Everything you type is persisted automatically; you never have to save.
`name gym-grind` gives the current session a name, and `./iar --session
gym-grind` (or `load gym-grind` inside) brings the same vibe back.
`sessions` lists your named sessions, the presets and the auto-saved
ones; sessions you never named are swept two days after they last
played (`sessions.auto_retention_days`), while named ones stay. Session
files are plain JSON in your data directory.

Twenty presets ship built in, grouped by energy - pick a feeling first
and steer the genre later. They behave like read-only sessions: starting
one seeds a fresh session you can steer and name.

| Group | Presets |
| --- | --- |
| high-energy | `grind` · `hard-rock` · `liquid-dnb` · `nu-metal` · `pop-punk` |
| upbeat | `chiptune` · `deep-house` · `funk-soul` · `sunshine-pop` |
| cruise | `boom-bap` · `epic-score` · `night-drive` · `reggae-dub` · `roadhouse-country` |
| chill | `chamber-strings` · `deep-focus` · `jazz-club` · `lofi-study` |
| sleep-noise | `pink-noise` · `sleep` |

`./iar presets` describes each one. A preset can be deleted like a
session; `./iar sessions restore-presets` brings them all back.

## Saving tracks and exporting MP3s

When a track lands just right, type `save` and it is written as a
high-quality MP3 into the snippets folder, with the song's title, genre
line and lyrics in its tags. `save prev` captures the previous track for
when it clicks a moment too late; `save gym` files it under a tag, and
tags become folders (`snippets/gym/`). Saving never interrupts playback,
and saving the same track twice is a friendly no-op.

`mp3 20` renders a ~20-minute MP3 of the current vibe in the background,
generating only while the playback buffer is full, so the stream never
stutters. Exports are capped at 180 minutes per run. The same thing
works headless:

```sh
./iar export --minutes 20 --preset sleep
./iar export --minutes 30 --session gym-grind --out ~/Music/grind.mp3
```

## Listening from your phone

The web remote is a phone-first page with the live stream, the
now-playing song and its words, the shared steering context, a station
picker, save buttons, and a player for the songs you have saved.
Everything needs a login, and the first account is created in the
terminal:

```sh
./iar remote setup   # create the first admin (email + password)
./iar --remote       # start as the station: remote on, local speakers at 0
```

With `--remote` the machine is the station, not the listening room -
type `volume 80` in its terminal to also hear it locally. The player
prints the URL to open; the stream is MP3 at ~192 kbps. Like everything
else it sits behind the login, so a non-browser player needs the session
cookie passed along to read `/stream.mp3`. The page can be installed as
an app from the browser menu ("Add to Home screen").

For safety the remote binds only to localhost and, when the machine has
one, its Tailscale address - never your LAN or the internet unless you
add addresses to `remote.bind`. The intended setup is a private
[Tailscale](https://tailscale.com) network:

1. Install Tailscale on the computer (needs sudo) and sign in.
2. Install the Tailscale app on the phone, same account.
3. `./iar remote setup`, then `./iar --remote`.
4. Open the printed `http://100.x.y.z:8246` URL on the phone, log in,
   tap play.

To also reach it on your home LAN, add the machine's address to
`remote.bind` (`"bind": ["0.0.0.0"]` listens everywhere the machine
is):

```json
{ "remote": { "enabled": true, "bind": ["192.168.1.20"] } }
```

Security, plainly:

- Logins happen over plain HTTP. Tailscale encrypts everything between
  the devices, so that is fine on the tailnet; on your own LAN it is
  your call. Never expose the port to the public internet without TLS
  in front of it - behind such a proxy the login cookie is marked
  secure automatically.
- A login lasts 30 days of inactivity (it is a radio). Changing or
  resetting a password logs every other device out.
- Failed logins are rate-limited per address and per account, and the
  page never reveals whether an email exists.
- Requests whose Host or Origin is not an address the remote listens on
  (or an entry in `remote.allowed_hosts`) are refused - a cross-site and
  DNS-rebinding defense.

### On the phone

The page defaults to **buffered playback**: it downloads whole upcoming
tracks and plays them back-to-back, so the music keeps going through
minutes of dead signal. How much is buffered is a per-device choice
under Settings, from one track ahead on metered connections to about 45
minutes for flights. In buffered mode the Next button skips only on that
device; other listeners and the machine keep their own position. A
Settings switch selects the direct live stream instead - the one
`/stream.mp3` serves - whose Next skips for everyone. The Loop button
works the same way: when a track is a keeper, tap it and the song
repeats until you tap again - on this device alone in buffered mode,
for the whole radio (speakers, stream and all) on the direct stream.
Skipping, steering or changing the session turns the loop off, since
each of those means "move on". If the
stream drops or stalls, the page reconnects on its own and picks up the
moment the stream is reachable again; only an expired login stops it.

Saving from the phone captures what *you* are hearing - in buffered mode
that is this device's track, which may trail the machine's speakers.

The page publishes media-session metadata, so lock screens, Bluetooth
displays and car interfaces show the song's title and genre line with
working play, pause and next buttons. Browsers refuse to autoplay on an
untouched page, so after a reload the next tap anywhere starts the
audio. Installing the page as a web app removes even that, though only
over HTTPS; on the plain `http://` tailnet address the one-tap start is
how it works.

Two car conveniences live under Settings, remembered per device:

- **The previous-track button saves the track.** An endless stream has
  no meaningful "previous", so that button - on the steering wheel,
  headset or car screen - doubles as save-what-I-am-hearing, confirmed
  by a "Saved:" flash in the title. It captures every previous-track
  input, a voice assistant's "previous song" included. In the
  saved-songs player, previous keeps its normal meaning.
- **Resume when the car reconnects.** When the car turns off, playback
  pauses and resumes by itself when the car asks to play again - never
  on a timer, so a phone in a pocket stays silent. Hands-free resume
  may need Android Auto's "Automatically resume media" setting, and
  after a long stop the phone's battery management can require one tap.

### Saved songs

The remote's **Saved** mode plays your saved songs entirely on the
phone; the live stream and the machine's speakers are untouched. Tick
the tags and sung languages you want and it loops through the checked
songs forever; uncheck a song to skip it without deleting anything, or
loop just one. Tapping a row opens the song's panel: full lyrics,
details, and buttons to download, rename, move or delete it.

On disk, a saved song's file is named after its title with the sung
language's tag before the extension
(`20260101-120000-tumutunaw-ang-selyo.tl.mp3`), and the full lyrics sit
next to the MP3 in a `.txt` with the same base name.

### Accounts and permissions

Every listener has an account; the email address is the login, and only
the account holder ever knows the password. Admins manage everything on
the **Users** page: adding a user produces a one-time invitation link
(expires in 7 days) to send by text or chat - the person opens it,
chooses a password, and is in. The link carries the address you created
it from, so make it from a browser on the same route the invitee will
use (the tailnet address for tailnet users, the LAN address for LAN
users). Four independent permission switches per
account: *admin* (manage users, delete sessions and saved songs, edit
the language list), *can steer*, *new prompts* (start prompts, load
sessions and presets), *can save*. Listening needs none of them. The
page only shows the controls an account may use, and the server refuses
the rest either way.

Password resets are one-time links an admin hands out (24 hours, the
user picks the new password). You cannot delete your own account, and
the last admin can neither be deleted nor demoted; a locked-out sole
admin recovers with `./iar remote setup` in the terminal. Accounts live
in owner-only JSON files under the data directory.

Optionally, point `remote.smtp` at any mail provider and invitation and
reset links are emailed as well; `./iar remote test-email
you@example.com` checks the setup. With no email configured you pass
links on yourself, and nothing is missing.

```json
{ "remote": { "smtp": {
    "host": "smtp-relay.example.com", "port": 587, "tls": "starttls",
    "username": "your-login", "password": "your-smtp-key",
    "from": "radio@example.com" } } }
```

## Desktop integration

Infinite AI Radio appears as a regular MPRIS media player, so media
keys, KDE's media controls, KDE Connect and `playerctl` all work:

```sh
playerctl --player iar play-pause
playerctl --player iar metadata xesam:title
```

## Configuration

Optional. The player reads a JSON config file - path shown by
`iar doctor`, usually `~/.config/iar/config.json` - covering the audio
pipeline, the buffer depths, the phone remote, the engine and Ollama.
Data lives under XDG paths (`~/.local/share/iar` by default); the
`IAR_DATA_DIR` and `IAR_CONFIG_DIR` environment variables relocate
everything. [docs/configuration.md](docs/configuration.md) documents
every setting and its default.

## Models and licensing

The code is MIT licensed. Model weights are never bundled; they are
downloaded on demand from their publishers, and the default model needs
no account, token or license click-through.

| Model | Used for | Weights license |
| --- | --- | --- |
| [ACE-Step 1.5](https://huggingface.co/ACE-Step/Ace-Step1.5) (default) | music generation | MIT |
| [ACE-Step 5Hz LM 0.6B](https://huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B) | planning and lyrics inside the engine | MIT |

The ACE-Step model card states that generated music may be used
commercially. If you point the optional Ollama integration at a model of
your choice, that model's own license applies to it.
[docs/models.md](docs/models.md) has the detail, including the word data
embedded in the binary.

## Troubleshooting

`iar doctor` checks system tools, the GPU, the engine install and
whether the engine is responding, and prints the path to the structured
log, which has the full story including the engine's own output.

- **"music engine not installed"** - run `./iar setup`.
- **No sound** - confirm `pw-play` or `pacat` plays something, then try
  `./iar --player pipe`.
- **The first track takes a while** - the initial model load is a few
  minutes; later launches skip it while the disk buffer is healthy.
- **Buzz or static while a track generates** - if the underrun counter
  (in the header and in `iar doctor`) stays at zero, the audio stream
  itself is clean and the noise is electrical interference induced
  after the digital output.
- **Generation failures** - the stream degrades gracefully (buffer,
  then looping the last track, then a noise bed) and the engine
  restarts itself; expect at most a couple of minutes of looped music.
  The interface and `iar doctor` show the failure streak and the last
  reason.

Known limitations: vocal lyrics are short and chorus-driven (this is a
background-music tool, not a songwriting studio), and the phone remote
has no self-service password reset - an admin hands out reset links.

## Development

```sh
make help          # list targets
make test          # unit tests (silent; no audio devices touched)
make lint          # go vet + gofmt check
make smoke         # end-to-end test of the built binary (sandboxed, silent)
make browser-test  # the phone remote in headless Firefox
make screenshots   # re-shoot the screenshots above
make release       # build the release tarballs and checksums into dist/
```

The test suite, smoke test and browser test never make a sound: they use
the null or file audio backends, sandboxed data directories, and a
temporary null audio sink for the browser. See
[CONTRIBUTING.md](CONTRIBUTING.md) to get started.

## License

MIT - see [LICENSE](LICENSE). The pronouncing dictionary embedded in the
binary carries its own BSD-style notice; see
[docs/models.md](docs/models.md).
