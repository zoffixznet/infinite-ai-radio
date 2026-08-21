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
  "buffer_tracks": 2,
  "volume": 80,
  "bed_while_waiting": false,
  "pipe_latency_ms": 200,
  "normalize_loudness": true,
  "mp3_quality": 0,
  "snippets_dir": "",
  "library_max_mb": 600,
  "remote": {
    "enabled": false,
    "port": 8246,
    "token": "",
    "bind": [],
    "allowed_hosts": []
  },
  "acestep": {
    "port": 0,
    "idle_minutes": 15,
    "lm_model_path": "",
    "lm_backend": "auto",
    "inference_steps": 12,
    "thinking": true,
    "repo_url": "https://github.com/ace-step/ACE-Step-1.5",
    "tag": "v0.1.8"
  },
  "ollama": {
    "enabled": true,
    "url": "http://127.0.0.1:11434",
    "model": ""
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
- `buffer_tracks` (1-4): how many finished tracks to keep generated ahead
  of playback. Higher survives longer engine stalls, uses more memory
  (about 28 MB per 150-second track).
- `volume` (0-100): initial output volume.
- `bed_while_waiting`: when true, a quiet noise bed plays while the
  first track is prepared instead of the default silence-with-progress.
- `pipe_latency_ms` (20-2000): how much buffering the system audio
  player is asked for. Larger values ride out heavy system load at the
  cost of a slightly slower response to volume/pause.
- `normalize_loudness`: level every generated track to a consistent
  loudness (peak-safe) before playback and banking.
- `mp3_quality` (0-9): libmp3lame VBR quality for exports and snippets;
  0 is best (the default), 9 is smallest.
- `snippets_dir`: where the in-app `save` command writes captured
  tracks. Empty means `snippets/` under the data directory.
- `library_max_mb`: total size cap for the on-disk track library that
  powers instant starts (0 disables the library).

## remote

The phone remote (page + live MP3 stream); see the README's "Listening
from your phone" section for the full flow.

- `enabled`: turn the remote on (the `--remote` flag does the same for
  one run).
- `port`: the HTTP port (default 8246).
- `token`: optional shared secret; when set, every request must carry it
  (`?token=...` or an Authorization bearer header). The private tailnet
  is the default trust boundary, so this is off by default.
- `bind`: extra addresses to listen on, as a list (a plain string also
  works and means a one-element list). Binding is additive: localhost
  and the machine's Tailscale address are always kept, and every
  address you bind is automatically accepted in URLs. `"0.0.0.0"`
  listens on every network the machine is on and auto-allows the
  machine's own interface addresses. Any entry beyond localhost refuses
  to start unless `token` is also set - only expose the remote on
  networks you fully trust, and never on the public internet.
- `allowed_hosts`: extra hostnames or IPs clients may use in the URL
  (the remote rejects unknown Host and Origin values as a
  DNS-rebinding/cross-site defense). Localhost and the tailnet address
  are always allowed; you only need this together with a `bind`
  override.

## acestep

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
- `repo_url`, `tag`: which engine version `iar setup` installs. Change
  only if you know you want a different release.

## ollama

- `enabled`: when true and a daemon is reachable, the player asks Ollama to
  rewrite the accumulated steering into a clean prompt and to write
  lyrics for vocal tracks. When false or unreachable, a deterministic
  built-in path is used instead; everything still works.
- `url`: daemon address.
- `model`: model name to use; empty picks the first installed model. A
  small, fast model is plenty for this job.

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
- `sessions/` - one JSON file per saved session
- `library/` - banked tracks for instant starts (size-capped)
- `snippets/` - tracks captured with the save command (default location)
- `exports/` - MP3 exports
- `logs/iar.log` - structured JSON log of the player
- `logs/engine-daemon.log` - the shared engine daemon's log
- `state/` - engine daemon state, locks, and phase-duration records
