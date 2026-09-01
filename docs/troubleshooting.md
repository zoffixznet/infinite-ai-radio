# Troubleshooting and limitations

`iar doctor` checks your environment: system tools, GPU, engine
install, and whether the engine API is responding. The structured log
(path printed by doctor) has the full story, including the engine's
own output.

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
- **Buzz or static while a track generates**: check the underrun counter
  in the interface header and in `iar doctor`. If it stays at zero while
  you hear the noise, the audio stream itself is clean, and what you are
  hearing is electrical interference picked up after the digital output -
  heavy GPU load inducing it into an analog chain. A digital output (USB
  interface, HDMI) sidesteps it entirely.

## Known limitations

- Linux only for now, and playback expects PipeWire or PulseAudio.
- The music engine needs an NVIDIA GPU to be practical; CPU-only machines
  are limited to the noise modes.
- A steering tweak is audible only once a track matching it has been
  generated; the player switches over the moment that track is ready,
  which with a warm engine typically means tens of seconds, not the end
  of the current track.
- Vocal lyrics are short and chorus-driven; this is a background-music
  tool, not a songwriting studio.
- The phone remote has no self-service password reset: an admin hands
  out reset links, and a locked-out sole admin recovers with
  `iar remote setup` in the terminal.
- Car integration works through the browser's media session, which
  offers only the standard buttons: the save mapping borrows the
  previous-track button and cannot change its icon, and how long a
  paused stream stays resumable after the car turns off depends on the
  phone's battery management, so an overnight stop may need one tap. A
  labelled in-car save button would need a native companion app.
