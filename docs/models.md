# Models

Infinite AI Radio never bundles model weights. `iar setup` downloads them from their
publishers into the player's data directory, with visible progress, and the
default path requires no account, token, or license click-through.

## ACE-Step 1.5 (default engine)

- Code: [github.com/ace-step/ACE-Step-1.5](https://github.com/ace-step/ACE-Step-1.5),
  MIT license, pinned to a release tag by `iar setup`.
- Weights: [huggingface.co/ACE-Step/Ace-Step1.5](https://huggingface.co/ACE-Step/Ace-Step1.5),
  MIT license, ungated, about 10 GB. The bundle contains the turbo
  diffusion model, a 1.7B planner language model, a Qwen3 text embedder
  and the audio VAE.
- The smaller 0.6B planner LM from
  [huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B](https://huggingface.co/ACE-Step/acestep-5Hz-lm-0.6B)
  (also MIT, ungated, about 1.2 GB). `iar setup` fetches it alongside the
  main bundle whatever card you have, so the first play never stalls on a
  download; the engine uses it automatically on cards with less memory.
  Pinning `acestep.lm_model_path` to `acestep-5Hz-lm-1.7B` skips it.
- Graphics memory: the turbo model plus the 0.6B planner fit comfortably
  in 8 GB. On cards under 16 GB the planner runs on the memory-friendly
  PyTorch backend instead of vLLM (see `acestep.lm_backend` in
  [configuration.md](configuration.md)).
- The model card states that music generated with ACE-Step 1.5 may be
  used commercially (the training data is licensed, royalty-free, or
  synthetic). Verify current terms on the model card if this matters for
  your use.
- Output: 48 kHz stereo, full-song generations with optional sung vocals;
  instrumental tracks are requested with the engine's `[Instrumental]`
  lyrics convention.

The engine runs as a local API server, listening on localhost only,
managed by a background daemon the player starts, supervises and restarts
as needed (`iar engine status` / `iar engine stop`). Under phased
generation - the default - the daemon is started for each generation
cycle and shut down again afterwards, so between cycles it holds no
graphics memory and no system memory at all. With `buffer.phased` turned
off it instead stays warm between runs and shuts down after a few idle
minutes (`acestep.idle_minutes`).

## Ollama models (optional)

If you run [Ollama](https://ollama.com), the player can use whatever model you
have installed to polish prompts and write lyrics. It does not download
Ollama models; whichever model you point it at keeps its own license
terms. Without Ollama, it uses a built-in deterministic path and the
engine's own planner for lyrics, so this integration is a bonus, not a
requirement.

## Embedded word data (built in)

The lyric writer embeds two small datasets in the binary, used to check
syllable counts, rhymes and vocabulary offline:

- The [CMU Pronouncing Dictionary](https://github.com/cmusphinx/cmudict)
  (Carnegie Mellon University, BSD-style license; the notice ships as
  `LICENSE.third-party` in binary releases, and lives at
  `internal/prosody/data/LICENSE` in the source tree).
- An English word-frequency list from Peter Norvig's
  [Natural Language Corpus Data](https://norvig.com/ngrams/), derived
  from the Google Web Trillion Word Corpus.

## Noise synthesis (built in)

White, pink and brown noise are synthesized directly by the player in pure Go
(no model, no GPU): pink via Paul Kellet's filter, brown via a leaky
integrator. Noise serves as the instant-start bed, the last-resort
fallback when the engine is unavailable, and a first-class mode for
sleep/masking via the `pink-noise` preset or steering ("generate brown
noise").
