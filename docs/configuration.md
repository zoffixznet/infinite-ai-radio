# Configuration reference

Infinite AI Radio works with no configuration at all. When you want to tune it, create a
JSON file at the path shown by `iar doctor` (usually
`~/.config/iar/config.json`). Only include the keys you want to change;
everything else keeps its default.

```json
{
  "engine": "acestep",
  "player": "auto",
  "track_seconds": 150,
  "crossfade_seconds": 3,
  "buffer_tracks": 6,
  "volume": 80,
  "bed_while_waiting": false,
  "pipe_latency_ms": 200,
  "normalize_loudness": true,
  "mp3_quality": 0,
  "snippets_dir": "",
  "library_max_mb": 600,
  "lyrics_generator": "scribe",
  "vocal_languages": [],
  "default_preset": "nu-metal",
  "remote": {
    "enabled": false,
    "port": 8246,
    "bind": [],
    "allowed_hosts": [],
    "smtp": {
      "host": "",
      "port": 0,
      "username": "",
      "password": "",
      "from": "",
      "tls": "starttls"
    }
  },
  "acestep": {
    "port": 0,
    "idle_minutes": 15,
    "lm_model_path": "",
    "lm_backend": "auto",
    "inference_steps": 12,
    "thinking": true,
    "offload_dit": false,
    "max_track_seconds": 300,
    "repo_url": "https://github.com/ace-step/ACE-Step-1.5",
    "tag": "v0.1.8"
  },
  "ollama": {
    "enabled": true,
    "gpu_layers": 0,
    "url": "http://127.0.0.1:11434",
    "model": ""
  },
  "sessions": {
    "auto_retention_days": 2
  }
}
```

## Top level

- `engine`: `"acestep"` (AI music, default) or `"noise"` (pure noise
  synthesis, no GPU needed). The `--engine` flag overrides per run.
- `player`: `"auto"` picks the best available backend (currently the
  pw-play/pacat pipe). `"pipe"`, `"null"` (silent, realtime-paced) and
  `"file"` (raw PCM to a file, used with `--player-file`) are mostly for
  scripting and tests. The `--player` flag overrides per run.
- `track_seconds` (30-300): length of each generated track. Longer tracks
  mean fewer transitions but steering tweaks take longer to arrive.
- `crossfade_seconds` (0.5-10): equal-power crossfade between tracks.
- `buffer_tracks` (1-8): how many finished tracks to keep generated ahead
  of playback. The queue also feeds phone listeners who prefetch
  upcoming tracks to ride out signal dead zones, so the default is a
  deeper 6. Higher survives longer engine stalls and deeper dead zones,
  uses more memory (about 28 MB per 150-second track).
- `volume` (0-100): initial output volume. Starting the player with the
  `--remote` flag overrides this to 0 - a machine started as the
  station should not blast music into its own room. Turn the local
  speakers up any time with the `volume` command; remote listeners
  always get the full-level stream regardless (it is tapped before the
  volume control). Enabling the remote through the config file alone
  does not silence anything.
- `bed_while_waiting`: when true, a quiet noise bed plays while the
  first track is prepared instead of the default silence-with-progress.
- `pipe_latency_ms` (20-2000): how much buffering the system audio
  player is asked for. Larger values ride out heavy system load at the
  cost of a slightly slower response to volume/pause.
- `normalize_loudness`: level every generated track to a consistent
  loudness (peak-safe) before playback and banking.
- `mp3_quality` (0-9): libmp3lame VBR quality for exports and snippets;
  0 is best (the default), 9 is smallest.
- `snippets_dir`: where the `save` command writes captured tracks, one
  subdirectory per tag (`untagged/` for saves without one). Empty means
  `snippets/` under the data directory.
- `library_max_mb`: total size cap for the on-disk track library that
  powers instant starts (0 disables the library).

- `lyrics_generator`: which lyric writer pens the words on vocal
  tracks: `"scribe"` (default; plans, drafts and revises against
  dictionary-checked rhyme, syllable, repetition and topic rules) or
  `"smoothbrain"` (the original quick one-shot prompt). This sets the
  default for new sessions; the `lyrics` command and the phone remote
  switch it per session at runtime. Lyric writing uses Ollama; without
  a daemon the engine's own planner invents the words.
- `default_preset`: the preset the radio starts on when you run `iar`
  with no `--preset`, `--session` or prompt. Empty starts from the
  built-in fallback sound instead. A name that no longer exists is
  logged and falls back rather than stopping the radio.
- `vocal_languages`: the languages sung vocals are sung in, written
  the way you would say them: `["English", "Russian", "French",
  "Bisaya (Cebuano)"]`. Every song picks one of them at random, so the
  same language can come up twice in a row. Empty - the default -
  leaves the choice to the music engine, which sings in whatever
  language it likes. The `languages` command and the phone remote edit
  this list, and both write it back here; the remote's switches then
  narrow it down for the playing session without changing the list.
  The music engine publishes about fifty language tags but never checks
  a request against the list, and the model behind it knows more
  languages than the list names - Cebuano among them, asked for by its
  own tag. Anything it does not know still works: the words are written
  in it and sung with no tag at all. On tracks where the engine writes
  the words itself, the tag decides which language they are written in,
  so a near-miss tag gets a different language rather than an accent.
  A language steered in by hand ("sing in
  French") pins the session to it and outranks the list; a preset that
  names a language of its own does not. Editing the list releases that
  pin, drops the tracks queued ahead and starts generating in the new
  languages.
## remote

The phone remote (page + live MP3 stream + saved-chunk player); see the
README's "Listening from your phone" section for the full flow. Every
request needs a logged-in account (`iar remote setup` creates the first
admin; the Users page does the rest).

- `enabled`: turn the remote on (the `--remote` flag does the same for
  one run).
- `port`: the HTTP port (default 8246).
- `bind`: extra addresses to listen on, as a list (a plain string also
  works and means a one-element list). Binding is additive: localhost
  and the machine's Tailscale address are always kept, and every
  address you bind is automatically accepted in URLs. `"0.0.0.0"`
  listens on every network the machine is on and auto-allows the
  machine's own interface addresses. Remember that logins travel in
  clear text over plain HTTP: fine on the tailnet (encrypted), your
  call on a home LAN, never on the public internet without TLS in
  front.
- `allowed_hosts`: extra hostnames or IPs clients may use in the URL
  (the remote rejects unknown Host and Origin values as a
  DNS-rebinding/cross-site defense). Localhost, the tailnet address and
  bound addresses are always allowed; you only need this for a
  hostname or a reverse proxy.
- `smtp`: optional. When `host` is set, invitation and password-reset
  links are also emailed to their recipients (without it, admins pass
  the links on by hand). `tls` is `"starttls"` (default, port 587),
  `"tls"` (implicit TLS, port 465) or `"none"` (port 25); `port`
  overrides the default for the mode; `username`/`password`
  authenticate with PLAIN when set; `from` is the sender address. Test
  with `iar remote test-email you@example.com`. A config file holding
  the password should be readable only by you (`chmod 600`).

## acestep

Settings in this section are read when the engine daemon starts, and the
daemon outlives radio sessions - a playing radio keeps it alive
indefinitely. After changing anything here, run `iar engine stop`: the
next thing to need the engine brings up a fresh daemon with the new
settings (playback rides out the restart from its buffered tracks).

- `port`: pins the engine API to a fixed localhost port. The default 0
  allocates a free port for each engine daemon (the port is recorded in
  the state directory and shown by `iar engine status`).
- `idle_minutes`: the shared engine daemon shuts down after this long
  with nothing using it, freeing GPU memory. Restarting the player
  within the window reuses the warm engine instantly.
- `lm_model_path`: pins the engine's internal planner language model
  (e.g. `"acestep-5Hz-lm-0.6B"` or `"acestep-5Hz-lm-1.7B"`). Empty lets
  the engine pick one that fits your GPU.
- `lm_backend`: `"auto"` (default) runs the planner LM in a
  memory-friendly way (PyTorch backend, released from GPU memory between
  generations) on GPUs under 16 GB, and leaves the engine's faster vLLM
  default on larger ones. `"vllm"` or `"pt"` force a backend. If you see
  CUDA out-of-memory errors because other applications share the GPU,
  `"pt"` is the safe choice.
- `inference_steps` (1-20): diffusion steps for the turbo model. The
  default 12 renders audibly more high-end detail than the model's
  quick-start 8; on a strong GPU the speed difference is negligible
  because planning and decoding dominate generation time. 20 adds a
  little more brightness.
- `thinking`: when true, the engine's planner LM sketches the track
  before synthesis, which improves musical coherence at some speed cost.
- `offload_dit`: when true, the music model is kept in system memory
  between tracks instead of staying resident on the graphics card. Turn
  it on when something else needs the card - a speech-to-text model, a
  game, another generator. The radio only computes for about a fifth of
  the time it runs, so this hands roughly four and a half gigabytes of
  video memory back for the rest of it. It costs a few seconds per
  track while the weights move back, and about the same amount of
  system memory to hold them; the music itself is identical. It does
  not lower the peak during generation, so it fixes the collisions that
  happen between tracks, not the ones during them.
- `max_track_seconds`: a ceiling on how long a track the engine may plan
  when it writes the words itself - which happens whenever no lyric
  sheet from the lyric writer is ready in time. The planner picks a
  natural song length, typically three to four minutes. Longer plans cost proportionally more video memory and
  generation time, so the ceiling trims the occasional runaway pick
  without shortening normal songs; picks under the ceiling pass through
  untouched. 0 removes the ceiling. Instrumental tracks follow
  `track_seconds` exactly and never consult this.
- `repo_url`, `tag`: which engine version `iar setup` installs. Change
  only if you know you want a different release.

## ollama

- `enabled`: when true and a daemon is reachable, the player asks Ollama
  to refine steering inputs into structured, validated updates to the
  sound, to enrich vague starting prompts, and to write lyrics for
  vocal tracks. All of it happens in the background and only shapes
  later tracks; the deterministic built-in path handles every input on
  its own, so when Ollama is off or unreachable everything still works.
- `url`: daemon address.
- `model`: model name to use; empty picks the first installed model.
  Pin a small, fast instruction-following model here; it is plenty for
  this job and keeps the refinements timely.
- `gpu_layers`: how much of the helper model may go onto the graphics
  card. The default 0 keeps it entirely on the CPU: the music engine
  (and anything else sharing the card) needs the video memory more than
  the helper needs speed, and a helper load grabbing leftover memory
  between generation peaks is exactly what pushes the card into
  out-of-memory. Set -1 to let the Ollama daemon place the model
  itself (sensible on a machine with video memory to spare), or a
  positive number to put that many layers on the card.

## sessions

- `auto_retention_days`: sessions that were never given a name (the
  `session-...`, `prompt-...` and `<preset>-...` ones saved
  automatically) are deleted this many days after they last played, at
  startup and every half hour while playing. `0` keeps them forever.
  Named sessions, presets and the playing session are never touched.

## Environment variables

- `IAR_DATA_DIR`: relocates all state (engine install, sessions, exports,
  logs). Defaults to `$XDG_DATA_HOME/iar` or `~/.local/share/iar`.
- `IAR_CONFIG_DIR`: relocates the config file. Defaults to
  `$XDG_CONFIG_HOME/iar` or `~/.config/iar`.
- `IAR_PIPE_TARGET`: routes the pipe player to a specific PipeWire/Pulse
  sink (used by tests to play into a null sink).
- `IAR_PLAYER_SPEED`: speed multiplier for the null and file backends
  (testing only).
- `IAR_TEE_PCM`: path of a file to append every PCM byte sent to the
  audio backend (diagnostic; useful for verifying digital output).

## Data layout

Inside the data directory:

- `engine/` - the music engine checkout, its Python environment and model
  checkpoints
- `sessions/` - one JSON file per saved session, plus `deleted-presets`
  (the list of presets hidden with `iar sessions delete`)
- `library/` - banked tracks for instant starts (size-capped)
- `snippets/<tag>/` - tracks captured with the save command, one
  directory per tag (`untagged/` when none was given)
- `remote/` - the phone remote's accounts and login sessions
  (`users.json`, `sessions.json`; owner-only)
- `exports/` - MP3 exports
- `logs/iar.log` - structured JSON log of the player
- `logs/engine-daemon.log` - the shared engine daemon's log
- `state/` - engine daemon state, locks, and phase-duration records
