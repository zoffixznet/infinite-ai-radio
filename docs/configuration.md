# Configuration reference

bgm works with no configuration at all. When you want to tune it, create a
JSON file at the path shown by `bgm doctor` (usually
`~/.config/bgm/config.json`). Only include the keys you want to change;
everything else keeps its default.

```json
{
  "engine": "acestep",
  "player": "auto",
  "track_seconds": 150,
  "crossfade_seconds": 3,
  "buffer_tracks": 2,
  "volume": 80,
  "acestep": {
    "port": 8451,
    "lm_model_path": "",
    "inference_steps": 8,
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

## acestep

- `port`: localhost port for the engine's API server.
- `lm_model_path`: pins the engine's internal planner language model
  (e.g. `"acestep-5Hz-lm-0.6B"` or `"acestep-5Hz-lm-1.7B"`). Empty lets
  the engine pick one that fits your GPU.
- `inference_steps` (1-20): diffusion steps for the turbo model. 8 is the
  sweet spot; more is slower with mild quality gains.
- `thinking`: when true, the engine's planner LM sketches the track
  before synthesis, which improves musical coherence at some speed cost.
- `repo_url`, `tag`: which engine version `bgm setup` installs. Change
  only if you know you want a different release.

## ollama

- `enabled`: when true and a daemon is reachable, bgm asks Ollama to
  rewrite the accumulated steering into a clean prompt and to write
  lyrics for vocal tracks. When false or unreachable, a deterministic
  built-in path is used instead; everything still works.
- `url`: daemon address.
- `model`: model name to use; empty picks the first installed model. A
  small, fast model is plenty for this job.

## Environment variables

- `BGM_DATA_DIR`: relocates all state (engine install, sessions, exports,
  logs). Defaults to `$XDG_DATA_HOME/bgm` or `~/.local/share/bgm`.
- `BGM_CONFIG_DIR`: relocates the config file. Defaults to
  `$XDG_CONFIG_HOME/bgm` or `~/.config/bgm`.
- `BGM_PIPE_TARGET`: routes the pipe player to a specific PipeWire/Pulse
  sink (used by tests to play into a null sink).
- `BGM_PLAYER_SPEED`: speed multiplier for the null and file backends
  (testing only).

## Data layout

Inside the data directory:

- `engine/` - the music engine checkout, its Python environment and model
  checkpoints
- `sessions/` - one JSON file per saved session
- `exports/` - MP3 exports
- `logs/bgm.log` - structured JSON log
