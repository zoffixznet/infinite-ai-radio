# Sessions, presets and keeping what you like

Everything you type is remembered, any track can be kept, and any
amount of a session can be rendered to a file. See the
[README](../README.md) to get set up.

## Sessions

Everything you type is persisted automatically; you never have to save.
Name the current session to make it easy to find again:

```
name gym-grind
```

Later, get the same vibe back with `./iar --session gym-grind` or `load
gym-grind` inside the app.

`./iar sessions` (or `sessions` inside the app) lists everything you can
start in three groups: your named sessions (most recently played
first), the built-in presets, and auto-saved sessions (newest first),
each with a short description and when it last played:

```
your sessions:
  gym-grind                energetic electronic rock, driving...  vocals   2h ago
presets:
  high-energy:
    grind                  Energetic electronic rock with motivation...
    nu-metal               Heavy nu-metal: down-tuned riffs, rap-sun...
  ...
auto-saved sessions:
  session-20260101-120000  lofi chill beats, mellow, warm analog... +2 tweaks  3d ago
```

Sessions you never named are removed automatically two days after they
last played (`sessions.auto_retention_days` in the config; `0` keeps
them forever). Named sessions, presets and the session that is playing
are never removed this way.

Delete a session, with a confirmation question:

```sh
./iar sessions delete gym-grind         # asks: Delete session gym-grind? [y/N]
./iar sessions delete gym-grind --yes   # no question, for scripts
```

Inside the app, `delete gym-grind` asks on the next line; `y` confirms,
anything else cancels. The session that is playing cannot be deleted.
Presets can be deleted as well: the preset disappears from every
listing and can no longer be started until `./iar sessions
restore-presets` brings all of them back. Session files are plain JSON
in your data directory.

## Presets

Twenty curated starting points ship built in, grouped by energy so you
can pick a feeling first and steer the genre later. They behave like
read-only sessions: starting from one seeds a fresh session you can
steer and name.

| Group | Preset | Sound |
| --- | --- | --- |
| high-energy | `grind` | energetic electronic rock with motivational vocals |
| | `hard-rock` | crunchy riff-driven arena hard rock (vocals) |
| | `liquid-dnb` | fast, uplifting liquid drum and bass |
| | `nu-metal` | heavy nu-metal: down-tuned riffs, rap-sung vocals |
| | `pop-punk` | fast, fun pop-punk with singalong choruses (vocals) |
| upbeat | `chiptune` | playful 8-bit video game energy |
| | `deep-house` | warm groovy deep house |
| | `funk-soul` | joyful 70s funk and soul with horns (vocals) |
| | `sunshine-pop` | bright feel-good pop with catchy hooks (vocals) |
| cruise | `boom-bap` | dusty 90s boom-bap hip-hop beats |
| | `epic-score` | heroic cinematic orchestra with choir |
| | `night-drive` | neon 80s synthwave for driving |
| | `reggae-dub` | sunny reggae with spacious dub delays (vocals) |
| | `roadhouse-country` | warm country rock for the open road (vocals) |
| chill | `chamber-strings` | elegant classical string quartet |
| | `deep-focus` | beatless ambient pads for deep concentration |
| | `jazz-club` | late-night jazz combo, warm and relaxed |
| | `lofi-study` | chill lofi hip hop beats for studying and working |
| sleep-noise | `pink-noise` | steady pink noise, no music |
| | `sleep` | slow beatless drones for falling asleep |

`./iar presets` lists them in the terminal. Select at launch
(`./iar --preset night-drive`) or inside the app (`preset night-drive`);
on the phone the picker shows the same groups, with the playing group
open.

## Saving tracks you like

When a track lands just right, type:

```
save
```

and the currently playing track is written as a high-quality MP3 into
the snippets folder, path shown in the acknowledgment. Its tags carry
the track's short title, a genre/mood line and (for vocal tracks) the
lyrics. `save prev` captures the previous track instead, for when it
clicks a moment too late. Saving never interrupts playback, and saving
the same track twice is a friendly no-op.

Add a tag to file the track where you will find it again:

```
save gym
save prev late night
```

Tags become folder names (`snippets/gym/`, `snippets/late_night/`;
anything that is not a letter, digit or underscore turns into an
underscore, and the whole tag is lowercased, so `Gym` and `gym` are the
same folder) and are also written to the MP3's album tag. Saves without
a tag go to `snippets/untagged/`, and tracks saved before tags existed
are moved there the next time the player starts. The phone remote's saved-songs player
loops these folders by tag (see [Saved songs and tags](remote.md#saved-songs-and-tags)).
The base folder is configurable via `snippets_dir`.

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
