# Changelog

Notable changes to Infinite AI Radio. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.3.0] - 2026-09-11

### Added

- A position row for the **Saved** screen: the clock, a slider and the
  song's length. A saved song is a whole file - this device's own copy,
  or the radio's - so it rewinds wherever you drag the slider, which
  the direct live stream never could.

### Changed

- The **Saved** screen leads with the song playing on the device, laid
  out the way the live page lays out its own: the name at the top of
  the screen, its genre, language and tag on the line under it. The tag
  switches and the song list follow underneath.
- The position row - on both screens - moved down onto the player bar,
  above the buttons, so it is under your thumb wherever the page is
  scrolled to. Finding a moment in a saved song is now a matter of
  playing it from the list and dragging, not of scrolling to the top to
  drag and back down to find your place in the list. The buttons gave
  up a little height to make the room, so the bar stands about as tall
  as before.
- The loop moved out of the song rows and into the transport bar,
  beside Next - the slot the live bar gives its own loop - and says it
  is on by wearing an accent ring, rather than by turning one small
  icon in a list row a shade of colour. **Back to looping the checked
  songs** went with the row icons: the loop button is its own way off,
  and so are Prev and Next.
- A saved song that will not play says so under its name rather than
  replacing the name with the complaint, and gives the song's facts
  back once the trouble has been read.

### Fixed

- The car's save button works again from a locked phone on the live
  stream. The page had started naming the track to save, and a phone
  with its screen off stops asking the radio what is playing - so the
  name it sent was minutes old and the radio answered "that track is no
  longer available to save" while the song in the listener's ears sat
  there perfectly saveable. On the live stream the radio works out what
  is playing for itself again, which is never stale. A device playing
  from its own bank still names its own track, because the radio cannot
  know which one that is, and a save queued behind a dead patch still
  names the song the listener meant.
- The quiet lines on the player - the position clock either side of the
  slider, the station and queue line, and the device's bank count - take
  their colour from the palette again. They had been stuck on one fixed
  grey that never changed with the theme and was too faint to read
  comfortably in daylight.

## [1.2.0] - 2026-09-09

### Added

- Saved songs are banked on the device the way the live stream's are,
  to the same per-device buffering level, so a saved song starts on the
  tap instead of loading first and the next one is ready before it is
  needed. The line under the saved player counts what is ready.
- **Preload songs for the inactive mode**, off by default: a few saved
  songs kept ready while the live stream plays, and a few live songs
  while the saved ones do, so switching between the two away from a
  good signal plays straight away.
- A standby button on the app bar, beside the settings gear: holding
  the radio no longer means opening Settings and scrolling. The switch
  in Settings is still there and the two follow each other.
- The running build is shown where Settings opens, printed at startup
  (`iar: Infinite AI Radio <version>`) and repeated by `status`, so a
  page left open across a rebuild can be told from a fresh one.
- `[X]` in front of the song's name on lock screens and car displays
  while the radio cannot be reached, so a phone playing out of its own
  bank is told apart from one the radio is still feeding.
- A save that cannot reach the radio is queued on the device and goes
  through by itself when the signal comes back - for the dead patches
  of a drive, not an afternoon offline; past a quarter of an hour the
  phone says it has given up.
- `restart` in the terminal and **Empty the buffer and start over**
  under Settings on the phone: throw away every song made ahead and
  start generating again from the first rung of the batch ladder, with
  the session and its steering kept. Nothing goes quiet - the song in
  the speakers plays on until the first fresh one is ready.

### Changed

- Next on a buffered device now means *not this one*: the skipped song
  is deleted from the device and never downloaded or played again. With
  nothing else ready the trouble beeps sound and the device waits for
  the radio, instead of starting the song just rejected over from the
  top.
- The saved confirmation stays put. The song's name carries **Saving:**
  from the moment a save is asked for until the radio has it, then
  **Saved:** for as long as that song plays - on the page and on the car
  screen. It used to be a two-second flash.
- Each saved song's row button turns into a pause while that song is
  playing, and pressing it pauses the player.
- The buffering level is no longer hidden behind buffered playback: it
  governs the saved songs whichever transport the live stream uses.

### Fixed

- A control pressed with no signal answers in seconds instead of
  sitting on the browser's own connect timeout, and says the radio is
  out of reach rather than blaming what was asked for. Waking a radio
  from a dead zone was the worst case: the button simply spun.
- A song whose local copy would not start left the page reporting
  "playing (buffered)" over silence, because the next status repaint
  read the song that had been picked rather than the element that was
  supposed to be making sound. The device now says so, moves to the
  next song, and stops after three refusals in a row rather than
  working through the whole bank.
- Pressing play primes the audio elements inside the tap, the way the
  tap-anywhere path already did. Without it, the wait to open the
  store and ask the radio what is coming could outlive the gesture
  that authorised the sound - which is a slow connection, exactly
  where it hurts.
- `standby` in the terminal held the radio as the help said it did.
  Typed, it steered the music with the word instead.
- The trouble cue is six beeps, and Settings and the README now say six
  rather than three.

## [1.1.1] - 2026-09-05

### Fixed

- An answer of "no language in particular" survives a reload instead of
  coming back as the preset's language.
- A session saved before the languages belonged to sessions keeps
  singing what it was singing.

## [1.1.0] - 2026-09-05

### Added

- The radio can be held on standby: nothing plays and nothing is
  generated until it is woken.
- A song can be renamed from the page that is playing it, and the new
  name follows it into every copy that outlives playback.

### Changed

- A song is named where its words are written, and nothing renames it
  afterwards.
- The languages a session sings in belong to the session rather than to
  the machine's configuration.
- A restart resumes the sound that was playing, and changing it
  branches the session rather than overwriting it.

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

[Unreleased]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/zoffixznet/infinite-ai-radio/releases/tag/v1.0.0
