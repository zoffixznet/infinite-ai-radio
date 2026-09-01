# Contributing

Bug reports, ideas and patches are all welcome.

## Before you start

Infinite AI Radio is Linux-only today, and playback goes through PipeWire
or PulseAudio. Music generation wants an NVIDIA GPU; without one you can
still work on everything except the engine itself, because the test
suite never talks to a GPU.

You need Go 1.26 or newer, plus `ffmpeg` (with `libmp3lame`), `git` and a
C compiler for the engine's Python dependencies. `make deps` checks for
them.

## The loop

```sh
make build   # build ./iar
make test    # unit tests, silent, no audio devices touched
make lint    # go vet + gofmt check
make smoke   # end-to-end run of the built binary, sandboxed and silent
```

`make test`, `make smoke` and the browser test all run against sandboxed
data directories (`IAR_DATA_DIR`, `IAR_CONFIG_DIR`) and a null or file
audio backend, so they never touch your own library or make a sound.

Two heavier targets need extra tools and are skipped automatically when
those are missing:

```sh
make browser-test  # drives the phone remote in headless Firefox
                   # needs geckodriver, firefox, pactl
make screenshots   # re-shoots the README's pictures
                   # needs the above plus python3 with Pillow
```

CI runs `gofmt`, `go vet`, `go mod tidy`, the tests with and without the
race detector, the smoke test, and a cross-compile for arm64. Running
`make lint && make test && make smoke` locally covers most of it.

## Patches

- Keep the existing style. The code is plain Go with comments that
  explain *why*, and the tests are table-driven where it helps.
- Add a test for anything that could regress. The suite is fast.
- Commit messages are prose: a short subject saying what changed for the
  listener or the machine ("Tab switching never stops the music"), and a
  body explaining why when it is not obvious.
- Do not commit anything that fails to build or whose tests fail.

## Reporting a bug

For a security issue, use GitHub's private vulnerability reporting on
the repository instead of a public issue.

`iar doctor` prints your environment and the path to the structured log.
Both are far more useful than a description. If the problem involves
generation, the log carries the engine's own output too.

Please do not paste anything from your config file without removing
SMTP credentials first.
