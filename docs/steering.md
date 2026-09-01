# Steering, lyrics and commands

How to tell the radio what you want, what the words are made of, and
the full list of commands. See the [README](../README.md) to get set up.

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

Each input is acknowledged with how it was understood, and the player
switches to the newly steered sound as soon as the first track matching
it is generated; the old track never plays to its end after a steer,
and the acknowledgment estimates when the switch will be audible.

Steering has real semantics, not just word-appending:

- "less guitars" dials guitars down a notch without banishing them
  (repeat it to go further); "no drums" (or "without", "remove",
  "drop") removes the thing everywhere and tells the model to avoid it
  from then on; "more synths" (repeatable) raises the emphasis. The
  acknowledgments match: "dialing back guitars" versus "avoiding
  drums".
- Mood words replace their opposites: "calmer" also withdraws
  "energetic" if you asked for that earlier.
- "120 bpm", "faster", "slower", "in C minor", "3/4 time" and
  "vocals in Spanish" set tempo, key, meter and vocal language as hard
  constraints the engine honours directly.
- Repeating a request that is already in effect changes nothing (and
  says so) instead of churning the queue.

Steering accumulates: "calmer" then "no drums" gives you calm, drumless
music. `clear` wipes the accumulated steering and returns to the session's
base sound.

### Lyric writers

Vocal tracks get their words from a pluggable lyric writer; two are
built in:

- `scribe` (the default) plans each song before writing it: it pins
  the theme down to concrete images, gives every section its own job
  and its own rhyme sound, writes one section at a time, and checks
  every draft against a pronouncing dictionary - syllable counts,
  rhyme schemes, clichés, repetition budgets and theme coverage -
  sending specific corrections back for anything that fails. The
  chorus repeats because the song is assembled that way; nothing else
  gets to. The dictionary machinery is English-only: for any other
  sung language scribe writes in one pass, in that language.
- `smoothbrain` is the original writer: one quick prompt, no plan, no
  revision. Kept selectable for comparison.

`lyrics` shows the active writer and the options; `lyrics smoothbrain`
switches (the phone remote has the same switch under Settings). The
choice is saved with the session, and `lyrics_generator` in the
[configuration](configuration.md) sets the default for new
sessions. Lyric writing runs through Ollama; without a daemon the
engine's own planner invents lyrics from the theme instead.

### Sung languages

Left alone, the music engine sings in whatever language it feels like,
which is fun until it is not. Name the languages you want and every
song picks one of them at random, so the same language can come up
twice in a row:

```
languages English, Russian, French, Bisaya (Cebuano)
languages              # what is configured, and what is switched on
languages -Russian     # not in the mood for Russian right now
languages +Russian
languages none         # back to the engine's own choice
```

The list is saved as `vocal_languages` in the
[configuration](configuration.md), so it survives restarts and
preset switches. The phone remote shows one switch per language on its
Live screen, so narrowing the mix down to English is a single tap, and
edits the list itself under Settings.

The engine publishes a list of about fifty language tags, but it never
checks a request against it, and the model behind it knows more
languages than the list names. Cebuano is one of them: it is asked for
by its own tag and the words come back in Cebuano, even though the
published list has no Philippine language but Tagalog. Asking for
Tagalog instead would get Tagalog words - a different language, not a
Cebuano accent.

A language the engine has no tag for at all still works: the words are
written in it and sung with no language hint. That is best-effort
rather than a guarantee, and both the `languages` listing and the phone
remote say which languages are in that situation.

The tag matters more than it looks. On tracks where the engine writes
the words itself, it is the tag that decides which language they are
written in, so a wrong tag does not produce an accent - it produces a
different language.

Steering a language by hand ("sing in French") pins the session to it
and overrides the list; a preset that names a language of its own does
not. Editing the list or flipping a switch releases that pin, drops the
tracks queued ahead and starts generating in the new languages straight
away.

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
| `delete <name>` | delete a session or hide a preset (asks for confirmation) |
| `mp3 <minutes> [file]` | export minutes of the current vibe to MP3 |
| `skip` | jump to the next track |
| `pause` / `resume` | pause or continue output |
| `volume <0-100>` | set output volume |
| `lyrics [name]` | show or switch the lyric writer for vocal tracks |
| `languages [list]` | show, set or switch the languages vocals are sung in |
| `status` | engine, buffer and session status |
| `help` | list commands |
| `quit` | exit |

Longer output than fits on screen starts at its top with a "more below"
marker; PgUp/PgDn scroll the message backlog and End (or Esc) returns to
the live view.
