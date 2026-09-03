# Changelog

Notable changes to Infinite AI Radio. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2026-09-03

First public release. Everything below is what you get on day one.

### The stream

- Endless AI-generated music from a model running entirely on your own
  machine. No cloud, no API keys, no account.
- Plain-English steering while the music plays ("calmer", "no drums",
  "add vocals about winning", "120 bpm", "in C minor"), with real
  semantics rather than word-appending: opposites cancel, emphasis is
  repeatable, and hard constraints reach the engine directly.
- Phased generation: songs are planned and rendered in batches to an
  on-disk buffer, and the engine shuts down completely between batches,
  giving back all of its graphics and system memory while playback
  continues.
- Twenty-one built-in presets grouped by energy, sessions that persist
  automatically, equal-power crossfades, and loudness normalization.
- Noise modes (pink, white, brown) that need no GPU at all.

### Words and voices

- `scribe`, the default lyric writer: it plans a song before writing it,
  gives every section its own job and rhyme sound, and checks each draft
  against a pronouncing dictionary for syllable counts, rhyme schemes,
  clichés and theme coverage.
- Songs are named from their own lyrics rather than from the prompt.
- A configurable list of sung languages, chosen per song at random, with
  per-language switches on the phone remote.

### Listening elsewhere

- A phone-first web remote with the live stream, the shared steering
  context, a station picker, and a player for the songs you have saved.
- Buffered playback that survives minutes of dead signal, media-session
  metadata for lock screens and car displays, and two car conveniences
  (save from the previous-track button, resume when the car reconnects).
- A seek bar for buffered songs, a loop button that repeats the track
  you flag (the whole radio on the live stream, this device alone in
  buffered playback), buffering depth chosen in minutes, and a line
  showing what is banked on the device with a Flush button back to the
  live edge.
- Accounts with four independent permissions, invitation and reset links,
  and optional SMTP delivery for them.
- MPRIS desktop integration: media keys, `playerctl`, KDE Connect.

### Getting it

- Saving tracks you like as tagged MP3s, and MP3 export of any length.
- `iar doctor` for environment checks, and `iar --telemetry` for a live
  look at system memory, graphics memory and what is on the card.
- Published as a static Linux binary for amd64 and arm64, with every
  asset embedded.

[Unreleased]: https://github.com/OWNER/REPO/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/OWNER/REPO/releases/tag/v1.0.0
