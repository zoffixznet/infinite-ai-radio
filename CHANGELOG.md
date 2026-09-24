# Changelog

Notable changes to Infinite AI Radio. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- The phone plays only from its own bank now. The radio's store is
  listed to it whole, in the order the songs were made and with the
  ones other listeners have taken marked as such; the phone takes the
  songs it lacks in that order - a fresh phone starts with what the
  others have already heard - and taking a song is what tells the
  radio to make more. Each song's words come from the song's own route
  rather than riding on every listing. Next reports the music skipped
  to the radio, which brings its next batch forward, and the list of
  skipped songs is tied to the sound rather than to a clock: a steer
  empties it. A skipped-past song the radio has let go is saved from
  the phone's own copy, which the radio checks against the record it
  kept before keeping it.
- `play` and `stop` switch this machine's own player on and off:
  stopped, nothing comes out of its speakers and it takes nothing from
  the store, which goes on filling for the phones as before; `play`
  picks up from the song it was on. `--remote` starts with the player
  off instead of at volume 0 (which still consumed songs).
- A change to the sound branches the session only if the session has
  made a song, and a burst of changes within 30 seconds of each other
  makes one branch rather than one per tap. Branching used to wait for
  a song to come out of this machine's speakers, which a station whose
  player is off never has.
- The batch ladder climbs on the clock instead of on songs played
  through the speakers. After a rung is made the generator waits as
  long as that rung's music runs before making the next, less what
  listeners skipped - every phone reports the seconds it skipped with
  Next on its checks for new songs, and so does the terminal's `skip`.
  A cycle runs only while the store holds fewer than `buffer.songs`
  untaken songs, and a batch never makes more than the room left, so a
  full store is the off switch and the engine sleeps until somebody
  takes a song. Engine-invented words are allowed for the opener only;
  every later song waits for the writer. `buffer.render_low_minutes`
  is gone.
- Playing a song no longer deletes it from the store. The player takes
  it, which marks it consumed and leaves it on disk for a player that
  has not caught up, and the store keeps the newest `buffer.songs` (72
  by default) taken songs, trimming the oldest beyond that. The songs
  nobody has taken yet are what the generator fills against. The
  player's place in the store is remembered, so a restart continues
  after the last song it took rather than replaying the kept ones. A
  phone that was away is offered the kept songs first. The store keeps
  its listing in memory now, instead of reading every song's sidecar on
  every poll.
- A song goes out to the phone as the very file the radio rendered,
  byte for byte, instead of being encoded again on the way out; and
  saving a song still on disk copies that file with fresh tags rather
  than encoding it a second time, so the saved song is exactly the one
  the phone played.
- The radio keeps a book of every song it makes - `songbook.jsonl` in
  its data directory - with the file's hash, the song's name, words and
  prompt, and where it was saved. A song's saved mark and a listener's
  rename now outlive a restart, and a copy of a song the radio no
  longer holds can be recognised by its hash and saved under the
  recorded name.
- `iar buffer clear` refuses while the radio is running. The running
  radio keeps the store's listing in memory, so emptying the store
  behind its back left it listing songs that were gone; `restart` in
  the radio, or the phone's "Empty the buffer and start over", empties
  a running radio's store instead.
- The phone says how much music is on the device in hours and minutes,
  the way the machine's own readout says it. "~189 min banked" is
  arithmetic nobody should have to do to answer "how long can I drive
  on this"; it reads "~3h09m" now.
- Writing the words is the engine working. While a batch's words are
  being written the `gen` row reads "writing song words (4 of 10)",
  the status line and the phone read "writing song words" instead of
  "starting engine", and the `cpu`, `ram` and `vram` figures marked
  `radio` count the writer's processes along with the engine's - the
  card's memory is no longer listed as "shared" while the radio's own
  writer holds it. The `models` row names what is on the card for the
  radio, the writer or the engine's models, and says "engine asleep"
  only when nothing is being made at all, with the reason: the store
  is full, or the next batch is not due yet. While the engine starts
  up for a batch the `gen` row reads "waking the engine for the next
  batch" rather than "engine asleep" beside a status line that says
  the models are loading. The writer answering the radio's health
  check at startup, or a steer, is not the words being written: with
  nothing to make, every row says so. The log reads "render model
  unloaded while the words are written" when the engine hands the
  card to the writer, "stopping engine daemon" for the stop itself,
  and "engine asleep" only when it really is - "resting after
  failures" when that is why.

### Removed

- The live stream and the phone's direct mode: `/stream.mp3`, the
  "Buffered playback" switch (there is nothing else now), the
  reconnecting logic that went with the stream, and the radio-wide
  Next and Loop the phone drove in direct mode (`/next`, `/loop`).
  Next and Loop are the phone's own, as they were in buffered mode;
  the terminal keeps `skip` and `loop` for the machine's own player.
  `/state` no longer describes what the machine is playing.
- Standby: the power button and banner on the phone, the `standby`
  command and its hold across restarts. A full store is the off
  switch now - the engine sleeps on its own once nobody is taking
  songs - and `stop` covers this machine's speakers.
- The track library, its `library_max_mb` setting, the starter tracks
  `iar setup` banked (`--no-bank` with them) and the instant start from
  a banked song. The store keeps played songs now, so saving and
  renaming reach a song the radio has moved past without a second copy
  of it, and a restart plays from the store at once. `library/` in the
  data directory can be deleted.
- Noise mode: the `pink-noise` preset, "generate brown noise" steering,
  the noise bed under a starting radio and the `bed_while_waiting`
  setting, and the `noise` engine. A machine without a graphics card
  runs `--engine tone` instead, which makes short tone songs through
  the same store, ladder, phone and exports as the real engine; a
  configuration still naming the `noise` engine is read as `tone`. A
  saved session that was a noise session plays the default sound.
- `buffer.phased` and `buffer_tracks`. Every engine now makes its songs
  into the store; the old path that queued songs in memory is gone.
  Both keys can be deleted from the configuration.

### Fixed

- A save that reaches the radio while another is still being written
  waits its turn behind it instead of being refused. A phone back in
  reach after a dead patch delivers the saves it queued there one
  after another, and the second used to arrive mid-write and be told
  to try again in a moment - which the phone took as a final answer,
  so that song was never saved. Every song a phone saved while out of
  reach is delivered now, once, in the order it was asked for; a song
  whose save is already on its way is told so rather than saved twice;
  and two songs of one name saved in the same second get two files.
  A save waiting its turn holds on to its song, so a steer, a restart
  or the store trimming meanwhile no longer loses it; a save that
  reaches the radio as it shuts down stays queued on the phone and
  goes through once the radio is back, rather than being reported as
  failed; and a queued save whose answer arrives after a poll has
  already seen the song saved settles that song alone, where it used
  to take the next queued song with it.
  The phone also gives up on a save post nothing answers - packets
  dropped in a dead zone - after the same short deadline as its other
  controls, and queues it, instead of holding the queue on the
  browser's own minutes-long timeout.
- A song's copy sent from the phone over a slow connection is given
  the time it needs: the transfer is given up only when it stops
  moving, not at a fixed deadline that a slow link would miss at the
  same point every time, sending the whole copy again and again and
  never saving it.
- A song is one song from the moment it is made. It used to be given a
  new name at each stop of its life - one while it waited on disk,
  another when it joined the play queue, a third when it went into the
  library - and a phone, which keeps its songs by name, downloaded the
  same song two and three times over. A phone that had just been
  flushed would count 34 songs on a radio holding 18, spend a download
  on every copy, and could play a song it had already played.
- A song the radio has already played can still be saved from the
  phone that is playing it. Saving only ever looked at the song playing
  and the one before, so a phone playing its own copy well behind the
  speakers was told the song was "no longer available" while its audio
  sat on disk. It is found in the store now, after a restart too, and a
  save queued in a dead zone waits a day for the radio to come back
  instead of giving up after a quarter of an hour.
- A song can be saved or renamed while the radio is fading into it.
  For the few seconds of the crossfade it had left the play queue but
  was not yet the song playing, and was in no list the radio looked in
  - so a phone running ahead of the speakers, playing exactly that song,
  was told it was "no longer here". A skip opens that window at the
  moment a listener is most likely to be reaching for the pencil.

## [1.3.2] - 2026-09-18

### Fixed

- Standby stops what the radio is *doing*, not only what it would start
  next. The hold was checked once, on the way into a generation cycle,
  so a cycle already under way ran to the end regardless: on one
  machine that meant twenty minutes of writing song words after the
  button was pressed, and then the graphics card woken back up to
  render the whole batch - the exact spend the button exists to
  prevent. It now stops at the next clean seam, in the writer and
  between songs alike, and hands the card back.
- Nothing is thrown away to stop there. The sheet, plan or song in
  flight finishes and is banked; coming back counts what is on the
  shelf and on disk and writes only the remainder, so a deep batch is
  covered across as many holds as it takes.
- Instrumental sessions get a description written for each song of a
  batch, which is what they were always supposed to get. The phase that
  writes them turned every non-vocal session away three lines above the
  branch written to serve it, so it had never once run: every track
  went out under the same terse steering caption. It cost graphics card
  time as well as variety - with nothing ever written, planning refused
  on every cycle, so the radio woke the engine, planned nothing and
  hibernated again, once a minute for as long as the buffer stayed
  healthy.
- The resource readout names what else is holding the graphics card.
  The radio's own share is worked out through the engine daemon's
  ancestry, and the lyric helper runs as a service of its own - so
  while it held the card the only thing the screen could say was that
  the radio was holding nothing, which is true of the engine and no
  answer at all to "what is using my graphics card". Another program
  sharing the machine had the same problem. Both are named now, by
  process, and neither is ever folded into the radio's own figure.
- A held radio that had left the engine warm now hibernates it.
  Hibernation only ever happened on the way out of a cycle, so a cycle
  that ended staying warm and then met a hold left the daemon holding
  the graphics card with nothing left to run that would put it down.

## [1.3.1] - 2026-09-16

### Fixed

- A phone with songs banked on it plays them the moment you press play,
  rather than waiting on the radio first. Coming back to the page after
  a while - a tab left open on a signal that has gone stale, a radio
  woken from standby - meant sitting in front of *waiting for the radio
  to send a song* while the line underneath counted fifty songs already
  on the device. It plays what it holds and asks what is next
  afterwards, and on a link that is up but carrying nothing it no
  longer waits at all: the request for the listing has a deadline now,
  and a listing that misses it counts as being off the network.
- Banked songs the radio has since played past are no longer thrown
  away when you reopen the page. Hours banked for a flight survived
  being carried around and did not survive the tab being reloaded.
  Steering the radio somewhere new still retires them, which is the
  point at which you have said you want something else.
- The status line stops blaming the radio for a silence that is not its
  doing. A device holding nothing it is allowed to play says that; a
  device that has given up on songs that will not start keeps saying
  how to try again, instead of having that wiped two seconds later; and
  a device playing its own bank out of reach of the radio keeps
  reporting what it is doing rather than freezing on whatever the last
  answered poll left behind. The count of songs ready to play counts
  everything on the device, not only the part the radio's listing still
  names.
- A song arriving at a crawl no longer holds the one download slot
  indefinitely, on the live bank or the saved one: the ninety-second
  deadline covers the whole song rather than only the radio's first
  byte. Two downloads can no longer run at once after steering.

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

[Unreleased]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.3.2...HEAD
[1.3.2]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.3.1...v1.3.2
[1.3.1]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.3.0...v1.3.1
[1.3.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/zoffixznet/infinite-ai-radio/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/zoffixznet/infinite-ai-radio/releases/tag/v1.0.0
